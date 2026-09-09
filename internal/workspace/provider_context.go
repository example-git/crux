package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

// ReloadProviderContextInstructions publishes the owning client's edited
// instruction file before an agent rebuild. The editor owns the file change;
// this operation never writes either host's configuration or instruction files.
func (w *ClientWorkspace) ReloadProviderContextInstructions(ctx context.Context, owner providerregistry.RegistrationOwner) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.subCtx, cancel)
	defer stop()
	if w.subCtx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.clientOwned() {
		a := w.authority
		if a == nil {
			return errors.New("owning client instruction configuration is unavailable; reconnect with a collected runtime")
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		if err := w.reconcileClientAuthority(ctx, a); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.store.ValidateActiveProviderOwner(owner); err != nil {
			return err
		}
		if !providerContextSelected(a.store.Config().Models, owner.ProviderID) {
			return errors.New("provider context reload requires a selected model provider")
		}
		if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
			return err
		}
		if err := validateAcceptedProviderContext(a.accepted, owner); err != nil {
			return err
		}
		cfg := a.configView()
		if !providerContextSelected(cfg.Models, owner.ProviderID) {
			return errors.New("provider selection changed before instruction reload acknowledgement")
		}
		return validateProviderContextOwner(cfg, config.ProviderSurfaces(cfg), owner)
	}
	workspaceID := w.workspaceID()
	current, err := w.client.GetWorkspace(ctx, workspaceID)
	if err != nil {
		return err
	}
	if err := validateProviderContextOwner(current.Config, current.ProviderSurfaces, owner); err != nil {
		return err
	}
	if current.ID != workspaceID || w.workspaceID() != workspaceID {
		return errors.New("workspace changed before instruction reload acknowledgement")
	}
	w.adoptRuntimeResponse(*current)
	if w.workspaceID() != workspaceID {
		return errors.New("workspace changed before instruction reload acknowledgement")
	}
	return validateProviderContextOwner(w.Config(), w.ProviderSurfaces(), owner)
}

func validateProviderContextOwner(cfg *config.Config, surfaces []providerregistry.Surface, owner providerregistry.RegistrationOwner) error {
	if cfg == nil || cfg.Providers == nil {
		return errors.New("provider configuration is unavailable for instruction reload")
	}
	active, ok := cfg.ProviderOwner(owner.ProviderID)
	provider, exists := cfg.Providers.Get(owner.ProviderID)
	surface, surfaced := providerregistry.LookupSurface(surfaces, owner.ProviderID)
	if !ok || active != owner || !exists || provider.Disable || !surfaced || !surface.Available || surface.Owner == nil || *surface.Owner != owner {
		return errors.New("provider owner changed before instruction reload")
	}
	return nil
}

func providerContextSelected(models map[config.SelectedModelType]config.SelectedModel, providerID string) bool {
	for _, kind := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		if models[kind].Provider == providerID {
			return true
		}
	}
	return false
}

func validateAcceptedProviderContext(accepted config.RemoteRuntimeProposal, owner providerregistry.RegistrationOwner) error {
	if !providerContextSelected(accepted.Models, owner.ProviderID) {
		return errors.New("accepted runtime has no selected model for provider context reload")
	}
	if _, exists := accepted.ProviderContextInstructions[owner.ProviderID]; !exists {
		return errors.New("accepted runtime is missing provider context instructions")
	}
	for _, credential := range accepted.Credentials {
		if credential.Owner.ProviderID == owner.ProviderID {
			if credential.Owner != owner {
				return errors.New("accepted runtime provider context owner changed")
			}
			return nil
		}
	}
	return errors.New("accepted runtime provider context owner is missing")
}
