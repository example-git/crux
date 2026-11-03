package webclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type callbackBackend struct {
	ref      providerauth.OAuthLoginRef
	bound    chan providerauth.OAuthLoginBindRequest
	code     chan providerauth.OAuthLoginCodeRequest
	callback providerauth.OAuthLoginCallback
}

func (b *callbackBackend) WaitProviderOAuthLogin(_ context.Context, _ string, ref providerauth.OAuthLoginRef, _ uint64) (proto.ProviderOAuthLoginResponse, error) {
	return proto.ProviderOAuthLoginResponse{Login: ref, State: &providerauth.OAuthLoginState{Login: b.ref, Sequence: 1, Phase: providerauth.OAuthLoginWaitingLoopback, Callback: &b.callback}}, nil
}

func (b *callbackBackend) BindProviderOAuthLogin(_ context.Context, _ string, request providerauth.OAuthLoginBindRequest) (proto.ProviderOAuthLoginResponse, error) {
	b.bound <- request
	return proto.ProviderOAuthLoginResponse{Login: request.Login, BindingID: request.BindingID, Port: request.Port, State: &providerauth.OAuthLoginState{Login: b.ref, Sequence: 2, Phase: providerauth.OAuthLoginWaitingBrowser, Callback: &b.callback, AuthorizationURL: "https://example.test/login"}}, nil
}

func (b *callbackBackend) SubmitProviderOAuthLoginCode(_ context.Context, _ string, request providerauth.OAuthLoginCodeRequest) (proto.ProviderOAuthLoginResponse, error) {
	b.code <- request
	return proto.ProviderOAuthLoginResponse{Login: request.Login, SubmissionID: request.SubmissionID, State: &providerauth.OAuthLoginState{Login: b.ref, Sequence: 3, Phase: providerauth.OAuthLoginAuthorizing}}, nil
}

func TestWebOAuthLoopbackOriginalIdentityAndListenerLifetime(t *testing.T) {
	ref := providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider", HasOAuth: true, OAuthAdapter: providerregistry.LoginBrowser, OAuthFlowID: "flow"}, Generation: providerauth.Generation{Epoch: strings.Repeat("c", 32), Sequence: 1}}}
	for _, cancel := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancel), func(t *testing.T) {
			backend := &callbackBackend{ref: ref, bound: make(chan providerauth.OAuthLoginBindRequest, 1), code: make(chan providerauth.OAuthLoginCodeRequest, 1), callback: providerauth.OAuthLoginCallback{Mode: "loopback-dynamic", Path: "/callback"}}
			body, err := json.Marshal(ref)
			require.NoError(t, err)
			ctx, stop := context.WithCancel(t.Context())
			defer stop()
			request := httptest.NewRequestWithContext(ctx, "POST", "/", bytes.NewReader(body))
			request.SetPathValue("id", "workspace")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); oauthLoopback(backend)(response, request) }()
			var binding providerauth.OAuthLoginBindRequest
			select {
			case binding = <-backend.bound:
			case <-time.After(5 * time.Second):
				t.Fatal("callback was not bound")
			}
			require.Equal(t, ref, binding.Login)
			require.NoError(t, binding.Validate())
			address := fmt.Sprintf("localhost:%d", binding.Port)
			if cancel {
				stop()
			} else {
				r, err := http.Get("http://" + address + "/callback?code=fixture-code&state=fixture-state")
				require.NoError(t, err)
				_, _ = io.Copy(io.Discard, r.Body)
				require.NoError(t, r.Body.Close())
				require.Equal(t, http.StatusOK, r.StatusCode)
			}
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("callback request did not finish")
			}
			if cancel {
				require.Equal(t, http.StatusRequestTimeout, response.Code)
				require.Empty(t, backend.code)
			} else {
				require.Equal(t, http.StatusOK, response.Code)
				submission := <-backend.code
				require.Equal(t, ref, submission.Login)
				require.Equal(t, "code=fixture-code&state=fixture-state", submission.Input)
				require.NotEqual(t, binding.BindingID, submission.SubmissionID)
				_, err := proto.DecodeProviderOAuthLoginCodeResponse(response.Body.Bytes(), submission)
				require.NoError(t, err)
				require.NotContains(t, response.Body.String(), "fixture-code")
			}
			connection, err := net.DialTimeout("tcp", address, time.Second)
			if connection != nil {
				_ = connection.Close()
			}
			require.Error(t, err)
		})
	}
}

func TestWebOAuthLoopbackRejectsInvalidReferences(t *testing.T) {
	for _, body := range []string{`{}`, `null`, `{"login_id":"private"}`, strings.Repeat(" ", proto.MaxProviderAuthRequestBytes+1)} {
		request := httptest.NewRequestWithContext(t.Context(), "POST", "/", strings.NewReader(body))
		request.SetPathValue("id", "workspace")
		response := httptest.NewRecorder()
		oauthLoopback(nil)(response, request)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.NotContains(t, response.Body.String(), "private")
	}
}
