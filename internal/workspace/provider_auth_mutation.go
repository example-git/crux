package workspace

import (
	"context"
	"fmt"
	"reflect"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
)

func (w *AppWorkspace) SwitchProviderAccount(ctx context.Context, request providerauth.SwitchRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.providerAuth == nil {
		return initial, fmt.Errorf("local provider authentication service is unavailable")
	}
	result, err := w.providerAuth.Switch(ctx, request)
	return result.Outcome, err
}

func (w *AppWorkspace) LogoutProvider(ctx context.Context, request providerauth.LogoutRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.providerAuth == nil {
		return initial, fmt.Errorf("local provider authentication service is unavailable")
	}
	result, err := w.providerAuth.Logout(ctx, request)
	return result.Outcome, err
}

func (w *ClientWorkspace) SwitchProviderAccount(ctx context.Context, request providerauth.SwitchRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.clientOwned() {
		return w.switchClientAuthentication(ctx, request)
	}
	return w.mutateServerAuthentication(ctx, initial, func(id string) (proto.ProviderAuthenticationMutationResponse, error) {
		return w.client.SwitchProviderAccount(ctx, id, request)
	})
}

func (w *ClientWorkspace) LogoutProvider(ctx context.Context, request providerauth.LogoutRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.clientOwned() {
		return w.logoutClientAuthentication(ctx, request)
	}
	return w.mutateServerAuthentication(ctx, initial, func(id string) (proto.ProviderAuthenticationMutationResponse, error) {
		return w.client.LogoutProvider(ctx, id, request)
	})
}

// The SDK validates the exact request and coherent response. This adapter
// additionally fences cache adoption against local workspace/read changes;
// neither a generic GET nor a later public snapshot can replace this receipt.
func (w *ClientWorkspace) mutateServerAuthentication(ctx context.Context, initial providerauth.MutationOutcome, mutate func(string) (proto.ProviderAuthenticationMutationResponse, error)) (providerauth.MutationOutcome, error) {
	before := w.cached()
	if before.ID == "" || before.ID != initial.Previous.WorkspaceID || (before.Authority != nil && before.Authority.Mode != "server") {
		return initial, fmt.Errorf("provider authentication target does not match the server-owned workspace")
	}
	if w.client == nil {
		return initial, fmt.Errorf("provider authentication client is unavailable")
	}
	refreshSequence := w.refreshSequence.Add(1)
	readSequence := w.providerAuthReadSequence.Add(1)
	response, err := mutate(before.ID)
	if err != nil {
		if response.Outcome.OperationID == "" {
			return initial, err // No valid receipt: these false flags do not prove rollback.
		}
		return response.Outcome, err
	}
	if response.Outcome.Superseded {
		return response.Outcome, nil
	}
	if err := w.adoptServerAuthentication(ctx, before, refreshSequence, readSequence, response); err != nil {
		return response.Outcome, fmt.Errorf("%w: %w", providerauth.ErrReceiptUnverified, err)
	}
	return response.Outcome, nil
}

func (w *ClientWorkspace) adoptServerAuthentication(ctx context.Context, before proto.Workspace, refreshSequence, readSequence uint64, response proto.ProviderAuthenticationMutationResponse) error {
	if response.Workspace == nil || response.Outcome.Change == nil || response.Error != nil || response.Outcome.Superseded {
		return fmt.Errorf("authentication acknowledgement has no current workspace view")
	}
	view := response.Workspace
	if view.ID != before.ID || view.Config == nil {
		return fmt.Errorf("authentication acknowledgement changed workspace")
	}
	surfaces, err := bindAuthenticationSurfaces(view.ProviderSurfaces, before.ProviderSurfaces)
	if err != nil {
		return err
	}
	if err := view.Config.BindProviderSurfaceOwners(surfaces); err != nil {
		return fmt.Errorf("authentication workspace owner binding failed")
	}
	view.Config.SetupAgents()
	generation := response.Outcome.Change.Current.Target.Generation
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.ws.ID != before.ID || !reflect.DeepEqual(w.ws.Authority, before.Authority) || w.authority != nil {
		return fmt.Errorf("provider authentication workspace changed before acknowledgement")
	}
	if refreshSequence < w.appliedRefresh || readSequence < w.providerAuthAppliedRead {
		return fmt.Errorf("provider authentication acknowledgement was superseded")
	}
	if w.providerAuthWorkspaceID == before.ID && generation.Epoch == w.providerAuthGeneration.Epoch && generation.Sequence < w.providerAuthGeneration.Sequence {
		return fmt.Errorf("provider authentication generation moved backwards")
	}
	w.ws.Config, w.ws.ProviderSurfaces = view.Config, surfaces
	// Fence every read started before this exact acknowledgement, including
	// reads that began after the mutation but captured its precommit state.
	w.appliedRefresh = w.refreshSequence.Add(1)
	w.providerAuthWorkspaceID, w.providerAuthAppliedRead, w.providerAuthGeneration = before.ID, w.providerAuthReadSequence.Add(1), generation
	return nil
}

// Public owner fields are presentation references, not enough to construct
// executable authority. Reuse only each exact complete owner already retained
// by this workspace. Authentication never rescans or replaces registrations.
func bindAuthenticationSurfaces(public []proto.AuthenticationProviderSurface, retained []providerregistry.Surface) ([]providerregistry.Surface, error) {
	owners := make(map[string]*providerregistry.RegistrationOwner, len(retained))
	for _, surface := range retained {
		if _, exists := owners[surface.ID]; exists {
			return nil, fmt.Errorf("retained authentication provider catalog has duplicate owners")
		}
		owners[surface.ID] = surface.Owner
	}
	if len(public) != len(retained) {
		return nil, fmt.Errorf("authentication provider catalog changed")
	}
	result := make([]providerregistry.Surface, 0, len(public))
	for _, surface := range public {
		owner, exists := owners[surface.ID]
		if !exists || (owner == nil) != (surface.Owner == nil) || (owner != nil && providerauth.PublicOwner(*owner) != *surface.Owner) {
			return nil, fmt.Errorf("authentication provider owner changed")
		}
		delete(owners, surface.ID)
		result = append(result, (providerregistry.Surface{
			ID: surface.ID, Name: surface.Name, Owner: owner, Available: surface.Available, Availability: surface.Availability, Diagnostic: surface.Diagnostic,
			Description: surface.Description, Order: surface.Order, FlatRate: surface.FlatRate, Brand: surface.Brand,
			DefaultLargeModel: surface.DefaultLargeModel, DefaultSmallModel: surface.DefaultSmallModel, Models: surface.Models, Authentication: surface.Authentication,
			Configuration: surface.Configuration, ConfigurationUI: surface.ConfigurationUI, Images: surface.Images, Instructions: surface.Instructions,
			RuntimeControls: surface.RuntimeControls, UsageAvailable: surface.UsageAvailable,
		}).Clone())
	}
	return result, nil
}
