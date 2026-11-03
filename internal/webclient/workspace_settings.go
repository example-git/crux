package webclient

import (
	"context"
	"encoding/json"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
)

type workspaceSettings struct {
	resolve func(string) (workspace.Workspace, error)
}

var _ server.WorkspaceSettings = workspaceSettings{}

func (s workspaceSettings) get(id string) (workspace.Workspace, error) {
	if s.resolve == nil {
		return nil, backend.ErrWorkspaceNotFound
	}
	selected, err := s.resolve(id)
	if err != nil {
		return nil, err
	}
	if selected == nil {
		return nil, backend.ErrWorkspaceNotFound
	}
	return selected, nil
}

func (s workspaceSettings) RuntimeControlState(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	selected, err := s.get(id)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	return selected.RuntimeControlState(ctx, scope, target)
}

func (s workspaceSettings) SetRuntimeControl(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage) (config.RuntimeControlState, error) {
	selected, err := s.get(id)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	return selected.SetRuntimeControl(ctx, scope, target, value)
}

func (s workspaceSettings) RemoveRuntimeControl(ctx context.Context, id string, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	selected, err := s.get(id)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	return selected.RemoveRuntimeControl(ctx, scope, target)
}

func (s workspaceSettings) SetProviderToolingInstructions(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string) (proto.ProviderToolingState, error) {
	return s.providerTooling(ctx, id, scope, owner, profile, false)
}

func (s workspaceSettings) RemoveProviderToolingInstructions(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner) (proto.ProviderToolingState, error) {
	return s.providerTooling(ctx, id, scope, owner, "", true)
}

func (s workspaceSettings) providerTooling(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner, profile string, remove bool) (proto.ProviderToolingState, error) {
	if err := ctx.Err(); err != nil {
		return proto.ProviderToolingState{}, err
	}
	selected, err := s.get(id)
	if err != nil {
		return proto.ProviderToolingState{}, err
	}
	if remove {
		err = selected.RemoveProviderToolingInstructions(scope, owner)
	} else {
		err = selected.SetProviderToolingInstructions(scope, owner, profile)
	}
	if err != nil {
		return proto.ProviderToolingState{}, err
	}
	cfg := selected.Config()
	if remove && cfg != nil && cfg.Providers != nil {
		provider, _ := cfg.Providers.Get(owner.ProviderID)
		profile = provider.ToolingInstructions
	}
	state := proto.ProviderToolingState{Scope: scope, Owner: owner, Profile: profile}
	if err := state.ValidateConfig(cfg); err != nil {
		return proto.ProviderToolingState{}, err
	}
	return state, nil
}

func (s workspaceSettings) SetProviderDisabled(id string, scope config.Scope, owner providerregistry.RegistrationOwner, disabled bool) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.SetProviderDisabled(scope, owner, disabled)
}

func (s workspaceSettings) SetConfigField(id string, scope config.Scope, key string, value any) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.SetConfigField(scope, key, value)
}

func (s workspaceSettings) RemoveConfigField(id string, scope config.Scope, key string) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.RemoveConfigField(scope, key)
}

func (s workspaceSettings) UpdatePreferredModel(id string, scope config.Scope, kind config.SelectedModelType, model config.SelectedModel, owner providerregistry.RegistrationOwner) (config.AgentModelState, error) {
	selected, err := s.get(id)
	if err != nil {
		return config.AgentModelState{}, err
	}
	return selected.UpdatePreferredModel(scope, kind, model, owner)
}

func (s workspaceSettings) SetCompactMode(id string, scope config.Scope, enabled bool) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.SetCompactMode(scope, enabled)
}

func (s workspaceSettings) RemoveProviderCredentials(id string, scope config.Scope, owner providerregistry.RegistrationOwner) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.RemoveProviderCredentials(scope, owner)
}

func (s workspaceSettings) SetProviderAPIKey(id string, scope config.Scope, providerID string, key any) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.SetProviderAPIKey(scope, providerID, key)
}

func (s workspaceSettings) ImportCopilot(ctx context.Context, id string, owner providerregistry.RegistrationOwner) (bool, error) {
	selected, err := s.get(id)
	if err != nil {
		return false, err
	}
	return selected.ImportCopilot(ctx, owner)
}

func (s workspaceSettings) RefreshOAuthToken(ctx context.Context, id string, scope config.Scope, owner providerregistry.RegistrationOwner) error {
	selected, err := s.get(id)
	if err != nil {
		return err
	}
	return selected.RefreshOAuthToken(ctx, scope, owner)
}
