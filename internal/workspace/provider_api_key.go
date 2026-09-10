package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func initialAPIKeyCheckOutcome(request providerauth.APIKeyCheckRequest) providerauth.APIKeyCheckOutcome {
	return providerauth.APIKeyCheckOutcome{
		CheckID: request.CheckID, Previous: request.Target, CredentialID: request.CredentialID,
		Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeNotProbed, Policy: config.ConnectionProbePolicyNone},
	}
}

func (w *AppWorkspace) CheckProviderAPIKey(ctx context.Context, request providerauth.APIKeyCheckRequest) (providerauth.APIKeyCheckOutcome, error) {
	initial := initialAPIKeyCheckOutcome(request)
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.providerAuth == nil {
		return initial, errors.New("local provider authentication service is unavailable")
	}
	return w.providerAuth.CheckAPIKey(ctx, request)
}

func (w *AppWorkspace) SaveCheckedProviderAPIKey(ctx context.Context, request providerauth.APIKeySaveRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, CheckID: request.CheckID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.providerAuth == nil {
		return initial, errors.New("local provider authentication service is unavailable")
	}
	result, err := w.providerAuth.SaveAPIKey(ctx, request)
	return result.Outcome, err
}

func (w *ClientWorkspace) CheckProviderAPIKey(ctx context.Context, request providerauth.APIKeyCheckRequest) (providerauth.APIKeyCheckOutcome, error) {
	initial := initialAPIKeyCheckOutcome(request)
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	id := w.workspaceID()
	if id == "" || request.Target.WorkspaceID != id {
		return initial, providerauth.ErrStale
	}
	if w.clientOwned() {
		a := w.authority
		if a == nil || a.store == nil {
			return initial, errors.New("owning client authentication authority is unavailable")
		}
		if err := lockProviderAuthAuthority(ctx, a); err != nil {
			return initial, err
		}
		defer a.mu.Unlock()
		if w.authority != a || !w.clientOwned() {
			return initial, providerauth.ErrStale
		}
		if err := w.prepareClientProviderAuth(ctx, id); err != nil {
			return initial, err
		}
		if a.unacknowledgedClientAuthentication(id) {
			return initial, errors.New("a saved client authentication change requires explicit recovery before checking another credential")
		}
		outcome, err := a.providerAuth.CheckAPIKeyForAccepted(ctx, request, a.accepted, a.configView())
		if err != nil {
			return outcome, err
		}
		if w.authority != a || !w.clientOwned() {
			outcome.CheckedTarget = nil
			return outcome, providerauth.ErrStale
		}
		if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
			outcome.CheckedTarget = nil
			return outcome, err
		}
		if err := ctx.Err(); err != nil {
			outcome.CheckedTarget = nil
			return outcome, err
		}
		return outcome, nil
	}
	if w.client == nil {
		return initial, errors.New("provider authentication client is unavailable")
	}
	sequence := w.providerAuthReadSequence.Add(1)
	response, err := w.client.CheckProviderAPIKey(ctx, id, request)
	if err != nil {
		if response.Outcome.CheckID == "" {
			return initial, err
		}
		return response.Outcome, err
	}
	outcome := response.Outcome
	if outcome.CheckedTarget == nil {
		return outcome, providerauth.ErrReceiptUnverified
	}
	if err := ctx.Err(); err != nil {
		outcome.CheckedTarget = nil
		return outcome, err
	}
	if err := w.acceptProviderAuthRead(id, sequence, outcome.CheckedTarget.Generation); err != nil {
		outcome.CheckedTarget = nil
		return outcome, err
	}
	return outcome, nil
}

func (w *ClientWorkspace) SaveCheckedProviderAPIKey(ctx context.Context, request providerauth.APIKeySaveRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, CheckID: request.CheckID, Previous: request.Target}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.clientOwned() {
		return w.mutateClientAuthentication(ctx, clientAuthenticationRequest{operationID: request.OperationID, target: request.Target, checkID: request.CheckID})
	}
	return w.mutateServerAuthentication(ctx, initial, func(id string) (proto.ProviderAuthenticationMutationResponse, error) {
		return w.client.SaveCheckedProviderAPIKey(ctx, id, request)
	})
}
