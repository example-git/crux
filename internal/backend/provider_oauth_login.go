package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func (b *Backend) providerOAuthInteraction(ctx context.Context, id string, response proto.ProviderOAuthLoginResponse, validateRequest func() error, run func(context.Context, *providerauth.Service) (providerauth.OAuthLoginState, error), validateResponse func(proto.ProviderOAuthLoginResponse) error) (proto.ProviderOAuthLoginResponse, error) {
	fail := func(err error) (proto.ProviderOAuthLoginResponse, error) {
		response.Error = proto.NewProviderAuthenticationError(err)
		return response, err
	}
	if err := validateRequest(); err != nil {
		return fail(err)
	}
	if response.Login.Target.WorkspaceID != id {
		return fail(errors.New("OAuth login target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return fail(err)
	}
	defer done()
	state, err := run(ctx, ws.providerAuth)
	if state.Login.LoginID != "" {
		response.State = &state
	}
	if err != nil {
		response.Error = proto.NewProviderAuthenticationError(err)
	}
	if validationErr := validateResponse(response); validationErr != nil {
		// A malformed state cannot be a substitute for this request's session.
		response.State = nil
		return fail(validationErr)
	}
	return response, err
}

func (b *Backend) BeginProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginRequest) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: request}, request.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.BeginOAuthLogin(ctx, request)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateBegin(request) })
}

func (b *Backend) BindProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginBindRequest) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: request.Login, BindingID: request.BindingID, Port: request.Port}, request.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.BindOAuthLogin(ctx, request)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateBind(request) })
}

func (b *Backend) SubmitProviderOAuthLoginCode(ctx context.Context, id string, request providerauth.OAuthLoginCodeRequest) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: request.Login, SubmissionID: request.SubmissionID}, request.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.SubmitOAuthLoginCode(ctx, request)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateCode(request) })
}

func (b *Backend) WaitProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef, after uint64) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: ref}, ref.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.WaitOAuthLogin(ctx, ref, after)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateWait(ref, after) })
}

func (b *Backend) CancelProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef) (proto.ProviderOAuthLoginResponse, error) {
	return b.providerOAuthInteraction(ctx, id, proto.ProviderOAuthLoginResponse{Login: ref}, ref.Validate,
		func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
			return service.CancelOAuthLogin(ctx, ref)
		},
		func(r proto.ProviderOAuthLoginResponse) error { return r.ValidateCancel(ref) })
}

func (b *Backend) CompleteProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef) (proto.ProviderAuthenticationMutationResponse, error) {
	initial := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target}}
	if err := ref.Validate(); err != nil {
		return authenticationMutationFailure(initial, err)
	}
	if ref.Target.WorkspaceID != id {
		return authenticationMutationFailure(initial, errors.New("OAuth login target does not match workspace"))
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, id)
	if err != nil {
		return authenticationMutationFailure(initial, err)
	}
	defer done()
	result, err := ws.providerAuth.CompleteOAuthLogin(ctx, ref)
	response, err := finishAuthenticationMutation(ctx, ws, result, err)
	if validationErr := response.ValidateOAuthLogin(ref); validationErr != nil {
		return authenticationMutationFailure(response, validationErr)
	}
	return response, err
}
