package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// ProviderAuthenticationSavedState is owner-local. None of these methods routes
// to receiver account/configuration APIs in a client-owned workspace.
type ProviderAuthenticationSavedState interface {
	CanReconcileProviderAuthentication() bool
	SavedProviderAuthentication(context.Context) (providerauth.Snapshot, error)
	ReloadProviderAuthentication(context.Context, providerauth.ReloadRequest) (providerauth.ReloadOutcome, error)
}

func (w *ClientWorkspace) lockSavedAuthentication(ctx context.Context, id string) (*clientAuthority, error) {
	if !w.CanReconcileProviderAuthentication() {
		return nil, errAuthenticationReconciliationUnsupported
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return nil, err
	}
	a := w.authority
	w.mu.Unlock()
	if a == nil || a.store == nil {
		return nil, providerauth.ErrStale
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return nil, err
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	valid := w.authority == a && w.ws.ID == id && w.ws.Authority != nil && w.ws.Authority.Mode == "client" && w.ws.Authority.Principal == a.principal
	w.mu.Unlock()
	if !valid {
		a.mu.Unlock()
		return nil, providerauth.ErrStale
	}
	if err := a.loadAuthenticationJournal(ctx, id); err != nil {
		a.mu.Unlock()
		return nil, err
	}
	// Do not reconcile pending original publications. Fresh intent preserves
	// those historical receipts, including unknown results.
	if a.providerAuth == nil || a.providerAuthWorkspaceID != id {
		if w.subCtx == nil {
			a.mu.Unlock()
			return nil, errors.New("authentication lifetime unavailable")
		}
		if a.providerAuth != nil {
			a.providerAuth.Close()
		}
		a.providerAuth = providerauth.NewWithContext(w.subCtx, a.store, id)
		a.providerAuthWorkspaceID = id
	}
	return a, nil
}
func (w *ClientWorkspace) SavedProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	id := w.workspaceID()
	a, err := w.lockSavedAuthentication(ctx, id)
	if err != nil {
		return providerauth.Snapshot{}, err
	}
	defer a.mu.Unlock()
	return a.providerAuth.Status(ctx)
}
func (w *ClientWorkspace) ReloadProviderAuthentication(ctx context.Context, request providerauth.ReloadRequest) (providerauth.ReloadOutcome, error) {
	initial := providerauth.ReloadOutcome{ReloadID: request.ReloadID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	a, err := w.lockSavedAuthentication(ctx, request.Target.WorkspaceID)
	if err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	return a.providerAuth.ReloadSavedAuthentication(ctx, request)
}
