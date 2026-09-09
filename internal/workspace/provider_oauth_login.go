package workspace

import (
	"context"
	"errors"
	"reflect"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

type oauthSessionCall func(context.Context, *providerauth.Service) (providerauth.OAuthLoginState, error)
type oauthRemoteCall func(context.Context, string) (proto.ProviderOAuthLoginResponse, error)

func (w *AppWorkspace) localOAuthSession(ctx context.Context, ref providerauth.OAuthLoginRef, call oauthSessionCall) (providerauth.OAuthLoginState, error) {
	if err := ref.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	ctx, done := providerAuthContext(ctx, w.providerAuthCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	if w.providerAuth == nil {
		return providerauth.OAuthLoginState{}, errors.New("local provider authentication service is unavailable")
	}
	return call(ctx, w.providerAuth)
}

func (w *AppWorkspace) BeginProviderOAuthLogin(ctx context.Context, request providerauth.OAuthLoginRequest) (providerauth.OAuthLoginState, error) {
	return w.localOAuthSession(ctx, request, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.BeginOAuthLogin(ctx, request)
	})
}

func (w *AppWorkspace) BindProviderOAuthLogin(ctx context.Context, request providerauth.OAuthLoginBindRequest) (providerauth.OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	return w.localOAuthSession(ctx, request.Login, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.BindOAuthLogin(ctx, request)
	})
}

func (w *AppWorkspace) SubmitProviderOAuthLoginCode(ctx context.Context, request providerauth.OAuthLoginCodeRequest) (providerauth.OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	return w.localOAuthSession(ctx, request.Login, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.SubmitOAuthLoginCode(ctx, request)
	})
}

func (w *AppWorkspace) WaitProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef, after uint64) (providerauth.OAuthLoginState, error) {
	return w.localOAuthSession(ctx, ref, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.WaitOAuthLogin(ctx, ref, after)
	})
}

func (w *AppWorkspace) CancelProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef) (providerauth.OAuthLoginState, error) {
	return w.localOAuthSession(ctx, ref, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.CancelOAuthLogin(ctx, ref)
	})
}

func (w *AppWorkspace) CompleteProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target}
	if err := ref.Validate(); err != nil {
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
	result, err := w.providerAuth.CompleteOAuthLogin(ctx, ref)
	return result.Outcome, err
}

func (w *ClientWorkspace) BeginProviderOAuthLogin(ctx context.Context, request providerauth.OAuthLoginRequest) (providerauth.OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	if !w.clientOwned() {
		return w.remoteOAuthSession(ctx, request, func(ctx context.Context, id string) (proto.ProviderOAuthLoginResponse, error) {
			return w.client.BeginProviderOAuthLogin(ctx, id, request)
		})
	}
	id, a := w.workspaceID(), w.authority
	if id == "" || id != request.Target.WorkspaceID || a == nil || a.store == nil {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	defer a.mu.Unlock()
	if w.authority != a || !w.clientOwned() || w.workspaceID() != id {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	if err := w.verifyClientProviderAuthAuthority(id, a); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	// A lost Begin reply is a read of the original retained login, including
	// terminal failures. Do not replace its generation or initialize a new
	// service when a later unacknowledged transaction is present.
	if a.providerAuth != nil && a.providerAuthWorkspaceID == id {
		state, err := a.providerAuth.WaitOAuthLogin(ctx, request, 0)
		if !errors.Is(err, providerauth.ErrOAuthLoginUnavailable) {
			return state, err
		}
	}
	if err := w.prepareClientProviderAuth(ctx, id); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	if a.unacknowledgedClientAuthentication(id) {
		return providerauth.OAuthLoginState{}, errors.New("a saved client authentication change requires explicit recovery before another login")
	}
	return a.providerAuth.BeginOAuthLoginForAccepted(ctx, request, a.accepted, a.configView())
}

// Interaction states do not publish or acknowledge configuration. Follow-ups
// retain the initiating reference even after Begin consumed its generation.
// Wait must not hold a.mu, which is needed by cancellation and completion.
func (w *ClientWorkspace) clientOAuthSession(ctx context.Context, ref providerauth.OAuthLoginRef, call oauthSessionCall, remote oauthRemoteCall) (providerauth.OAuthLoginState, error) {
	if err := ref.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	if !w.clientOwned() {
		return w.remoteOAuthSession(ctx, ref, remote)
	}
	id, a := w.workspaceID(), w.authority
	if id == "" || id != ref.Target.WorkspaceID || a == nil {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	service := a.providerAuth
	valid := w.authority == a && w.clientOwned() && a.providerAuthWorkspaceID == id && w.workspaceID() == id
	if valid {
		valid = w.verifyClientProviderAuthAuthority(id, a) == nil
	}
	a.mu.Unlock()
	if !valid || service == nil {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	state, err := call(ctx, service)
	if lockErr := lockProviderAuthAuthority(ctx, a); lockErr != nil {
		return state, lockErr
	}
	defer a.mu.Unlock()
	if w.authority != a || !w.clientOwned() || a.providerAuth != service || a.providerAuthWorkspaceID != id || w.workspaceID() != id {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	if verifyErr := w.verifyClientProviderAuthAuthority(id, a); verifyErr != nil {
		return providerauth.OAuthLoginState{}, verifyErr
	}
	return state, err
}

func (w *ClientWorkspace) remoteOAuthSession(ctx context.Context, ref providerauth.OAuthLoginRef, call oauthRemoteCall) (providerauth.OAuthLoginState, error) {
	before := w.cached()
	if before.ID == "" || before.ID != ref.Target.WorkspaceID || (before.Authority != nil && before.Authority.Mode != "server") || w.client == nil {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	response, err := call(ctx, before.ID)
	if response.State == nil {
		if err != nil {
			return providerauth.OAuthLoginState{}, err
		}
		return providerauth.OAuthLoginState{}, providerauth.ErrReceiptUnverified
	}
	state := *response.State
	if state.Login != ref || state.Validate() != nil {
		return providerauth.OAuthLoginState{}, providerauth.ErrReceiptUnverified
	}
	w.mu.RLock()
	valid := w.ws.ID == before.ID && reflect.DeepEqual(w.ws.Authority, before.Authority) && w.authority == nil
	w.mu.RUnlock()
	if !valid {
		return providerauth.OAuthLoginState{}, providerauth.ErrStale
	}
	if ctx.Err() != nil {
		return state, ctx.Err()
	}
	return state, err
}

func (w *ClientWorkspace) BindProviderOAuthLogin(ctx context.Context, request providerauth.OAuthLoginBindRequest) (providerauth.OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	return w.clientOAuthSession(ctx, request.Login, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.BindOAuthLogin(ctx, request)
	}, func(ctx context.Context, id string) (proto.ProviderOAuthLoginResponse, error) {
		return w.client.BindProviderOAuthLogin(ctx, id, request)
	})
}

func (w *ClientWorkspace) SubmitProviderOAuthLoginCode(ctx context.Context, request providerauth.OAuthLoginCodeRequest) (providerauth.OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return providerauth.OAuthLoginState{}, err
	}
	return w.clientOAuthSession(ctx, request.Login, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.SubmitOAuthLoginCode(ctx, request)
	}, func(ctx context.Context, id string) (proto.ProviderOAuthLoginResponse, error) {
		return w.client.SubmitProviderOAuthLoginCode(ctx, id, request)
	})
}

func (w *ClientWorkspace) WaitProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef, after uint64) (providerauth.OAuthLoginState, error) {
	return w.clientOAuthSession(ctx, ref, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.WaitOAuthLogin(ctx, ref, after)
	}, func(ctx context.Context, id string) (proto.ProviderOAuthLoginResponse, error) {
		return w.client.WaitProviderOAuthLogin(ctx, id, ref, after)
	})
}

func (w *ClientWorkspace) CancelProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef) (providerauth.OAuthLoginState, error) {
	return w.clientOAuthSession(ctx, ref, func(ctx context.Context, service *providerauth.Service) (providerauth.OAuthLoginState, error) {
		return service.CancelOAuthLogin(ctx, ref)
	}, func(ctx context.Context, id string) (proto.ProviderOAuthLoginResponse, error) {
		return w.client.CancelProviderOAuthLogin(ctx, id, ref)
	})
}

func (w *ClientWorkspace) CompleteProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target}
	if err := ref.Validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if w.clientOwned() {
		return w.mutateClientAuthentication(ctx, clientAuthenticationRequest{operationID: ref.OperationID, loginID: ref.LoginID, target: ref.Target})
	}
	return w.mutateServerAuthentication(ctx, initial, func(id string) (proto.ProviderAuthenticationMutationResponse, error) {
		return w.client.CompleteProviderOAuthLogin(ctx, id, ref)
	})
}
