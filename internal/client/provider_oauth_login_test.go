package client

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func sdkOAuthRef() providerauth.OAuthLoginRef {
	return providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider", HasOAuth: true, OAuthAdapter: providerregistry.LoginBrowser, OAuthFlowID: "flow"}, Generation: providerauth.Generation{Epoch: strings.Repeat("c", 32), Sequence: 1}}}
}

type oauthSDKAction struct {
	name     string
	request  any
	response any
	status   int
	call     func(context.Context, *Client, string) (any, error)
}

func oauthSDKActions(ref providerauth.OAuthLoginRef) []oauthSDKAction {
	bind := providerauth.OAuthLoginBindRequest{Login: ref, BindingID: strings.Repeat("d", 32), Port: 32123}
	code := providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("e", 32), Input: "code=synthetic-private&state=synthetic-private-state"}
	state := func(phase providerauth.OAuthLoginPhase) *providerauth.OAuthLoginState {
		return &providerauth.OAuthLoginState{Login: ref, Sequence: 3, Phase: phase}
	}
	return []oauthSDKAction{
		{"begin", ref, proto.ProviderOAuthLoginResponse{Login: ref, State: state(providerauth.OAuthLoginPreparing)}, 200, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.BeginProviderOAuthLogin(ctx, id, ref)
		}},
		{"bind", bind, proto.ProviderOAuthLoginResponse{Login: ref, BindingID: bind.BindingID, Port: bind.Port, State: state(providerauth.OAuthLoginPreparing)}, 200, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.BindProviderOAuthLogin(ctx, id, bind)
		}},
		{"code", code, proto.ProviderOAuthLoginResponse{Login: ref, SubmissionID: code.SubmissionID, State: state(providerauth.OAuthLoginAuthorizing)}, 200, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.SubmitProviderOAuthLoginCode(ctx, id, code)
		}},
		{"wait", proto.ProviderOAuthLoginWaitRequest{Login: ref, After: 2}, proto.ProviderOAuthLoginResponse{Login: ref, State: state(providerauth.OAuthLoginAuthorized)}, 200, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.WaitProviderOAuthLogin(ctx, id, ref, 2)
		}},
		{"cancel", ref, proto.ProviderOAuthLoginResponse{Login: ref, State: state(providerauth.OAuthLoginCanceled), Error: proto.NewProviderAuthenticationError(providerauth.ErrOAuthLogin)}, 422, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.CancelProviderOAuthLogin(ctx, id, ref)
		}},
		{"complete", ref, proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target, Progress: providerauth.MutationProgress{AccountsSaved: true}}, Error: proto.NewProviderAuthenticationError(providerauth.ErrMutation)}, 422, func(ctx context.Context, c *Client, id string) (any, error) {
			return c.CompleteProviderOAuthLogin(ctx, id, ref)
		}},
	}
}

func TestProviderOAuthSDKExactRoutesAndFailureState(t *testing.T) {
	for _, action := range oauthSDKActions(sdkOAuthRef()) {
		t.Run(action.name, func(t *testing.T) {
			var calls atomic.Int32
			expected, err := json.Marshal(action.request)
			require.NoError(t, err)
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/v1/workspaces/workspace/auth/oauth/"+action.name, r.URL.Path)
				require.Empty(t, r.URL.RawQuery)
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				require.JSONEq(t, string(expected), string(body))
				w.WriteHeader(action.status)
				require.NoError(t, json.NewEncoder(w).Encode(action.response))
			}))
			defer host.Close()
			result, err := action.call(t.Context(), captureClient(t, host), "workspace")
			if action.status == 200 {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Equal(t, action.response, result)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestProviderOAuthSDKRejectsChangedRepliesWithoutRetry(t *testing.T) {
	ref := sdkOAuthRef()
	for _, action := range oauthSDKActions(ref) {
		encoded, err := json.Marshal(action.response)
		require.NoError(t, err)
		valid := string(encoded)
		for name, body := range map[string]string{
			"login identity":  strings.Replace(valid, ref.LoginID, strings.Repeat("f", 32), 1),
			"workspace":       strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"changed"`, 1),
			"alias":           strings.Replace(valid, `"login_id":`, `"Login_ID":`, 1),
			"trailing":        valid + `{}`,
			"oversized":       strings.Repeat(" ", proto.MaxProviderAuthResponseBytes) + valid,
			"status mismatch": valid,
		} {
			t.Run(action.name+"/"+name, func(t *testing.T) {
				var calls atomic.Int32
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					status := action.status
					if name == "status mismatch" {
						if status == 200 {
							status = 422
						} else {
							status = 200
						}
					}
					w.WriteHeader(status)
					_, _ = io.WriteString(w, body)
				}))
				defer host.Close()
				result, err := action.call(t.Context(), captureClient(t, host), "workspace")
				require.Error(t, err)
				require.Zero(t, result)
				require.EqualValues(t, 1, calls.Load())
			})
		}
	}
}

func TestProviderOAuthSDKRejectsInputBeforeHTTPAndNeverRedirects(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(400) }))
	defer host.Close()
	sdk := captureClient(t, host)
	for _, id := range []string{"", "other", ".", "..", "workspace/other", " workspace"} {
		ref := sdkOAuthRef()
		if id == "." || id == ".." {
			ref.Target.WorkspaceID = id
		}
		for _, action := range oauthSDKActions(ref) {
			_, err := action.call(t.Context(), sdk, id)
			require.Error(t, err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, action := range oauthSDKActions(sdkOAuthRef()) {
		_, err := action.call(ctx, sdk, "workspace")
		require.ErrorIs(t, err, context.Canceled)
	}
	bad := providerauth.OAuthLoginCodeRequest{Login: sdkOAuthRef(), SubmissionID: strings.Repeat("e", 32), Input: strings.Repeat("x", providerauth.OAuthLoginInputLimit+1)}
	_, err := sdk.SubmitProviderOAuthLoginCode(t.Context(), "workspace", bad)
	require.Error(t, err)
	require.Zero(t, calls.Load())
	for _, status := range []int{307, 308} {
		origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, host.URL+r.URL.Path, status) }))
		for _, action := range oauthSDKActions(sdkOAuthRef()) {
			_, err := action.call(t.Context(), captureClient(t, origin), "workspace")
			require.Error(t, err)
		}
		origin.Close()
	}
	require.Zero(t, calls.Load())
}
