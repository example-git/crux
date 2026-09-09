package backend

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) ProviderAuthentication(ctx context.Context, workspaceID string) (providerauth.Snapshot, error) {
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, workspaceID)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	defer done()
	state, err := ws.providerAuth.Status(ctx)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	if err := state.Validate(); err != nil {
		return providerauth.Snapshot{}, err
	}
	if state.WorkspaceID != workspaceID {
		return providerauth.Snapshot{}, fmt.Errorf("provider authentication workspace changed")
	}
	return state, ctx.Err()
}

func (b *Backend) ProviderAccounts(ctx context.Context, workspaceID string, target providerauth.Target) (providerauth.AccountsState, error) {
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, workspaceID)
	if err != nil {
		return providerauth.AccountsState{}, err
	}
	defer done()
	if err := target.Validate(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if target.WorkspaceID != workspaceID {
		return providerauth.AccountsState{}, fmt.Errorf("provider account target does not match workspace")
	}
	state, err := ws.providerAuth.Accounts(ctx, target)
	if err != nil {
		return providerauth.AccountsState{}, err
	}
	if err := state.Validate(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if state.Target != target {
		return providerauth.AccountsState{}, fmt.Errorf("provider account target changed")
	}
	return state, ctx.Err()
}

func (b *Backend) beginProviderAuthRead(ctx context.Context, workspaceID string) (*Workspace, context.Context, func(), error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, ctx, nil, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return nil, ctx, nil, ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ws.ctx, cancel)
	done := func() { stop(); cancel(); ws.runWG.Done() }
	if ws.ctx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		done()
		return nil, ctx, nil, err
	}
	if ws.Cfg == nil {
		done()
		return nil, ctx, nil, fmt.Errorf("provider authentication configuration is unavailable")
	}
	// A detached receiver must never read the receiver's account store for
	// a runtime whose credential authority belongs to its connected client.
	if ws.Cfg.RemoteAuthority() != nil {
		done()
		return nil, ctx, nil, config.ErrClientRuntimeManaged
	}
	ws.providerAuthOnce.Do(func() { ws.providerAuth = providerauth.New(ws.Cfg, workspaceID) })
	return ws, ctx, done, nil
}
