package manifestflow

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func examplePluginFlow(t *testing.T, server *httptest.Server) (*Executor, manifest.Manifest) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	value.Capabilities.OAuth[0].ClientID = manifest.Template{Kind: "literal", Value: "example-client"}
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	for index := range value.Capabilities.Endpoints {
		endpoint := &value.Capabilities.Endpoints[index]
		switch endpoint.ID {
		case "authorize":
			endpoint.BaseURL = server.URL + "/authorize"
		case "token":
			endpoint.BaseURL = server.URL + "/token"
		default:
			continue
		}
		endpoint.AllowedSchemes = []string{target.Scheme}
		endpoint.AllowedHosts = []string{target.Hostname()}
		endpoint.FollowRedirects = false
	}
	executor, err := New(value, value.Capabilities.OAuth[0])
	require.NoError(t, err)
	executor.client = server.Client()
	return executor, value
}

func TestDeclarativeRefreshPreservesToken(t *testing.T) {
	var fields url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/token", request.URL.Path)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		fields, err = url.ParseQuery(string(body))
		require.NoError(t, err)
		_, _ = io.WriteString(response, `{"access_token":"new-access","expires_in":120}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)

	token, err := executor.Refresh(context.Background(), "old-refresh")
	require.NoError(t, err)
	require.Equal(t, "new-access", token.AccessToken)
	require.Equal(t, "old-refresh", token.RefreshToken)
	require.Equal(t, 120, token.ExpiresIn)
	require.Equal(t, "refresh_token", fields.Get("grant_type"))
	require.Equal(t, "old-refresh", fields.Get("refresh_token"))
}

func TestDeclarativeAuthorizationURLUsesScopesPKCEAndState(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	executor, value := examplePluginFlow(t, server)

	raw, err := executor.authorizationURL("http://localhost:4321/callback", "challenge", "state")
	require.NoError(t, err)
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	query := parsed.Query()
	require.Equal(t, "example-client", query.Get("client_id"))
	require.Equal(t, "code", query.Get("response_type"))
	require.Equal(t, "http://localhost:4321/callback", query.Get("redirect_uri"))
	require.Equal(t, "challenge", query.Get("code_challenge"))
	require.Equal(t, "S256", query.Get("code_challenge_method"))
	require.Equal(t, "state", query.Get("state"))
	require.Equal(t, strings.Join(value.Capabilities.OAuth[0].Scopes, " "), query.Get("scope"))
}

func TestDeclarativeDynamicLoopbackCompletesCodeExchange(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/token", request.URL.Path)
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		fields, err := url.ParseQuery(string(body))
		require.NoError(t, err)
		require.Equal(t, "authorization_code", fields.Get("grant_type"))
		require.Equal(t, "callback-code", fields.Get("code"))
		require.NotEmpty(t, fields.Get("code_verifier"))
		_, _ = io.WriteString(response, `{"access_token":"callback-access","refresh_token":"callback-refresh","expires_in":120}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	callbackResult := make(chan error, 1)

	token, err := executor.Authorize(context.Background(), func(raw string) error {
		authorization, parseErr := url.Parse(raw)
		if parseErr != nil {
			return parseErr
		}
		callback, parseErr := url.Parse(authorization.Query().Get("redirect_uri"))
		if parseErr != nil {
			return parseErr
		}
		query := callback.Query()
		query.Set("code", "callback-code")
		query.Set("state", authorization.Query().Get("state"))
		callback.RawQuery = query.Encode()
		go func() {
			request, requestErr := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
			if requestErr != nil {
				callbackResult <- requestErr
				return
			}
			response, requestErr := http.DefaultClient.Do(request)
			if requestErr == nil {
				_ = response.Body.Close()
			}
			callbackResult <- requestErr
		}()
		return nil
	}, nil)
	require.NoError(t, err)
	require.NoError(t, <-callbackResult)
	require.Equal(t, "callback-access", token.AccessToken)
	require.Equal(t, "callback-refresh", token.RefreshToken)
}

func TestParseHostedCallback(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, input, code, state, providerError string
		wantErr                                 bool
	}{
		{name: "bare code", input: " 4/0AX4Xf ", code: "4/0AX4Xf"},
		{name: "query", input: "code=abc&state=xyz", code: "abc", state: "xyz"},
		{name: "URL", input: "https://example.invalid/callback?code=abc&state=xyz", code: "abc", state: "xyz"},
		{name: "provider error", input: "https://example.invalid/callback?error=access_denied&state=xyz", state: "xyz", providerError: "access_denied"},
		{name: "empty", wantErr: true},
		{name: "missing code", input: "state=xyz", wantErr: true},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			code, state, providerError, err := parseHostedCallback(test.input)
			if test.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.code, code)
			require.Equal(t, test.state, state)
			require.Equal(t, test.providerError, providerError)
		})
	}
}

func TestTokenResponseRejectsOverflow(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(response, strings.Repeat("x", 65))
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.TokenResponse.MaxBodyBytes = 64

	_, err := executor.Refresh(context.Background(), "refresh")
	require.ErrorContains(t, err, "exceeds 64 bytes")
}

func TestTokenEndpointRejectsRedirect(t *testing.T) {
	var followed atomic.Bool
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			http.Redirect(response, request, "/final", http.StatusFound)
			return
		}
		followed.Store(true)
		_, _ = io.WriteString(response, `{"access_token":"unexpected"}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)

	_, err := executor.Refresh(context.Background(), "refresh")
	require.Error(t, err)
	require.False(t, followed.Load())
}
