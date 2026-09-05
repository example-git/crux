package manifestflow

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
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

func TestDeclarativeRequestsRejectOwnerReplacementBeforeDispatch(t *testing.T) {
	var dispatched atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		dispatched.Add(1)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed")
	})

	_, err := executor.Refresh(ctx, "refresh-token")
	require.ErrorContains(t, err, "owner changed")
	_, err = executor.deviceRequest(ctx, "token", nil, nil, map[string]string{}, 1024)
	require.ErrorContains(t, err, "owner changed")
	require.Zero(t, dispatched.Load())
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

func TestDeclarativeOAuthTemplatesUseBoundConfigurationAndCredentials(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	_, value := examplePluginFlow(t, server)
	value.Capabilities.OAuth[0].ClientID = manifest.Template{Kind: "config", Ref: "oauth_client_id"}
	executor, err := New(value, value.Capabilities.OAuth[0], Bindings{
		Configuration: map[string]any{"oauth_client_id": "configured-client"},
		Credentials:   map[string]string{"client_secret": "configured-secret"},
	})
	require.NoError(t, err)

	raw, err := executor.authorizationURL("http://localhost:4321/callback", "challenge", "state")
	require.NoError(t, err)
	parsed, err := url.Parse(raw)
	require.NoError(t, err)
	require.Equal(t, "configured-client", parsed.Query().Get("client_id"))
	secret, err := executor.eval(manifest.Template{Kind: "credential", Ref: "client_secret"}, nil)
	require.NoError(t, err)
	require.Equal(t, "configured-secret", secret)
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
		require.Equal(t, "localhost", callback.Hostname())
		require.NotEmpty(t, callback.Port())
		require.NotEqual(t, "0", callback.Port())
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

func TestDeclarativeDeviceCodeFlowRequestsAndPolls(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/device":
			_, _ = io.WriteString(response, `{"device_code":"device-secret","user_code":"ABCD-EFGH","verification_uri":"https://example.invalid/device","expires_in":120,"interval":1}`)
		case "/token":
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			fields, err := url.ParseQuery(string(body))
			require.NoError(t, err)
			require.Equal(t, "device-secret", fields.Get("device_code"))
			_, _ = io.WriteString(response, `{"access_token":"device-access","refresh_token":"device-refresh","expires_in":120}`)
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	_, value := examplePluginFlow(t, server)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	value.Capabilities.Endpoints = append(value.Capabilities.Endpoints, manifest.Endpoint{
		ID: "device", BaseURL: server.URL + "/device",
		AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}, Override: "forbidden",
	})
	flow := &value.Capabilities.OAuth[0]
	flow.Redirect = manifest.OAuthRedirect{Mode: "device-code"}
	flow.PKCE = "disabled"
	flow.DeviceCode = &manifest.DeviceCodeFlow{
		Endpoint: "device",
		Request: []manifest.FieldRule{
			{Name: "client_id", Value: manifest.Template{Kind: "context", Ref: "oauth.client_id"}},
		},
		DeviceCodePointer: "/device_code", UserCodePointer: "/user_code", VerificationURLPointer: "/verification_uri",
		ExpiresInPointer: "/expires_in", IntervalPointer: "/interval", DefaultIntervalSeconds: 1,
		Poll: []manifest.FieldRule{
			{Name: "device_code", Value: manifest.Template{Kind: "context", Ref: "oauth.device_code"}},
		},
		ErrorPointer: "/error", MaxBodyBytes: 1024,
	}
	executor, err := New(value, *flow)
	require.NoError(t, err)
	executor.client = server.Client()

	authorization, err := executor.RequestDeviceCode(t.Context())
	require.NoError(t, err)
	require.Equal(t, "ABCD-EFGH", authorization.UserCode)
	require.Equal(t, "https://example.invalid/device", authorization.VerificationURL)
	authorization.state.interval = 0
	token, err := executor.PollDeviceCode(t.Context(), authorization)
	require.NoError(t, err)
	require.Equal(t, "device-access", token.AccessToken)
	require.Equal(t, "device-refresh", token.RefreshToken)
}

func TestDeclarativeTokenAuthenticationStyles(t *testing.T) {
	for _, test := range []struct {
		name      string
		authStyle string
		basic     bool
	}{
		{name: "parameters", authStyle: "params"},
		{name: "HTTP basic", authStyle: "basic", basic: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				require.NoError(t, request.ParseForm())
				if test.basic {
					clientID, clientSecret, ok := request.BasicAuth()
					require.True(t, ok)
					require.Equal(t, "example-client", clientID)
					require.Equal(t, "example-secret", clientSecret)
					require.Empty(t, request.Form.Get("client_id"))
					require.Empty(t, request.Form.Get("client_secret"))
				} else {
					require.Equal(t, "example-client", request.Form.Get("client_id"))
					require.Equal(t, "example-secret", request.Form.Get("client_secret"))
					require.Empty(t, request.Header.Get("Authorization"))
				}
				_, _ = io.WriteString(response, `{"access_token":"access","refresh_token":"refresh","expires_in":120}`)
			}))
			defer server.Close()
			executor, _ := examplePluginFlow(t, server)
			executor.flow.ClientSecret = &manifest.Template{Kind: "literal", Value: "example-secret"}
			executor.flow.TokenRequest.AuthStyle = test.authStyle

			token, err := executor.Refresh(t.Context(), "old-refresh")
			require.NoError(t, err)
			require.Equal(t, "access", token.AccessToken)
		})
	}
}

func TestDeclarativeRefreshUsesDeclaredScopes(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.NoError(t, request.ParseForm())
		require.Equal(t, "refresh-one refresh-two", request.Form.Get("scope"))
		_, _ = io.WriteString(response, `{"access_token":"access","refresh_token":"refresh","expires_in":120}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.RefreshScopes = []string{"refresh-one", "refresh-two"}
	executor.flow.TokenRequest.Refresh = append(executor.flow.TokenRequest.Refresh, manifest.FieldRule{
		Name: "scope", Value: manifest.Template{Kind: "context", Ref: "oauth.scopes"},
	})

	_, err := executor.Refresh(t.Context(), "old-refresh")
	require.NoError(t, err)
}

func TestDeclarativeGeneratedTemplates(t *testing.T) {
	executor := &Executor{}
	generatedUUID, err := executor.eval(manifest.Template{Kind: "uuid"}, nil)
	require.NoError(t, err)
	require.Len(t, generatedUUID, 36)

	unixTime, err := executor.eval(manifest.Template{Kind: "unix-time"}, nil)
	require.NoError(t, err)
	_, err = strconv.ParseInt(unixTime, 10, 64)
	require.NoError(t, err)

	randomHex, err := executor.eval(manifest.Template{Kind: "random-hex", Bytes: 16}, nil)
	require.NoError(t, err)
	decoded, err := hex.DecodeString(randomHex)
	require.NoError(t, err)
	require.Len(t, decoded, 16)
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
