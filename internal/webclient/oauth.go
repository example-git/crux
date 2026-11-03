package webclient

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callbackrelay"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

type oauthBackend interface {
	WaitProviderOAuthLogin(context.Context, string, providerauth.OAuthLoginRef, uint64) (proto.ProviderOAuthLoginResponse, error)
	BindProviderOAuthLogin(context.Context, string, providerauth.OAuthLoginBindRequest) (proto.ProviderOAuthLoginResponse, error)
	SubmitProviderOAuthLoginCode(context.Context, string, providerauth.OAuthLoginCodeRequest) (proto.ProviderOAuthLoginResponse, error)
}

func oauthLoopback(backend oauthBackend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proto.MaxProviderAuthRequestBytes))
		if err != nil {
			http.Error(w, "invalid OAuth login request", http.StatusBadRequest)
			return
		}
		ref, err := proto.DecodeProviderOAuthLoginBeginRequest(body)
		if err != nil || ref.Target.WorkspaceID != r.PathValue("id") {
			http.Error(w, "invalid OAuth login reference", http.StatusBadRequest)
			return
		}
		state, err := backend.WaitProviderOAuthLogin(r.Context(), ref.Target.WorkspaceID, ref, 0)
		if err != nil || state.ValidateWait(ref, 0) != nil || state.State == nil || state.State.Phase != providerauth.OAuthLoginWaitingLoopback {
			http.Error(w, "login is not waiting for a local callback listener", http.StatusConflict)
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		if state.State.ExpiresAt > 0 {
			var stop context.CancelFunc
			ctx, stop = context.WithDeadline(ctx, time.UnixMilli(state.State.ExpiresAt))
			defer stop()
		}
		callback := state.State.Callback
		relay, err := callbackrelay.Start(ctx, oauth.CallbackRequirement{Mode: callback.Mode, Port: callback.Port, Path: callback.Path})
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		defer relay.Close()
		var ids [32]byte
		if _, err = rand.Read(ids[:]); err != nil {
			http.Error(w, "could not create callback identity", http.StatusInternalServerError)
			return
		}
		binding := providerauth.OAuthLoginBindRequest{Login: ref, BindingID: hex.EncodeToString(ids[:16]), Port: relay.Port()}
		bound, err := backend.BindProviderOAuthLogin(ctx, ref.Target.WorkspaceID, binding)
		if err != nil || bound.ValidateBind(binding) != nil {
			http.Error(w, "could not bind the original OAuth login", http.StatusConflict)
			return
		}
		input, err := relay.Wait(ctx)
		if err != nil {
			http.Error(w, "OAuth callback listener closed before authorization", http.StatusRequestTimeout)
			return
		}
		submission := providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: hex.EncodeToString(ids[16:]), Input: input}
		result, err := backend.SubmitProviderOAuthLoginCode(ctx, ref.Target.WorkspaceID, submission)
		if err != nil || result.ValidateCode(submission) != nil {
			http.Error(w, "the original OAuth login did not accept the callback", http.StatusConflict)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	}
}
