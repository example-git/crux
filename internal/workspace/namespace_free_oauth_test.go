package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNamespaceFreeWorkspaceOAuthThroughTLS(t *testing.T) {
	for _, mode := range []string{"fresh", "expired", "unauthorized", "manual", "lost-login-response", "lost-refresh-response"} {
		t.Run(mode, func(t *testing.T) {
			var logins, refreshes, oldRequests, freshRequests atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NotNil(t, r.TLS)
				switch r.URL.Path {
				case "/token":
					require.NoError(t, r.ParseForm())
					assert.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					w.Header().Set("Content-Type", "application/json")
					if r.Form.Get("grant_type") == "refresh_token" {
						refreshes.Add(1)
						assert.Equal(t, "namespace-old-refresh", r.Form.Get("refresh_token"))
						_, _ = io.WriteString(w, `{"access_token":"namespace-fresh-access","refresh_token":"namespace-fresh-refresh","expires_in":3600}`)
					} else {
						logins.Add(1)
						assert.Equal(t, "workspace-code", r.Form.Get("code"))
						assert.NotEmpty(t, r.Form.Get("code_verifier"))
						expires := 3600
						if mode == "expired" {
							expires = 1
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "namespace-old-access", "refresh_token": "namespace-old-refresh", "expires_in": expires})
					}
				case "/v1/responses":
					_, _ = io.Copy(io.Discard, r.Body)
					switch r.Header.Get("Authorization") {
					case "Bearer namespace-old-access":
						oldRequests.Add(1)
						if mode == "unauthorized" || mode == "lost-refresh-response" {
							w.Header().Set("Content-Type", "application/json")
							w.WriteHeader(http.StatusUnauthorized)
							_, _ = io.WriteString(w, `{"error":{"message":"expired fixture token","type":"authentication_error"}}`)
							return
						}
					case "Bearer namespace-fresh-access":
						freshRequests.Add(1)
					default:
						t.Error("unaccepted credential reached the namespace-free provider")
						http.Error(w, "unexpected credential", http.StatusForbidden)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"namespace-json","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"namespace accepted","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				default:
					http.NotFound(w, r)
				}
			}))
			defer host.Close()
			f := newWorkspaceOAuthFixture(t, host, "hosted-paste", func(value *manifest.Manifest) {
				value.Provider.AccountNamespace = ""
				value.Capabilities.Operations[0].Retry.Authentication = "refresh-once"
			})
			require.Empty(t, f.namespace)
			accountPath := filepath.Join(f.root, "accounts", "accounts.json")
			unrelated := []byte(`{"active":{"unrelated":"kept"},"accounts":{"unrelated":[{"id":"kept","accessToken":"unrelated-private","vendor":{"number":1.0}}]},"foreign":{"large":9007199254740993}}`)
			require.NoError(t, os.MkdirAll(filepath.Dir(accountPath), 0o700))
			require.NoError(t, os.WriteFile(accountPath, unrelated, 0o600))
			state, err := f.w.ProviderAuthentication(t.Context())
			require.NoError(t, err)
			f.ref.Target.Generation = state.Generation
			serverInfos, serverFiles := clientAuthenticationFiles(t, f.transport.path, f.transport.accountsPath)
			originalAuthority := *f.w.ws.Authority
			authorizeWorkspaceOAuth(t, f, "hosted-paste")
			if mode == "lost-login-response" {
				f.transport.putMode.Store(2)
				f.transport.getMode.Store(1)
			}
			outcome, err := f.w.CompleteProviderOAuthLogin(t.Context(), f.ref)
			if mode == "lost-login-response" {
				require.Error(t, err)
				f.transport.putMode.Store(0)
				f.transport.getMode.Store(0)
				replayed, err := f.w.CompleteProviderOAuthLogin(t.Context(), f.ref)
				require.NoError(t, err)
				require.Equal(t, outcome, replayed)
			} else {
				require.NoError(t, err)
			}
			require.NoError(t, outcome.ValidateOAuthLogin(f.ref))
			require.Equal(t, providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}, outcome.Progress)
			require.Len(t, f.w.authority.accepted.Credentials, 1)
			binding := f.w.authority.accepted.Credentials[0]
			require.Nil(t, binding.Account)
			require.NotNil(t, binding.OAuthToken)
			require.Empty(t, binding.APIKey)
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			if mode == "lost-login-response" {
				_, err := f.w.client.SubscribeEvents(ctx, f.w.workspaceID(), originalAuthority)
				require.ErrorContains(t, err, "different accepted runtime", "a lost response must not make the original attachment current")
				require.Greater(t, f.w.ws.Authority.Revision, originalAuthority.Revision)
			}
			// Use the same accepted-authority attachment path as runSubscription.
			// Discovery intentionally does not update Client's creation cache.
			events, err := f.w.subscribeAcceptedEvents(f.w.subCtx)
			require.NoError(t, err)
			done := make(chan struct{})
			go func() {
				defer close(done)
				for event := range events {
					f.w.HandleClientRefreshEvent(ctx, event)
				}
			}()
			defer func() {
				cancel()
				f.w.subCancel()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Error("namespace-free accepted event stream did not stop")
				}
			}()
			require.NoError(t, f.w.InitCoderAgentNonInteractive(ctx))
			if mode == "manual" {
				require.NoError(t, f.w.RefreshOAuthToken(ctx, config.ScopeGlobal, binding.Owner))
			}
			if mode == "lost-refresh-response" {
				f.transport.putMode.Store(2)
			}
			receiver, err := f.transport.s.Backend().GetWorkspace(f.w.workspaceID())
			require.NoError(t, err)
			result, err := receiver.CurrentAgentCoordinator().Model().Model.Generate(ctx, fantasy.Call{Headers: map[string]string{"x-session-id": "namespace-acceptance"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("Namespace-free accepted OAuth")}})
			require.NoError(t, err)
			require.Equal(t, "namespace accepted", result.Content[0].(fantasy.TextContent).Text)
			require.EqualValues(t, 1, logins.Load())
			wantRefresh := mode == "expired" || mode == "unauthorized" || mode == "manual" || mode == "lost-refresh-response"
			if wantRefresh {
				require.EqualValues(t, 1, refreshes.Load())
				require.EqualValues(t, 1, freshRequests.Load())
				configured, _ := f.store.Config().Providers.Get(binding.Owner.ProviderID)
				require.Equal(t, "namespace-fresh-refresh", configured.OAuthToken.RefreshToken)
				accepted, ok := receiver.Cfg.RuntimeSnapshot().ClientOAuthToken(binding.Owner)
				require.True(t, ok)
				require.Equal(t, configured.OAuthToken, accepted)
			} else {
				require.Zero(t, refreshes.Load())
				require.EqualValues(t, 1, oldRequests.Load())
			}
			if mode == "expired" || mode == "manual" {
				require.Zero(t, oldRequests.Load())
			}
			after, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			require.Equal(t, unrelated, after, "config-only login and refresh must preserve unrelated account bytes")
			infos, files := clientAuthenticationFiles(t, f.transport.path, f.transport.accountsPath)
			require.Equal(t, serverInfos, infos)
			require.Equal(t, serverFiles, files)
			public, err := json.Marshal(receiver.Cfg.RemoteAuthority())
			require.NoError(t, err)
			for _, secret := range []string{"namespace-old-access", "namespace-old-refresh", "namespace-fresh-access", "namespace-fresh-refresh", "unrelated-private"} {
				require.NotContains(t, string(public), secret, fmt.Sprintf("private token in %s acknowledgement", mode))
			}
		})
	}
}
