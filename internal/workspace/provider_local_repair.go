package workspace

import (
	"context"
	"errors"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
)

func (w *AppWorkspace) RepairLocalAuthentication(ctx context.Context, request providerauth.LocalRepairRequest) (config.LocalAuthenticationRepairResult, error) {
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if w.providerAuth == nil {
		return config.LocalAuthenticationRepairResult{}, errors.New("local authentication service is unavailable")
	}
	return w.providerAuth.RepairLocalAuthentication(ctx, request)
}
func (w *ClientWorkspace) RepairLocalAuthentication(ctx context.Context, request providerauth.LocalRepairRequest) (config.LocalAuthenticationRepairResult, error) {
	if err := request.Validate(); err != nil {
		return config.LocalAuthenticationRepairResult{}, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	id := w.workspaceID()
	if id != request.WorkspaceID {
		return config.LocalAuthenticationRepairResult{}, errors.New("local repair workspace changed")
	}
	if w.clientOwned() {
		a := w.authority
		if a == nil {
			return config.LocalAuthenticationRepairResult{}, errors.New("owning client authority is unavailable")
		}
		if err := lockProviderAuthAuthority(ctx, a); err != nil {
			return config.LocalAuthenticationRepairResult{}, err
		}
		defer a.mu.Unlock()
		// Historical disk repair neither reconciles an uncertain PUT nor adopts new
		// authority. Existing retained control identity must still match this owner.
		if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
			return config.LocalAuthenticationRepairResult{}, err
		}
		if a.providerAuth == nil || a.providerAuthWorkspaceID != id {
			if a.providerAuth != nil {
				a.providerAuth.Close()
			}
			a.providerAuth = providerauth.NewWithContext(w.subCtx, a.store, id)
			a.providerAuthWorkspaceID = id
		}
		result, err := a.providerAuth.RepairLocalAuthentication(ctx, request)
		if verify := w.verifyClientProviderAuthAuthority(id, a); verify != nil {
			return result, errors.Join(err, verify)
		}
		return result, err
	}
	if w.client == nil {
		return config.LocalAuthenticationRepairResult{}, errors.New("authentication client is unavailable")
	}
	response, err := w.client.RepairLocalAuthentication(ctx, id, request)
	if w.workspaceID() != id {
		return response.Result, errors.Join(err, errors.New("local repair workspace changed after response"))
	}
	return response.Result, err
}

func (w *AppWorkspace) AuthenticationWorkspaceID() string    { return w.providerAuth.WorkspaceID() }
func (w *ClientWorkspace) AuthenticationWorkspaceID() string { return w.workspaceID() }
