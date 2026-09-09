package backend

import (
	"context"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
)

func (b *Backend) SetProviderToolingInstructions(ctx context.Context, workspaceID string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string) (proto.ProviderToolingState, error) {
	return b.mutateProviderTooling(ctx, workspaceID, scope, owner, profile, false)
}

func (b *Backend) RemoveProviderToolingInstructions(ctx context.Context, workspaceID string, scope config.Scope, owner providerregistry.RegistrationOwner) (proto.ProviderToolingState, error) {
	return b.mutateProviderTooling(ctx, workspaceID, scope, owner, "", true)
}

func (b *Backend) mutateProviderTooling(ctx context.Context, workspaceID string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string, remove bool) (proto.ProviderToolingState, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return proto.ProviderToolingState{}, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return proto.ProviderToolingState{}, ErrWorkspaceClosing
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
		return proto.ProviderToolingState{}, err
	}
	if remove {
		err = ws.Cfg.RemoveProviderToolingInstructionsContext(ctx, scope, owner)
	} else {
		err = ws.Cfg.SetProviderToolingInstructionsContext(ctx, scope, owner, profile)
	}
	if err != nil {
		return proto.ProviderToolingState{}, err
	}
	cfg := ws.Cfg.Config()
	if remove {
		provider, _ := cfg.Providers.Get(owner.ProviderID)
		profile = provider.ToolingInstructions
	}
	state := proto.ProviderToolingState{Scope: scope, Owner: owner, Profile: profile}
	if err := state.ValidateConfig(cfg); err != nil {
		return proto.ProviderToolingState{}, err
	}
	// Tooling instructions do not change process-wide MCP configuration.
	ws.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: ws.ID}})
	return state, nil
}
