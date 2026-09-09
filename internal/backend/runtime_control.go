package backend

import (
	"context"
	"encoding/json"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
)

func (b *Backend) RuntimeControlState(ctx context.Context, workspaceID string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return b.withRuntimeControl(ctx, workspaceID, false, func(ctx context.Context, store *config.ConfigStore) (config.RuntimeControlState, error) {
		return store.RuntimeControlState(ctx, scope, target)
	})
}

func (b *Backend) SetRuntimeControl(ctx context.Context, workspaceID string, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage) (config.RuntimeControlState, error) {
	return b.withRuntimeControl(ctx, workspaceID, true, func(ctx context.Context, store *config.ConfigStore) (config.RuntimeControlState, error) {
		return store.SetRuntimeControl(ctx, scope, target, value)
	})
}

func (b *Backend) RemoveRuntimeControl(ctx context.Context, workspaceID string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return b.withRuntimeControl(ctx, workspaceID, true, func(ctx context.Context, store *config.ConfigStore) (config.RuntimeControlState, error) {
		return store.RemoveRuntimeControl(ctx, scope, target)
	})
}

func (b *Backend) withRuntimeControl(ctx context.Context, workspaceID string, mutate bool, operation func(context.Context, *config.ConfigStore) (config.RuntimeControlState, error)) (config.RuntimeControlState, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return config.RuntimeControlState{}, ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()
	defer ws.runWG.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ws.ctx, cancel)
	defer stop()
	if ws.ctx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	// Receiver-owned disk resolution is invalid for detached client runtimes,
	// including read-only scoped state requests.
	if ws.Cfg.RemoteAuthority() != nil {
		return config.RuntimeControlState{}, config.ErrClientRuntimeManaged
	}
	state, err := operation(ctx, ws.Cfg)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	if mutate {
		ws.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: ws.ID}})
	}
	return state, nil
}
