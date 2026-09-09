package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func serverOAuthRef() providerauth.OAuthLoginRef {
	return providerauth.OAuthLoginRef{LoginID: strings.Repeat("a", 32), OperationID: strings.Repeat("b", 32), Target: providerauth.Target{WorkspaceID: "workspace", Owner: providerauth.Owner{ProviderID: "provider", HasOAuth: true, OAuthAdapter: providerregistry.LoginBrowser, OAuthFlowID: "flow"}, Generation: providerauth.Generation{Epoch: strings.Repeat("c", 32), Sequence: 1}}}
}

func TestProviderOAuthRoutesRejectMalformedBeforeBackend(t *testing.T) {
	ref := serverOAuthRef()
	c := &controllerV1{}
	for _, test := range []struct {
		name    string
		request any
		maximum int
		handler http.HandlerFunc
	}{
		{"begin", ref, proto.MaxProviderAuthRequestBytes, c.handlePostWorkspaceOAuthLoginBegin},
		{"bind", providerauth.OAuthLoginBindRequest{Login: ref, BindingID: strings.Repeat("d", 32), Port: 1234}, proto.MaxProviderAuthRequestBytes, c.handlePostWorkspaceOAuthLoginBind},
		{"code", providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: strings.Repeat("e", 32), Input: "synthetic-private"}, proto.MaxProviderOAuthCodeRequestBytes, c.handlePostWorkspaceOAuthLoginCode},
		{"wait", proto.ProviderOAuthLoginWaitRequest{Login: ref, After: 1}, proto.MaxProviderAuthRequestBytes, c.handlePostWorkspaceOAuthLoginWait},
		{"cancel", ref, proto.MaxProviderAuthRequestBytes, c.handlePostWorkspaceOAuthLoginCancel},
		{"complete", ref, proto.MaxProviderAuthRequestBytes, c.handlePostWorkspaceOAuthLoginComplete},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.request)
			require.NoError(t, err)
			valid := string(encoded)
			for _, body := range []string{`null`, `{}`, valid + `{}`, strings.Replace(valid, `"login_id":`, `"Login_ID":`, 1), strings.Replace(valid, `"workspace_id":"workspace"`, `"workspace_id":"other"`, 1), strings.Replace(valid, `"target":{`, `"target":{"private":"synthetic-private",`, 1), strings.Repeat(" ", test.maximum) + valid} {
				r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				r.SetPathValue("id", "workspace")
				w := httptest.NewRecorder()
				test.handler(w, r)
				require.Equal(t, 400, w.Code)
				require.Equal(t, "no-store", w.Header().Get("Cache-Control"))
				require.NotContains(t, w.Body.String(), "synthetic-private")
			}
		})
	}
}

func TestProviderOAuthRegisteredTLSRoutesRetainAuthenticationBoundary(t *testing.T) {
	host, clients := newRemoteAuthorityTLSHarness(t)
	for _, action := range []string{"begin", "bind", "code", "wait", "cancel", "complete"} {
		path := host.URL + "/v1/workspaces/missing/auth/oauth/" + action
		response, err := clients["unauthorized"].Post(path, "application/json", strings.NewReader(`{"input":"synthetic-private"}`))
		if response != nil {
			response.Body.Close()
		}
		require.Error(t, err, "unapproved actual TLS client cannot reach OAuth")
		response, err = clients["retained"].Post(path, "application/json", strings.NewReader(`{"input":"synthetic-private"}`))
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, 404, response.StatusCode, "registered workspace guard runs before body decoder")
		require.Contains(t, string(body), "workspace")
		require.NotContains(t, string(body), "synthetic-private")
	}
}

func TestProviderOAuthOversizedCompletionRetainsLoginAndProgress(t *testing.T) {
	ref := serverOAuthRef()
	response := proto.ProviderAuthenticationMutationResponse{Outcome: providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target, Progress: providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}}, Workspace: &proto.AuthenticationWorkspaceView{ID: strings.Repeat("x", proto.MaxProviderAuthResponseBytes)}}
	w := httptest.NewRecorder()
	writeProviderAuthMutationResponse(w, response)
	require.Equal(t, 500, w.Code)
	got, err := proto.DecodeProviderOAuthLoginCompleteResponse(w.Body.Bytes(), ref)
	require.NoError(t, err)
	require.Equal(t, response.Outcome, got.Outcome)
	require.Nil(t, got.Workspace)
	require.ErrorIs(t, got.Error, providerauth.ErrReceiptUnverified)
}
