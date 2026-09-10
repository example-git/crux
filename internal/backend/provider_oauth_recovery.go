package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) RecoverProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginRecoveryRequest) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: request.Login, RecoveryWorkspaceID: request.OriginalWorkspaceID, RecoveryOperationID: request.OriginalOperationID}, request.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.RecoverOAuthLogin(ctx, request)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateRecover(request) })
}

func (b *Backend) ListProviderOAuthLoginResults(ctx context.Context, id string, target providerauth.Target) (proto.ProviderOAuthLoginRecoveryListResponse, error) {
	response := proto.ProviderOAuthLoginRecoveryListResponse{List: providerauth.OAuthLoginRecoveryList{Target: target, Results: []providerauth.OAuthLoginRecordedResult{}}}
	fail := func(err error) (proto.ProviderOAuthLoginRecoveryListResponse, error) {
		response.Error = proto.NewProviderAuthenticationError(err)
		return response, err
	}
	if err := target.Validate(); err != nil {
		return fail(err)
	}
	if target.WorkspaceID != id {
		return fail(errors.New("OAuth recovery target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return fail(err)
	}
	defer done()
	list, err := ws.providerAuth.ListOAuthLoginResults(ctx, target)
	if err != nil {
		return fail(err)
	}
	response.List = list
	if err := response.Validate(target); err != nil {
		return fail(err)
	}
	return response, nil
}
