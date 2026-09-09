package backend

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/pubsub"
)

func (b *Backend) SwitchProviderAccount(ctx context.Context, id string, request providerauth.SwitchRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	initial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}}
	if err := request.Validate(); err != nil {
		return authenticationMutationFailure(initial, err)
	}
	if request.Target.WorkspaceID != id {
		return authenticationMutationFailure(initial, fmt.Errorf("provider authentication target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return authenticationMutationFailure(initial, err)
	}
	defer done()
	result, err := ws.providerAuth.Switch(ctx, request)
	response, err := finishAuthenticationMutation(ctx, ws, result, err)
	if err == nil {
		if validErr := response.ValidateSwitch(request); validErr != nil {
			return authenticationMutationFailure(response, validErr)
		}
	}
	return response, err
}

func (b *Backend) LogoutProvider(ctx context.Context, id string, request providerauth.LogoutRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	initial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}}
	if err := request.Validate(); err != nil {
		return authenticationMutationFailure(initial, err)
	}
	if request.Target.WorkspaceID != id {
		return authenticationMutationFailure(initial, fmt.Errorf("provider authentication target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return authenticationMutationFailure(initial, err)
	}
	defer done()
	result, err := ws.providerAuth.Logout(ctx, request)
	response, err := finishAuthenticationMutation(ctx, ws, result, err)
	if err == nil {
		if validErr := response.ValidateLogout(request); validErr != nil {
			return authenticationMutationFailure(response, validErr)
		}
	}
	return response, err
}

func authenticationMutationFailure(response proto.ProviderAuthenticationMutationResponse, err error) (proto.ProviderAuthenticationMutationResponse, error) {
	response.Workspace = nil
	response.Error = proto.NewProviderAuthenticationError(err)
	return response, err
}

func finishAuthenticationMutation(ctx context.Context, ws *Workspace, result providerauth.MutationResult, operationErr error) (proto.ProviderAuthenticationMutationResponse, error) {
	response := proto.ProviderAuthenticationMutationResponse{Outcome: result.Outcome}
	// This event announces publication, including an unverified partial result;
	// it is never the coherent acknowledgement. The transaction owns auth signals.
	if response.Outcome.Progress.RuntimePublished {
		ws.SendEvent(pubsub.Event[proto.ConfigChanged]{Type: pubsub.UpdatedEvent, Payload: proto.ConfigChanged{WorkspaceID: ws.ID}})
	}
	if operationErr != nil {
		return authenticationMutationFailure(response, operationErr)
	}
	if response.Outcome.Superseded {
		return response, nil
	}
	snapshot, ok := result.RuntimeSnapshot()
	expected, captured := result.AuthenticationCapture()
	if !ok || !captured {
		return authenticationMutationFailure(response, providerauth.ErrReceiptUnverified)
	}
	view, err := proto.NewAuthenticationWorkspaceView(ws.ID, snapshot)
	if err != nil {
		return authenticationMutationFailure(response, providerauth.ErrReceiptUnverified)
	}
	// Comparison only: never build a view from this later observation or bless
	// changed credentials/accounts as the original transaction's result.
	observed, err := ws.Cfg.CaptureAuthentication(ctx)
	if err != nil {
		return authenticationMutationFailure(response, fmt.Errorf("%w: %w", providerauth.ErrReceiptUnverified, err))
	}
	if !expected.SameObservation(observed) {
		response.Outcome.Superseded = true
		return response, nil
	}
	response.Workspace = view
	return response, nil
}
