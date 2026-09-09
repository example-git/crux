package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerregistry"
)

func (w *AppWorkspace) ImportCopilot(ctx context.Context, owner providerregistry.RegistrationOwner) (bool, error) {
	_, found, err := w.store.ImportCopilotForOwner(ctx, owner)
	return found, err
}

func (w *ClientWorkspace) ImportCopilot(ctx context.Context, owner providerregistry.RegistrationOwner) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.subCtx, cancel)
	defer stop()
	if w.subCtx.Err() != nil {
		cancel()
	}
	if !w.clientOwned() {
		found, err := w.client.ImportCopilot(ctx, w.workspaceID(), owner)
		if err == nil && found {
			w.refreshWorkspace()
		}
		return found, err
	}
	a := w.authority
	if a == nil {
		return false, errors.New("owning client authority is unavailable for Copilot import")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return false, err
	}
	_, found, err := a.store.ImportCopilotForOwner(ctx, owner)
	if err != nil || !found {
		return false, err
	}
	delete(a.removed, owner)
	if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
		return false, err
	}
	return true, nil
}
