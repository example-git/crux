package backend

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) RemoveProviderAccount(ctx context.Context, id string, request providerauth.RemoveRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	initial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, RemovedAccountID: request.AccountID, Previous: request.Target}}
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
	result, err := ws.providerAuth.Remove(ctx, request)
	response, err := finishAuthenticationMutation(ctx, ws, result, err)
	if err == nil {
		if validErr := response.ValidateRemove(request); validErr != nil {
			return authenticationMutationFailure(response, validErr)
		}
	}
	return response, err
}
