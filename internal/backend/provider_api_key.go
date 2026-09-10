package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) CheckProviderAPIKey(ctx context.Context, id string, request providerauth.APIKeyCheckRequest) (proto.ProviderAPIKeyCheckResponse, error) {
	response := proto.ProviderAPIKeyCheckResponse{Outcome: providerauth.APIKeyCheckOutcome{
		CheckID: request.CheckID, Previous: request.Target, CredentialID: request.CredentialID,
		Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeNotProbed, Policy: config.ConnectionProbePolicyNone},
	}}
	fail := func(err error) (proto.ProviderAPIKeyCheckResponse, error) {
		response.Outcome.CheckedTarget = nil
		response.Error = proto.NewProviderAuthenticationError(err)
		return response, err
	}
	if err := request.Validate(); err != nil {
		return fail(err)
	}
	if request.Target.WorkspaceID != id {
		return fail(errors.New("API key check target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return fail(err)
	}
	defer done()
	response.Outcome, err = ws.providerAuth.CheckAPIKey(ctx, request)
	if err != nil {
		return fail(err)
	}
	if err := response.Validate(request); err != nil {
		return fail(err)
	}
	return response, nil
}

func (b *Backend) SaveCheckedProviderAPIKey(ctx context.Context, id string, request providerauth.APIKeySaveRequest) (proto.ProviderAuthenticationMutationResponse, error) {
	initial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: request.OperationID, CheckID: request.CheckID, Previous: request.Target}}
	if err := request.Validate(); err != nil {
		return authenticationMutationFailure(initial, err)
	}
	if request.Target.WorkspaceID != id {
		return authenticationMutationFailure(initial, errors.New("checked API key target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return authenticationMutationFailure(initial, err)
	}
	defer done()
	result, err := ws.providerAuth.SaveAPIKey(ctx, request)
	response, err := finishAuthenticationMutation(ctx, ws, result, err)
	if err == nil {
		if validationErr := response.ValidateAPIKeySave(request); validationErr != nil {
			return authenticationMutationFailure(response, validationErr)
		}
	}
	return response, err
}
