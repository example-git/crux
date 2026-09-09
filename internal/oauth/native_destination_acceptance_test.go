package oauth_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type nativeDestinationRoundTrip func(*http.Request) (*http.Response, error)

func (f nativeDestinationRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestNativeCredentialDestinationsThroughHTTPS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "AI_CLI_DIR", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME"} {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(path, 0o700))
		t.Setenv(name, path)
	}
	values := []string{"CODEX_OAUTH_CLIENT_ID=synthetic-client", "CODEX_CLI_VERSION=1.2.3", "CODEX_ORIGINATOR=synthetic-originator", "GEMINI_OAUTH_CLIENT_ID=synthetic-client", "GEMINI_OAUTH_CLIENT_SECRET=synthetic-client-secret", "ANTIGRAVITY_CLI_VERSION=1.2.3", "COPILOT_CLI_VERSION=1.2.3", "COPILOT_ADVERTISE_MODE=cli"}
	for _, value := range values {
		key, value, _ := strings.Cut(value, "=")
		t.Setenv(key, value)
	}
	tokenResult := func(token *oauth.Token, err error) (string, error) {
		if err != nil || token == nil {
			return "", err
		}
		return token.AccessToken, nil
	}
	for _, caller := range []struct {
		name, endpoint, method, bodyMarker, authorization, want string
		optional                                                bool
		call                                                    func(context.Context) (string, error)
	}{
		{"codex-code", "https://auth.openai.com/oauth/token", "POST", "code=synthetic-code", "", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(codex.ExchangeCode(ctx, "synthetic-code", "synthetic-verifier", "http://127.0.0.1:1455/auth/callback"))
		}},
		{"codex-refresh", "https://auth.openai.com/oauth/token", "POST", "refresh_token=synthetic-refresh", "", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(codex.RefreshToken(ctx, "synthetic-refresh"))
		}},
		{"codex-account", "https://auth.openai.com/api/accounts/v1/user-auth-credential/whoami", "GET", "", "Bearer synthetic-access", "synthetic@example.invalid", true, func(ctx context.Context) (string, error) { return codex.AccountEmail(ctx, "synthetic-access"), nil }},
		{"gemini-code", "https://oauth2.googleapis.com/token", "POST", "client_secret=synthetic-client-secret", "", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(gemini.ExchangeCode(ctx, "synthetic-code", "synthetic-verifier"))
		}},
		{"gemini-refresh", "https://oauth2.googleapis.com/token", "POST", "refresh_token=synthetic-refresh", "", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(gemini.Refresh(ctx, "synthetic-refresh"))
		}},
		{"gemini-project", "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist", "POST", "{}", "Bearer synthetic-access", "synthetic-project", true, func(ctx context.Context) (string, error) {
			return gemini.ProjectForCredential(ctx, "synthetic-access"), nil
		}},
		{"gemini-account", "https://www.googleapis.com/oauth2/v2/userinfo", "GET", "", "Bearer synthetic-access", "synthetic@example.invalid", true, func(ctx context.Context) (string, error) { return gemini.AccountEmail(ctx, "synthetic-access"), nil }},
		{"copilot-device", "https://github.com/login/device/code", "POST", "client_id=", "", "synthetic-device", false, func(ctx context.Context) (string, error) {
			value, err := copilot.RequestDeviceCode(ctx)
			if err != nil {
				return "", err
			}
			return value.DeviceCode, nil
		}},
		{"copilot-poll", "https://github.com/login/oauth/access_token", "POST", "device_code=synthetic-device", "", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(copilot.PollForToken(ctx, &copilot.DeviceCode{DeviceCode: "synthetic-device", ExpiresIn: 30, Interval: 5}))
		}},
		{"copilot-refresh", "https://api.github.com/copilot_internal/v2/token", "GET", "", "Bearer synthetic-access", "synthetic-access", false, func(ctx context.Context) (string, error) {
			return tokenResult(copilot.RefreshToken(ctx, "synthetic-access"))
		}},
		{"codex-usage", "https://chatgpt.com/backend-api/wham/usage", "GET", "", "Bearer synthetic-access", "synthetic-plan", false, func(ctx context.Context) (string, error) {
			value, err := usage.FetchWithTokenForOwner(ctx, "codex", "synthetic-access", usage.FetchCodex, nil)
			if err != nil {
				return "", err
			}
			return value.Plan, nil
		}},
		{"copilot-usage", "https://api.github.com/copilot_internal/user", "GET", "", "Bearer synthetic-access", "synthetic-plan", false, func(ctx context.Context) (string, error) {
			value, err := usage.FetchWithTokenForOwner(ctx, "copilot", "synthetic-access", usage.FetchCopilot, nil)
			if err != nil {
				return "", err
			}
			return value.Plan, nil
		}},
	} {
		for _, status := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			for _, mode := range []string{"relative", "other-port", "other-host", "owner-changed"} {
				t.Run(fmt.Sprintf("%s/%d/%s", caller.name, status, mode), func(t *testing.T) {
					var starts, accepted, escaped, tokenExchanges atomic.Int32
					var ownerChanged atomic.Bool
					original, err := url.Parse(caller.endpoint)
					require.NoError(t, err)
					check := func(r *http.Request) {
						assert.Equal(t, caller.method, r.Method)
						assert.Equal(t, caller.authorization, r.Header.Get("Authorization"))
						body, err := io.ReadAll(r.Body)
						assert.NoError(t, err)
						if caller.bodyMarker != "" {
							assert.Contains(t, string(body), caller.bodyMarker)
						}
						assert.NotEmpty(t, r.Header.Get("User-Agent"))
					}
					sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { escaped.Add(1); _, _ = io.WriteString(w, `{}`) }))
					defer sink.Close()
					sinkURL, err := url.Parse(sink.URL)
					require.NoError(t, err)
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if caller.name == "copilot-poll" && r.URL.Path == "/copilot_internal/v2/token" {
							tokenExchanges.Add(1)
							assert.Equal(t, http.MethodGet, r.Method)
							assert.Equal(t, "Bearer synthetic-access", r.Header.Get("Authorization"))
							_, _ = io.WriteString(w, `{"token":"synthetic-access","expires_at":2000000000}`)
							return
						}
						check(r)
						if r.URL.Path == "/accepted" {
							accepted.Add(1)
							assert.Equal(t, "fixed", r.URL.Query().Get("source"))
							w.Header().Set("Content-Type", "application/json")
							_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-next","expires_in":3600,"email":"synthetic@example.invalid","cloudaicompanionProject":"synthetic-project","token":"synthetic-access","expires_at":2000000000,"device_code":"synthetic-device","user_code":"CODE","verification_uri":"https://example.invalid/device","interval":5,"plan_type":"synthetic-plan","copilot_plan":"synthetic-plan"}`)
							return
						}
						starts.Add(1)
						location := "/accepted?source=fixed"
						switch mode {
						case "other-port":
							location = "https://" + original.Hostname() + ":" + sinkURL.Port() + "/accepted"
						case "other-host":
							location = sink.URL + "/accepted"
						case "owner-changed":
							ownerChanged.Store(true)
						}
						http.Redirect(w, r, location, status)
					}))
					defer server.Close()
					serverURL, err := url.Parse(server.URL)
					require.NoError(t, err)
					// Only dial the two loopback TLS fixtures. The request retains its
					// production URL until RoundTrip, so net/http applies the actual
					// fixed-origin redirect policy before this transport is invoked.
					transport := nativeDestinationRoundTrip(func(r *http.Request) (*http.Response, error) {
						target := serverURL
						if r.URL.Hostname() != original.Hostname() || r.URL.Port() != original.Port() {
							target = sinkURL
						}
						if caller.name == "copilot-poll" && r.URL.String() == "https://api.github.com/copilot_internal/v2/token" {
							target = serverURL
						}
						clone := r.Clone(r.Context())
						address := *r.URL
						address.Scheme, address.Host = target.Scheme, target.Host
						clone.URL = &address
						return server.Client().Transport.RoundTrip(clone)
					})
					previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
					http.DefaultTransport, http.DefaultClient = transport, &http.Client{Transport: transport}
					defer func() { http.DefaultTransport, http.DefaultClient = previousTransport, previousClient }()
					ctx := oauth.ContextWithEnvironment(t.Context(), values)
					ctx = providertransport.ContextWithOwnerValidator(ctx, func() error {
						if ownerChanged.Load() {
							return fmt.Errorf("synthetic owner changed")
						}
						return nil
					})
					value, err := caller.call(ctx)
					if mode == "relative" {
						require.NoError(t, err)
						require.Equal(t, caller.want, value)
						require.EqualValues(t, 1, accepted.Load())
						if caller.name == "copilot-poll" {
							require.EqualValues(t, 1, tokenExchanges.Load())
						}
					} else {
						require.Empty(t, value)
						if !caller.optional {
							if mode == "owner-changed" {
								require.ErrorContains(t, err, "synthetic owner changed")
							} else {
								require.ErrorContains(t, err, "provider redirect refused")
							}
						}
						require.Zero(t, accepted.Load())
						require.Zero(t, tokenExchanges.Load())
					}
					require.EqualValues(t, 1, starts.Load())
					require.Zero(t, escaped.Load(), "no credential, form body or identity header may reach an undeclared destination")
				})
			}
		}
	}
}
