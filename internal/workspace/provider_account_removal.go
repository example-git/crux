package workspace

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (w *AppWorkspace) RemoveProviderAccount(ctx context.Context, request providerauth.RemoveRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, RemovedAccountID: request.AccountID, Previous: request.Target}
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
	result, err := w.providerAuth.Remove(ctx, request)
	return result.Outcome, err
}

func (w *ClientWorkspace) RemoveProviderAccount(ctx context.Context, request providerauth.RemoveRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, RemovedAccountID: request.AccountID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.clientOwned() {
		return w.removeClientAuthentication(ctx, request)
	}
	return w.mutateServerAuthentication(ctx, initial, func(id string) (proto.ProviderAuthenticationMutationResponse, error) {
		return w.client.RemoveProviderAccount(ctx, id, request)
	})
}

func (w *ClientWorkspace) removeClientAuthentication(ctx context.Context, request providerauth.RemoveRequest) (providerauth.MutationOutcome, error) {
	return w.mutateClientAuthentication(ctx, clientAuthenticationRequest{operationID: request.OperationID, target: request.Target, accountID: request.AccountID, removedAccountID: request.AccountID})
}
