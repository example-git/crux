package workspace

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
)

func (w *AppWorkspace) SetProviderToolingInstructions(scope config.Scope, owner providerregistry.RegistrationOwner, profile string) error {
	return w.store.SetProviderToolingInstructions(scope, owner, profile)
}

func (w *AppWorkspace) RemoveProviderToolingInstructions(scope config.Scope, owner providerregistry.RegistrationOwner) error {
	return w.store.RemoveProviderToolingInstructions(scope, owner)
}

func (w *AppWorkspace) ReloadProviderContextInstructions(ctx context.Context, owner providerregistry.RegistrationOwner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.store.ValidateActiveProviderOwner(owner); err != nil {
		return err
	}
	return ctx.Err()
}

func (w *ClientWorkspace) SetProviderToolingInstructions(scope config.Scope, owner providerregistry.RegistrationOwner, profile string) error {
	return w.mutateProviderTooling(w.subCtx, scope, owner, profile, false)
}

func (w *ClientWorkspace) RemoveProviderToolingInstructions(scope config.Scope, owner providerregistry.RegistrationOwner) error {
	return w.mutateProviderTooling(w.subCtx, scope, owner, "", true)
}

func (w *ClientWorkspace) mutateProviderTooling(ctx context.Context, scope config.Scope, owner providerregistry.RegistrationOwner, profile string, remove bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !w.clientOwned() {
		workspaceID := w.workspaceID()
		var state proto.ProviderToolingState
		var err error
		if remove {
			state, err = w.client.RemoveProviderToolingInstructions(ctx, workspaceID, scope, owner)
		} else {
			state, err = w.client.SetProviderToolingInstructions(ctx, workspaceID, scope, owner, profile)
		}
		if err != nil {
			return err
		}
		current, err := w.client.GetWorkspace(ctx, workspaceID)
		if err != nil {
			return fmt.Errorf("provider tooling applied; cannot refresh workspace configuration: %w", err)
		}
		if err := state.ValidateConfig(current.Config); err != nil {
			return err
		}
		if current.ID != workspaceID || w.workspaceID() != workspaceID {
			return fmt.Errorf("workspace changed before provider tooling acknowledgement")
		}
		w.adoptRuntimeResponse(*current)
		if w.workspaceID() != workspaceID {
			return fmt.Errorf("workspace changed before provider tooling acknowledgement")
		}
		return state.ValidateConfig(w.Config())
	}
	a := w.authority
	if a == nil {
		return fmt.Errorf("owning client provider tooling configuration is unavailable; reconnect with a collected runtime")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return err
	}
	if err := a.requireAuthenticationPublication(w.workspaceID()); err != nil {
		return err
	}
	var err error
	if remove {
		err = a.store.RemoveProviderToolingInstructionsContext(ctx, scope, owner)
	} else {
		err = a.store.SetProviderToolingInstructionsContext(ctx, scope, owner, profile)
	}
	if err != nil {
		return err
	}
	cfg := a.store.Config()
	if remove {
		provider, _ := cfg.Providers.Get(owner.ProviderID)
		profile = provider.ToolingInstructions
	}
	state := proto.ProviderToolingState{Scope: scope, Owner: owner, Profile: profile}
	if err := state.ValidateConfig(cfg); err != nil {
		return err
	}
	if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
		return err
	}
	if err := validateAcceptedProviderTooling(a.accepted, state); err != nil {
		return err
	}
	return state.ValidateConfig(a.configView())
}

func validateAcceptedProviderTooling(accepted config.RemoteRuntimeProposal, state proto.ProviderToolingState) error {
	for _, definition := range accepted.Providers {
		if definition.Config.ID != state.Owner.ProviderID {
			continue
		}
		if definition.Config.Disable || definition.Config.ToolingInstructions != state.Profile {
			return fmt.Errorf("accepted runtime provider tooling profile differs from the requested selection")
		}
		for _, credential := range accepted.Credentials {
			if credential.Owner.ProviderID == state.Owner.ProviderID {
				if credential.Owner != state.Owner {
					return fmt.Errorf("accepted runtime provider tooling owner changed")
				}
				return nil
			}
		}
		return fmt.Errorf("accepted runtime provider tooling owner is missing")
	}
	for _, model := range accepted.Models {
		if model.Provider == state.Owner.ProviderID {
			return fmt.Errorf("accepted runtime provider tooling definition is missing")
		}
	}
	// Preferences for unselected providers stay local until a model uses them.
	return nil
}
