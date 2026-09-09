package workspace

import (
	"context"
	"fmt"
	"time"

	"github.com/example-git/crux/internal/providerauth"
)

func (w *AppWorkspace) ProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.Snapshot{}, err
	}
	if w.providerAuth == nil {
		return providerauth.Snapshot{}, fmt.Errorf("local provider authentication service is unavailable")
	}
	return w.providerAuth.Status(ctx)
}

func (w *AppWorkspace) ProviderAccounts(ctx context.Context, target providerauth.Target) (providerauth.AccountsState, error) {
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if w.providerAuth == nil {
		return providerauth.AccountsState{}, fmt.Errorf("local provider authentication service is unavailable")
	}
	return w.providerAuth.Accounts(ctx, target)
}

func (w *ClientWorkspace) ProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.Snapshot{}, err
	}
	id := w.workspaceID()
	if id == "" {
		return providerauth.Snapshot{}, fmt.Errorf("provider authentication workspace is unavailable")
	}
	if w.clientOwned() {
		a := w.authority
		if a == nil {
			return providerauth.Snapshot{}, fmt.Errorf("owning client authentication authority is unavailable; reconnect with a collected runtime")
		}
		if err := lockProviderAuthAuthority(ctx, a); err != nil {
			return providerauth.Snapshot{}, err
		}
		defer a.mu.Unlock()
		if err := w.prepareClientProviderAuth(ctx, id); err != nil {
			return providerauth.Snapshot{}, err
		}
		state, err := a.providerAuth.StatusForAccepted(ctx, a.accepted, a.configView())
		if err != nil {
			return providerauth.Snapshot{}, err
		}
		if err := state.Validate(); err != nil {
			return providerauth.Snapshot{}, err
		}
		if state.WorkspaceID != id {
			return providerauth.Snapshot{}, fmt.Errorf("provider authentication workspace changed")
		}
		if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
			return providerauth.Snapshot{}, err
		}
		return state, ctx.Err()
	}
	if w.client == nil {
		return providerauth.Snapshot{}, fmt.Errorf("provider authentication client is unavailable")
	}
	sequence := w.providerAuthReadSequence.Add(1)
	state, err := w.client.ProviderAuthentication(ctx, id)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	if err := ctx.Err(); err != nil {
		return providerauth.Snapshot{}, err
	}
	if err := w.acceptProviderAuthRead(id, sequence, state.Generation); err != nil {
		return providerauth.Snapshot{}, err
	}
	return state, ctx.Err()
}

func (w *ClientWorkspace) ProviderAccounts(ctx context.Context, target providerauth.Target) (providerauth.AccountsState, error) {
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if err := target.Validate(); err != nil {
		return providerauth.AccountsState{}, err
	}
	id := w.workspaceID()
	if target.WorkspaceID != id {
		return providerauth.AccountsState{}, fmt.Errorf("provider account target does not match current workspace")
	}
	if w.clientOwned() {
		a := w.authority
		if a == nil {
			return providerauth.AccountsState{}, fmt.Errorf("owning client authentication authority is unavailable; reconnect with a collected runtime")
		}
		if err := lockProviderAuthAuthority(ctx, a); err != nil {
			return providerauth.AccountsState{}, err
		}
		defer a.mu.Unlock()
		if err := w.prepareClientProviderAuth(ctx, id); err != nil {
			return providerauth.AccountsState{}, err
		}
		state, err := a.providerAuth.AccountsForAccepted(ctx, target, a.accepted, a.configView())
		if err != nil {
			return providerauth.AccountsState{}, err
		}
		if err := state.Validate(); err != nil {
			return providerauth.AccountsState{}, err
		}
		if state.Target != target {
			return providerauth.AccountsState{}, fmt.Errorf("provider account target changed")
		}
		if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
			return providerauth.AccountsState{}, err
		}
		return state, ctx.Err()
	}
	if w.client == nil {
		return providerauth.AccountsState{}, fmt.Errorf("provider authentication client is unavailable")
	}
	sequence := w.providerAuthReadSequence.Add(1)
	state, err := w.client.ProviderAccounts(ctx, id, target)
	if err != nil {
		return providerauth.AccountsState{}, err
	}
	if err := ctx.Err(); err != nil {
		return providerauth.AccountsState{}, err
	}
	if err := w.acceptProviderAuthRead(id, sequence, state.Target.Generation); err != nil {
		return providerauth.AccountsState{}, err
	}
	return state, ctx.Err()
}

// Called under the retained authority mutex. This only reconciles an existing
// ambiguous acknowledgement by GET; it never collects or publishes a runtime.
func (w *ClientWorkspace) prepareClientProviderAuth(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.workspaceID() != id || !w.clientOwned() {
		return fmt.Errorf("provider authentication workspace changed")
	}
	a := w.authority
	if a.recoveryProposal != nil {
		return fmt.Errorf("workspace recreation is awaiting acknowledgement; retry the same recovery before changing authentication")
	}
	if a.pending != nil {
		if w.client == nil {
			return fmt.Errorf("pending client authentication state cannot be reconciled")
		}
		if err := w.reconcileClientAuthority(ctx, a); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
		return err
	}
	if a.providerAuth == nil || a.providerAuthWorkspaceID != id {
		if w.subCtx == nil {
			return fmt.Errorf("provider authentication workspace lifetime is unavailable")
		}
		if a.providerAuth != nil {
			a.providerAuth.Close()
		}
		a.providerAuth, a.providerAuthWorkspaceID = providerauth.NewWithContext(w.subCtx, a.store, id), id
	}
	return nil
}

func (w *ClientWorkspace) verifyClientProviderAuthAuthority(id string, authority *clientAuthority) error {
	current := w.cached()
	if current.ID != id || !matchesAuthority(current.Authority, authority.principal, authority.accepted) {
		return fmt.Errorf("accepted client authentication authority no longer matches the workspace; reconnect or reconcile the client runtime")
	}
	return nil
}

// Keep freshness tracking separate from the cached config: these read APIs do
// not acknowledge or adopt an unrelated workspace/configuration response.
func (w *ClientWorkspace) acceptProviderAuthRead(id string, sequence uint64, generation providerauth.Generation) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.ws.ID != id || (w.ws.Authority != nil && w.ws.Authority.Mode == "client") {
		return fmt.Errorf("provider authentication workspace changed")
	}
	if w.providerAuthWorkspaceID == id {
		if sequence < w.providerAuthAppliedRead {
			return fmt.Errorf("provider authentication read was superseded")
		}
		if generation.Epoch == w.providerAuthGeneration.Epoch && generation.Sequence < w.providerAuthGeneration.Sequence {
			return fmt.Errorf("provider authentication generation moved backwards")
		}
	}
	w.providerAuthWorkspaceID, w.providerAuthAppliedRead, w.providerAuthGeneration = id, sequence, generation
	return nil
}

func providerAuthContext(ctx, lifetime context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	if lifetime == nil {
		return ctx, cancel
	}
	stop := context.AfterFunc(lifetime, cancel)
	if lifetime.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}

func lockProviderAuthAuthority(ctx context.Context, authority *clientAuthority) error {
	for !authority.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		authority.mu.Unlock()
		return err
	}
	return nil
}
