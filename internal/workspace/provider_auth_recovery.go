package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// ProviderAuthenticationRecoveryRequest authorizes one explicit attempt to
// publish an original completed client authentication change. A retry retains
// every field. A new explicit attempt uses a new ID and a larger sequence.
// Neither form authorizes another local account or configuration mutation.
type ProviderAuthenticationRecoveryRequest struct {
	RecoveryID       string
	OperationID      string
	Target           providerauth.Target
	RecoverySequence uint64
}

func (request ProviderAuthenticationRecoveryRequest) Validate() error {
	return clientAuthenticationRecoveryRequest(request).validate()
}

// ProviderAuthenticationRecoverer is an optional Workspace capability for
// owning-client publication recovery. Server-owned and local authentication
// use their original Switch/Logout receipt instead.
type ProviderAuthenticationRecoverer interface {
	CanRecoverProviderAuthentication() bool
	RecoverProviderAuthentication(context.Context, ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error)
}

func (w *ClientWorkspace) CanRecoverProviderAuthentication() bool {
	return w != nil && w.authority != nil && w.authority.store != nil && w.client != nil && w.clientOwned()
}

func (w *ClientWorkspace) RecoverProviderAuthentication(ctx context.Context, request ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	if !w.CanRecoverProviderAuthentication() {
		return initial, errors.New("authentication publication recovery requires the owning client workspace; retry the original authentication request for a server-owned or local workspace")
	}
	return w.recoverClientAuthentication(ctx, clientAuthenticationRecoveryRequest(request))
}
