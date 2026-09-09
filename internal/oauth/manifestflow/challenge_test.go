package manifestflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestPrepareCodeCapturesManifestAndClientBoundCallback(t *testing.T) {
	var calls atomic.Int32
	var form url.Values
	var header string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/token", r.URL.Path)
		require.NoError(t, r.ParseForm())
		form = r.Form
		header = r.Header.Get("X-Captured")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","expires_in":120}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.Redirect.CallbackPath = "/callback%2Fpart"
	executor.flow.TokenRequest.Code = append(executor.flow.TokenRequest.Code, manifest.FieldRule{Name: "redirect_uri", Value: manifest.Template{Kind: "context", Ref: "oauth.redirect_uri"}})
	executor.flow.ClientID = manifest.Template{Kind: "config", Ref: "client_id"}
	secret := manifest.Template{Kind: "credential", Ref: "client_secret"}
	executor.flow.ClientSecret = &secret
	headerValue := manifest.Template{Kind: "config", Ref: "header"}
	executor.flow.TokenRequest.Headers = []manifest.HeaderRule{{Operation: "set", Name: "X-Captured", Value: &headerValue}}
	executor.bindings = Bindings{Configuration: map[string]any{"client_id": "captured-client", "header": "captured-header"}, Credentials: map[string]string{"client_secret": "captured-secret"}}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	defer listener.Close()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	challenge, err := executor.PrepareCode(t.Context(), port)
	require.NoError(t, err)
	defer challenge.Close()
	u, err := url.Parse(challenge.AuthorizationURL())
	require.NoError(t, err)
	redirect := "http://localhost:" + strconv.Itoa(int(port)) + "/callback%2Fpart"
	require.Equal(t, redirect, u.Query().Get("redirect_uri"))
	require.Equal(t, "captured-client", u.Query().Get("client_id"))
	require.Zero(t, calls.Load())
	executor.bindings.Configuration["client_id"] = "changed-client"
	executor.bindings.Configuration["header"] = "changed-header"
	executor.bindings.Credentials["client_secret"] = "changed-secret"
	executor.flow.TokenRequest.Code = nil
	executor.flow.Scopes[0] = "changed-scope"
	executor.flow.ClientSecret.Value = "changed-secret"
	executor.endpoints["token"] = manifest.Endpoint{BaseURL: "https://changed.invalid"}
	input := url.Values{"code": {"synthetic-code"}, "state": {u.Query().Get("state")}}.Encode()
	token, err := challenge.Exchange(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, "synthetic-access", token.AccessToken)
	require.Equal(t, "captured-client", form.Get("client_id"))
	require.Equal(t, "captured-secret", form.Get("client_secret"))
	require.Equal(t, redirect, form.Get("redirect_uri"))
	require.Equal(t, "captured-header", header)
	digest := sha256.Sum256([]byte(form.Get("code_verifier")))
	require.Equal(t, u.Query().Get("code_challenge"), base64.RawURLEncoding.EncodeToString(digest[:]))
	_, err = challenge.Exchange(t.Context(), input)
	require.NoError(t, err)
	_, err = challenge.Exchange(t.Context(), input+"&extra=conflict")
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestPrepareCodeManifestModesAndInvalidInput(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":120}`)
	}))
	defer server.Close()
	for _, mode := range []string{"loopback-fixed", "loopback-dynamic", "hosted-paste"} {
		t.Run(mode, func(t *testing.T) {
			executor, _ := examplePluginFlow(t, server)
			executor.flow.Redirect.Mode = mode
			port := uint16(12345)
			badPort := uint16(0)
			switch mode {
			case "loopback-fixed":
				executor.flow.Redirect.Port = int(port)
			case "hosted-paste":
				port = 0
				badPort = 1
				executor.flow.Redirect.URI = "https://hosted.invalid/paste"
			}
			_, err := executor.PrepareCode(t.Context(), badPort)
			require.Error(t, err)
			for _, inputMode := range []string{"valid", "state", "duplicate", "malformed", "owner", "provider-error"} {
				t.Run(inputMode, func(t *testing.T) {
					before := calls.Load()
					var changed atomic.Bool
					ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
						if changed.Load() {
							return errors.New("owner changed")
						}
						return nil
					})
					challenge, err := executor.PrepareCode(ctx, port)
					require.NoError(t, err)
					defer challenge.Close()
					require.Equal(t, before, calls.Load())
					u, err := url.Parse(challenge.AuthorizationURL())
					require.NoError(t, err)
					q := url.Values{"code": {"synthetic-code"}, "state": {u.Query().Get("state")}}
					switch inputMode {
					case "state":
						q.Set("state", "wrong")
					case "duplicate":
						q.Add("code", "second")
					case "owner":
						changed.Store(true)
					case "provider-error":
						q.Set("error", "access_denied")
					}
					input := q.Encode()
					if inputMode == "malformed" {
						input += "&bad=%zz"
					}
					if mode == "hosted-paste" {
						input = "https://hosted.invalid/paste?" + input
					}
					token, err := challenge.Exchange(t.Context(), input)
					if inputMode == "valid" {
						require.NoError(t, err)
						require.NotNil(t, token)
						require.Equal(t, before+1, calls.Load())
					} else {
						require.Error(t, err)
						require.Nil(t, token)
						require.Equal(t, before, calls.Load())
					}
					_, again := challenge.Exchange(t.Context(), input)
					require.Equal(t, err, again)
				})
			}
		})
	}
}

func TestPrepareCodeManifestRejectsRedirectReplacement(t *testing.T) {
	server := httptest.NewTLSServer(http.NotFoundHandler())
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.AuthorizationParams = append(executor.flow.AuthorizationParams, manifest.QueryRule{Name: "redirect_uri", Value: manifest.Template{Kind: "literal", Value: "https://other.invalid/callback"}})
	_, err := executor.PrepareCode(t.Context(), 12345)
	require.ErrorContains(t, err, "replaced the captured callback")
}

func TestManifestChallengeCloseCancelsActualExchange(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	executor, _ := examplePluginFlow(t, server)
	challenge, err := executor.PrepareCode(t.Context(), 12345)
	require.NoError(t, err)
	defer challenge.Close()
	u, err := url.Parse(challenge.AuthorizationURL())
	require.NoError(t, err)
	input := url.Values{"code": {"code"}, "state": {u.Query().Get("state")}}.Encode()
	done := make(chan error, 1)
	go func() { _, err := challenge.Exchange(t.Context(), input); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("token request did not start")
	}
	challenge.Close()
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("closed challenge left token request active")
	}
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestManifestLegacyCallbackMatchesExactEscapedPath(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":120}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.Redirect.CallbackPath = "/callback%2Fpart"
	callbacks := make(chan error, 1)
	token, err := executor.Authorize(t.Context(), func(raw string) error {
		callback, err := url.Parse(callbackURLForTest(t, raw))
		if err != nil {
			return err
		}
		go func() {
			browser := &http.Client{Timeout: 3 * time.Second}
			wrong := *callback
			wrong.RawPath = ""
			response, err := browser.Get(wrong.String())
			if err != nil {
				callbacks <- err
				return
			}
			status := response.StatusCode
			response.Body.Close()
			if status != http.StatusNotFound {
				callbacks <- errors.New("decoded callback path was accepted")
				return
			}
			response, err = browser.Get(callback.String())
			if response != nil {
				response.Body.Close()
			}
			callbacks <- err
		}()
		return nil
	}, nil)
	require.NoError(t, err)
	require.NoError(t, <-callbacks)
	require.Equal(t, "synthetic-access", token.AccessToken)
	require.EqualValues(t, 1, calls.Load())
}
