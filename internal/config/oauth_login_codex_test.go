package config

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

type oauthLoginRoundTripFunc func(*http.Request) (*http.Response, error)

func (f oauthLoginRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOAuthLoginRealCodexChallengeHTTPSAndScopedPublication(t *testing.T) {
	for _, validState := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid state", true: "exchange and publish"}[validState], func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeWorkspace, false)
			values := environmentValues(f.store.baseEnvironment)
			values["CODEX_OAUTH_CLIENT_ID"] = "synthetic-captured-client"
			var err error
			f.store, err = LoadIsolated(f.root, filepath.Join(f.root, "workspace-data"), false, env.NewFromMap(values))
			require.NoError(t, err)
			var found bool
			f.owner, found = f.store.RuntimeSnapshot().ProviderOwner("codex")
			require.True(t, found)
			before := f.capture(t)
			prep, err := f.store.PrepareOAuthLogin(t.Context(), before, f.owner)
			require.NoError(t, err)
			require.Equal(t, &oauth.CallbackRequirement{Mode: "loopback-fixed", Port: 1455, Path: "/auth/callback"}, prep.CallbackRequirement())
			code, err := f.store.PrepareOAuthCodeChallenge(t.Context(), prep, 1455)
			require.NoError(t, err)
			defer code.Close()
			authorization, err := url.Parse(code.AuthorizationURL())
			require.NoError(t, err)
			query := authorization.Query()
			require.Equal(t, "https://auth.openai.com/oauth/authorize", authorization.Scheme+"://"+authorization.Host+authorization.Path)
			require.Equal(t, "synthetic-captured-client", query.Get("client_id"))
			require.Equal(t, "http://localhost:1455/auth/callback", query.Get("redirect_uri"))
			require.NotEmpty(t, query.Get("code_challenge"))
			require.False(t, code.ExpiresAt().IsZero())
			t.Setenv("CODEX_OAUTH_CLIENT_ID", "synthetic-ambient-replacement")
			claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"synthetic-account-c"}}`))
			access := "e30." + claims + ".synthetic-signature"
			var exchanges, identities atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/oauth/token":
					exchanges.Add(1)
					require.NoError(t, r.ParseForm())
					require.Equal(t, "authorization_code", r.Form.Get("grant_type"))
					require.Equal(t, "synthetic-captured-client", r.Form.Get("client_id"))
					require.Equal(t, "synthetic-code", r.Form.Get("code"))
					require.Equal(t, "http://localhost:1455/auth/callback", r.Form.Get("redirect_uri"))
					digest := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
					require.Equal(t, query.Get("code_challenge"), base64.RawURLEncoding.EncodeToString(digest[:]))
					_ = json.NewEncoder(w).Encode(map[string]any{"access_token": access, "refresh_token": "synthetic-refresh", "expires_in": 120})
				case "/api/accounts/v1/user-auth-credential/whoami":
					identities.Add(1)
					require.Equal(t, "Bearer "+access, r.Header.Get("Authorization"))
					_ = json.NewEncoder(w).Encode(map[string]string{"email": "synthetic@example.invalid"})
				default:
					t.Errorf("unexpected synthetic OAuth path %q", r.URL.Path)
					http.Error(w, "unexpected path", http.StatusNotFound)
				}
			}))
			defer host.Close()
			target, err := url.Parse(host.URL)
			require.NoError(t, err)
			original := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: oauthLoginRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Scheme != "https" || r.URL.Host != "auth.openai.com" {
					return nil, errors.New("unexpected outbound OAuth endpoint")
				}
				copy := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = target.Scheme, target.Host
				copy.URL = &address
				return host.Client().Transport.RoundTrip(copy)
			})}
			defer func() { http.DefaultClient = original }()
			input := url.Values{"state": {query.Get("state")}, "code": {"synthetic-code"}}
			if !validState {
				input.Set("state", "synthetic-wrong-state")
			}
			authorized, err := f.store.ExchangeOAuthCode(t.Context(), code, input.Encode())
			if !validState {
				require.Error(t, err)
				require.Nil(t, authorized.state)
				require.Zero(t, exchanges.Load())
				require.Zero(t, identities.Load())
				require.True(t, before.SameObservation(f.capture(t)))
				return
			}
			require.NoError(t, err)
			require.True(t, before.SameObservation(f.capture(t)), "code exchange alone cannot save accounts or configuration")
			replayed, err := f.store.ExchangeOAuthCode(context.Background(), code, input.Encode())
			require.NoError(t, err)
			require.Same(t, authorized.state, replayed.state)
			result, err := f.store.CommitOAuthLogin(t.Context(), f.scope, authorized)
			require.NoError(t, err)
			require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
			require.True(t, result.After.SameObservation(f.capture(t)))
			provider, ok := f.store.Config().Providers.Get("codex")
			require.True(t, ok)
			require.Equal(t, access, provider.APIKey)
			require.Equal(t, "synthetic-refresh", provider.OAuthToken.RefreshToken)
			account, captured, err := result.After.runtime.CapturedConstructionAccount(f.owner)
			require.NoError(t, err)
			require.True(t, captured)
			require.Equal(t, "synthetic@example.invalid", account.ID)
			require.JSONEq(t, `{"account_id":"synthetic-account-c"}`, string(account.Raw))
			require.Equal(t, int32(1), exchanges.Load())
			require.Equal(t, int32(1), identities.Load())
		})
	}
}
