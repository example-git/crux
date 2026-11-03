package webclient

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

func remoteAuthJSON(w http.ResponseWriter, value any, err error) {
	body, encodeErr := json.Marshal(value)
	if encodeErr != nil || len(body) > proto.MaxProviderAuthResponseBytes {
		http.Error(w, "authentication response is unavailable", http.StatusInternalServerError)
		return
	}
	status := http.StatusOK
	if err != nil {
		status = http.StatusUnprocessableEntity
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusRequestTimeout
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func remoteAuthError(err error) *proto.ProviderAuthenticationError {
	if err == nil {
		return nil
	}
	return proto.NewProviderAuthenticationError(err)
}

func remoteAuthOperation[T, R any](decode func([]byte) (T, error), call func(context.Context, T) (R, error), response func(T, R, error) any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := int64(proto.MaxProviderAuthRequestBytes)
		if r.URL.Path == "/auth/api-key/check" {
			limit = proto.MaxProviderAPIKeyCheckRequestBytes
		} else if r.URL.Path == "/auth/oauth/code" {
			limit = proto.MaxProviderOAuthCodeRequestBytes
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		if err != nil {
			http.Error(w, "invalid authentication request", http.StatusBadRequest)
			return
		}
		request, err := decode(body)
		if err != nil {
			http.Error(w, "invalid authentication request", http.StatusBadRequest)
			return
		}
		result, err := call(r.Context(), request)
		remoteAuthJSON(w, response(request, result, err), err)
	}
}

func remoteMutation[T any](_ T, result providerauth.MutationOutcome, err error) any {
	return struct {
		Outcome     providerauth.MutationOutcome       `json:"outcome"`
		ClientOwned bool                               `json:"client_owned"`
		Error       *proto.ProviderAuthenticationError `json:"error,omitempty"`
	}{result, true, remoteAuthError(err)}
}

func remoteOAuthResponse(response proto.ProviderOAuthLoginResponse, state providerauth.OAuthLoginState, err error) proto.ProviderOAuthLoginResponse {
	if state.Login.LoginID != "" {
		response.State = &state
	}
	response.Error = remoteAuthError(err)
	return response
}

func (a *remoteAttachment) authenticationHandler() http.Handler {
	mux := http.NewServeMux()
	selected := a.workspace
	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		result, err := selected.ProviderAuthentication(r.Context())
		if err != nil {
			remoteAuthJSON(w, map[string]any{"error": remoteAuthError(err)}, err)
			return
		}
		remoteAuthJSON(w, result, nil)
	})
	mux.HandleFunc("POST /auth/accounts", remoteAuthOperation(proto.DecodeProviderAuthTarget, selected.ProviderAccounts,
		func(_ providerauth.Target, result providerauth.AccountsState, err error) any {
			if err != nil {
				return map[string]any{"error": remoteAuthError(err)}
			}
			return result
		}))
	mux.HandleFunc("POST /auth/external", remoteAuthOperation(proto.DecodeExternalAuthenticationRequest,
		func(ctx context.Context, request config.ExternalAuthenticationRequest) (config.ExternalAuthentication, error) {
			return a.client.ExternalAuthentication(ctx, selected.Snapshot().ID, request)
		}, func(request config.ExternalAuthenticationRequest, result config.ExternalAuthentication, err error) any {
			if err != nil {
				return map[string]any{"error": remoteAuthError(err)}
			}
			return proto.ExternalAuthenticationResponse{WorkspaceID: selected.Snapshot().ID, Service: request.Service, Authentication: result}
		}))
	mux.HandleFunc("POST /auth/switch", remoteAuthOperation(proto.DecodeProviderAuthSwitchRequest, selected.SwitchProviderAccount, remoteMutation[providerauth.SwitchRequest]))
	mux.HandleFunc("POST /auth/logout", remoteAuthOperation(proto.DecodeProviderAuthLogoutRequest, selected.LogoutProvider, remoteMutation[providerauth.LogoutRequest]))
	mux.HandleFunc("POST /auth/remove", remoteAuthOperation(proto.DecodeProviderAuthRemoveRequest, selected.RemoveProviderAccount, remoteMutation[providerauth.RemoveRequest]))
	mux.HandleFunc("POST /auth/api-key/save", remoteAuthOperation(proto.DecodeProviderAPIKeySaveRequest, selected.SaveCheckedProviderAPIKey, remoteMutation[providerauth.APIKeySaveRequest]))
	mux.HandleFunc("POST /auth/api-key/check", remoteAuthOperation(proto.DecodeProviderAPIKeyCheckRequest, selected.CheckProviderAPIKey,
		func(_ providerauth.APIKeyCheckRequest, result providerauth.APIKeyCheckOutcome, err error) any {
			return proto.ProviderAPIKeyCheckResponse{Outcome: result, Error: remoteAuthError(err)}
		}))
	mux.HandleFunc("POST /auth/local-repair", remoteAuthOperation(proto.DecodeProviderLocalRepairRequest, selected.RepairLocalAuthentication,
		func(request providerauth.LocalRepairRequest, result config.LocalAuthenticationRepairResult, err error) any {
			response := proto.ProviderLocalRepairResponse{Request: request, Result: result}
			if err != nil {
				response.Error = proto.NewProviderLocalRepairError(err)
			}
			return response
		}))
	mux.HandleFunc("POST /auth/oauth/begin", remoteAuthOperation(proto.DecodeProviderOAuthLoginBeginRequest, selected.BeginProviderOAuthLogin,
		func(request providerauth.OAuthLoginRequest, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/recover", remoteAuthOperation(proto.DecodeProviderOAuthLoginRecoverRequest, selected.RecoverProviderOAuthLogin,
		func(request providerauth.OAuthLoginRecoveryRequest, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login, RecoveryWorkspaceID: request.OriginalWorkspaceID, RecoveryOperationID: request.OriginalOperationID}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/bind", remoteAuthOperation(proto.DecodeProviderOAuthLoginBindRequest, selected.BindProviderOAuthLogin,
		func(request providerauth.OAuthLoginBindRequest, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login, BindingID: request.BindingID, Port: request.Port}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/code", remoteAuthOperation(proto.DecodeProviderOAuthLoginCodeRequest, selected.SubmitProviderOAuthLoginCode,
		func(request providerauth.OAuthLoginCodeRequest, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login, SubmissionID: request.SubmissionID}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/wait", remoteAuthOperation(proto.DecodeProviderOAuthLoginWaitRequest,
		func(ctx context.Context, request proto.ProviderOAuthLoginWaitRequest) (providerauth.OAuthLoginState, error) {
			return selected.WaitProviderOAuthLogin(ctx, request.Login, request.After)
		}, func(request proto.ProviderOAuthLoginWaitRequest, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/cancel", remoteAuthOperation(proto.DecodeProviderOAuthLoginCancelRequest, selected.CancelProviderOAuthLogin,
		func(request providerauth.OAuthLoginRef, result providerauth.OAuthLoginState, err error) any {
			return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request}, result, err)
		}))
	mux.HandleFunc("POST /auth/oauth/complete", remoteAuthOperation(proto.DecodeProviderOAuthLoginCompleteRequest, selected.CompleteProviderOAuthLogin, remoteMutation[providerauth.OAuthLoginRef]))
	mux.HandleFunc("POST /auth/oauth/results", remoteAuthOperation(proto.DecodeProviderOAuthLoginRecoveryListRequest, selected.ListProviderOAuthLoginResults,
		func(_ providerauth.Target, result providerauth.OAuthLoginRecoveryList, err error) any {
			return proto.ProviderOAuthLoginRecoveryListResponse{List: result, Error: remoteAuthError(err)}
		}))
	mux.HandleFunc("POST /auth/oauth/abandon", remoteAuthOperation(proto.DecodeProviderOAuthLoginAbandonRequest, selected.AbandonProviderOAuthLoginResult,
		func(_ providerauth.OAuthLoginAbandonRequest, result providerauth.OAuthLoginAbandonOutcome, err error) any {
			return proto.ProviderOAuthLoginAbandonResponse{Outcome: result, Error: remoteAuthError(err)}
		}))
	return mux
}

func (a *remoteAttachment) WaitProviderOAuthLogin(ctx context.Context, id string, ref providerauth.OAuthLoginRef, after uint64) (proto.ProviderOAuthLoginResponse, error) {
	if id != a.workspace.Snapshot().ID {
		return proto.ProviderOAuthLoginResponse{}, errors.New("OAuth workspace changed")
	}
	state, err := a.workspace.WaitProviderOAuthLogin(ctx, ref, after)
	return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: ref}, state, err), err
}

func (a *remoteAttachment) BindProviderOAuthLogin(ctx context.Context, id string, request providerauth.OAuthLoginBindRequest) (proto.ProviderOAuthLoginResponse, error) {
	if id != a.workspace.Snapshot().ID {
		return proto.ProviderOAuthLoginResponse{}, errors.New("OAuth workspace changed")
	}
	state, err := a.workspace.BindProviderOAuthLogin(ctx, request)
	return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login, BindingID: request.BindingID, Port: request.Port}, state, err), err
}

func (a *remoteAttachment) SubmitProviderOAuthLoginCode(ctx context.Context, id string, request providerauth.OAuthLoginCodeRequest) (proto.ProviderOAuthLoginResponse, error) {
	if id != a.workspace.Snapshot().ID {
		return proto.ProviderOAuthLoginResponse{}, errors.New("OAuth workspace changed")
	}
	state, err := a.workspace.SubmitProviderOAuthLoginCode(ctx, request)
	return remoteOAuthResponse(proto.ProviderOAuthLoginResponse{Login: request.Login, SubmissionID: request.SubmissionID}, state, err), err
}
