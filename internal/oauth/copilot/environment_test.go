package copilot

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestCodebaseIndexOAuthAndIdentity(t *testing.T) {
	original := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = original })
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"COPILOT_ADVERTISE_MODE=vscode", "COPILOT_VSCODE_EXTENSION_VERSION=1.2.3"})
	http.DefaultTransport = copilotRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		body := ""
		switch r.URL.Path {
		case "/login/device/code":
			require.NoError(t, r.ParseForm())
			require.Equal(t, codebaseIndexClientID, r.Form.Get("client_id"))
			body = `{"device_code":"synthetic-device","user_code":"ABCD","expires_in":120}`
		case "/login/oauth/access_token":
			require.NoError(t, r.ParseForm())
			require.Equal(t, codebaseIndexClientID, r.Form.Get("client_id"))
			body = `{"access_token":"synthetic-github"}`
		case "/user":
			require.Equal(t, "api.github.com", r.URL.Host)
			require.Equal(t, "Bearer synthetic-github", r.Header.Get("Authorization"))
			body = `{"id":42,"login":"example"}`
		default:
			t.Fatalf("unexpected OAuth destination: %s", r.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})
	device, err := RequestCodebaseIndexDeviceCode(ctx)
	require.NoError(t, err)
	token, err := tryGetGitHubTokenForClient(ctx, device.DeviceCode, codebaseIndexClientID)
	require.NoError(t, err)
	require.Equal(t, "synthetic-github", token.AccessToken)
	require.Empty(t, token.RefreshToken)
	id, name, _ := GitHubIdentity(ctx, token.AccessToken)
	require.Equal(t, "42", id)
	require.Equal(t, "example", name)
}

func TestDeviceAndPollRequestsUseCapturedIdentity(t *testing.T) {
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"COPILOT_ADVERTISE_MODE=vscode", "COPILOT_VSCODE_EXTENSION_VERSION=1.2.3", "COPILOT_VSCODE_INTEGRATION_ID=captured-integration", "COPILOT_VSCODE_EDITOR_VERSION=captured-editor", "COPILOT_VSCODE_EDITOR_PLUGIN_VERSION=captured-plugin"})
	t.Setenv("COPILOT_ADVERTISE_MODE", "cli")
	t.Setenv("COPILOT_CLI_VERSION", "9.9.9")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "9.9.9")
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "GitHubCopilotChat/1.2.3", r.Header.Get("User-Agent"))
		switch r.URL.Path {
		case "/login/device/code":
			_, _ = io.WriteString(w, `{"device_code":"synthetic-device","user_code":"ABCD","verification_uri":"https://example.invalid/device","expires_in":120,"interval":5}`)
		case "/login/oauth/access_token":
			_, _ = io.WriteString(w, `{"error":"authorization_pending"}`)
		default:
			t.Errorf("unexpected request path %s", r.URL.Path)
		}
	}))
	defer server.Close()
	originalTransport, originalClient := http.DefaultTransport, http.DefaultClient
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := copilotRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "github.com", r.URL.Host)
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})
	http.DefaultTransport = transport
	http.DefaultClient = &http.Client{Transport: transport}
	defer func() { http.DefaultTransport = originalTransport; http.DefaultClient = originalClient }()
	device, err := RequestDeviceCode(ctx)
	require.NoError(t, err)
	require.Equal(t, "ABCD", device.UserCode)
	_, err = tryGetToken(ctx, device.DeviceCode)
	require.ErrorIs(t, err, errPending)
	headers, err := HeadersForContext(ctx)
	require.NoError(t, err)
	require.Equal(t, "GitHubCopilotChat/1.2.3", headers["User-Agent"])
	require.Equal(t, "captured-integration", headers["Copilot-Integration-Id"])
	require.Equal(t, "captured-editor", headers["Editor-Version"])
	require.Equal(t, "captured-plugin", headers["Editor-Plugin-Version"])
	require.EqualValues(t, 2, calls.Load())
}
