package workspace_test

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestClientOAuthRefreshInferenceThroughTLS(t *testing.T) {
	for _, mode := range []string{"refresh-once", "expired", "never", "changed-definition", "changed-bundle", "definition-during-exchange", "bundle-during-exchange", "rejected-completion", "account-replacement-during-exchange", "account-identical-during-exchange", "account-switchback-during-exchange", "account-logout-during-exchange", "recovery-after-rotation"} {
		t.Run(mode, func(t *testing.T) {
			xdgIsolate(t)
			t.Setenv("AI_CLI_DIR", t.TempDir())
			serverCode, err := connection.EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			identity, err := connection.NewClientIdentity("inference-client")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(t.Context(), "inference-client", identity.Certificate))
			tlsConfig, err := connection.ServerTLSConfig(t.Context())
			require.NoError(t, err)
			s := server.NewServer(nil, "tcp", "127.0.0.1:0")
			require.NoError(t, s.EnableNetworkAuth(t.Context()))
			var completions atomic.Int32
			handler := s.Handler()
			remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/runtime/refresh-completion") {
					attempt := completions.Add(1)
					if mode == "rejected-completion" && attempt == 1 {
						http.Error(w, "synthetic accepted runtime conflict", http.StatusConflict)
						return
					}
				}
				handler.ServeHTTP(w, r)
			}))
			remote.TLS = tlsConfig
			remote.StartTLS()
			t.Cleanup(func() { remote.Close(); _ = s.Close() })

			var exchanges atomic.Int32
			var changeDuringExchange atomic.Pointer[func()]
			var requestMu sync.Mutex
			var credentials []string
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-old-refresh", r.Form.Get("refresh_token"))
					exchanges.Add(1)
					if change := changeDuringExchange.Load(); change != nil {
						(*change)()
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`))
					return
				}
				require.Equal(t, "/v1/responses", r.URL.Path)
				credential := r.Header.Get("Authorization")
				requestMu.Lock()
				credentials = append(credentials, credential)
				requestID := fmt.Sprintf("fixture_%d", len(credentials))
				requestMu.Unlock()
				if credential != "Bearer synthetic-new-access" {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"message":"synthetic token expired","type":"authentication_error"}}`))
					return
				}
				writeRefreshFixtureSSE(w, requestID)
			}))
			t.Cleanup(provider.Close)
			previousTransport := http.DefaultTransport
			http.DefaultTransport = provider.Client().Transport.(*http.Transport).Clone()
			t.Cleanup(func() { http.DefaultTransport = previousTransport })
			root := t.TempDir()
			configDir, dataDir, cacheDir := filepath.Join(root, "config"), filepath.Join(root, "data"), filepath.Join(root, "cache")
			require.NoError(t, os.MkdirAll(configDir, 0o700))
			t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
			t.Setenv("CRUX_GLOBAL_DATA", dataDir)
			t.Setenv("CRUX_CACHE_DIR", cacheDir)
			installRefreshFixture(t, provider.URL, dataDir, cacheDir, mode)
			entry := accounts.Entry{ID: "selected", AccessToken: "synthetic-old-access", RefreshToken: "synthetic-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
			if mode == "expired" {
				entry.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
			}
			require.NoError(t, accounts.Save(t.Context(), "example.responses", entry))
			configuration := `{"providers":{"example-responses":{"api_key":"synthetic-old-access","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(configuration), 0o600))
			store, err := config.Load(t.TempDir(), t.TempDir(), false)
			require.NoError(t, err)
			proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
			require.NoError(t, err)
			c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
			require.NoError(t, err)
			c.SetLocalRuntimeStore(store)
			created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), Runtime: &proposal, AuthorityMode: "client"})
			require.NoError(t, err)
			w := workspace.NewClientWorkspace(c, *created)
			t.Cleanup(w.Shutdown)
			require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
			changeDefinition := func() {
				var changed map[string]any
				require.NoError(t, json.Unmarshal([]byte(configuration), &changed))
				provider := changed["providers"].(map[string]any)["example-responses"].(map[string]any)
				provider["extra_headers"] = map[string]string{"X-Client-Policy": "changed"}
				data, err := json.Marshal(changed)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), data, 0o600))
				require.NoError(t, store.ReloadFromDisk(t.Context()))
			}
			changeBundle := func() {
				before, owner, err := store.RuntimeSnapshot().ClientProviderDefinition("example-responses")
				require.NoError(t, err)
				installRefreshFixture(t, provider.URL, dataDir, cacheDir, "replacement")
				require.NoError(t, store.ReloadFromDisk(t.Context()))
				after, newOwner, err := store.RuntimeSnapshot().ClientProviderDefinition("example-responses")
				require.NoError(t, err)
				require.Equal(t, owner, newOwner, "the replacement must retain the exact registration owner")
				require.NotEqual(t, before.BundleDigest, after.BundleDigest)
			}
			changeAccount := func() {
				switch mode {
				case "account-replacement-during-exchange":
					replacement := entry
					replacement.AccessToken, replacement.RefreshToken = "manual-access", "manual-refresh"
					require.NoError(t, accounts.Save(t.Context(), "example.responses", replacement))
				case "account-identical-during-exchange":
					require.NoError(t, accounts.Save(t.Context(), "example.responses", entry))
				case "account-switchback-during-exchange":
					require.NoError(t, accounts.Save(t.Context(), "example.responses", accounts.Entry{ID: "other", AccessToken: "other-access", RefreshToken: "other-refresh"}))
					require.NoError(t, accounts.SetActive(t.Context(), "example.responses", entry.ID))
				case "account-logout-during-exchange":
					require.NoError(t, accounts.RemoveProvider(t.Context(), "example.responses"))
				}
			}
			if strings.HasPrefix(mode, "account-") {
				changeDuringExchange.Store(&changeAccount)
			}
			switch mode {
			case "changed-definition":
				changeDefinition()
			case "changed-bundle":
				changeBundle()
			case "definition-during-exchange":
				changeDuringExchange.Store(&changeDefinition)
			case "bundle-during-exchange":
				changeDuringExchange.Store(&changeBundle)
			}
			if mode == "changed-definition" || mode == "changed-bundle" {
				owner := proposal.Credentials[0].Owner
				require.ErrorContains(t, w.RefreshOAuthToken(t.Context(), config.ScopeGlobal, owner), "provider definition changed")
				require.Zero(t, exchanges.Load(), "manual refresh must apply the same accepted-definition fence")
			}
			session, err := c.CreateSession(t.Context(), created.ID, "Synthetic refresh acceptance")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			events, err := c.SubscribeEvents(ctx, created.ID)
			require.NoError(t, err)
			require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, "refresh-fixture", "Return the fixture response.", proto.AgentPermissionDeny))
			for {
				select {
				case event, ok := <-events:
					require.True(t, ok, "event stream ended before the run completed")
					if w.HandleClientRefreshEvent(ctx, event) {
						continue
					}
					finished, ok := event.(pubsub.Event[proto.RunComplete])
					if !ok || finished.Payload.RunID != "refresh-fixture" {
						continue
					}
					if mode == "never" || strings.Contains(mode, "definition") || strings.Contains(mode, "bundle") || mode == "rejected-completion" || strings.HasPrefix(mode, "account-") {
						require.NotEmpty(t, finished.Payload.Error)
						expectedRefresh := "synthetic-old-refresh"
						if strings.HasSuffix(mode, "during-exchange") || mode == "rejected-completion" {
							require.EqualValues(t, 1, exchanges.Load())
							expectedRefresh = "synthetic-new-refresh"
						} else {
							require.Zero(t, exchanges.Load())
						}
						if mode != "never" {
							require.Contains(t, finished.Payload.Error, "owning client could not refresh")
							receiver, err := s.Backend().GetWorkspace(created.ID)
							require.NoError(t, err)
							if mode == "rejected-completion" {
								require.EqualValues(t, 2, completions.Load(), "report explicit failure after rejection without waiting for expiry")
								require.Empty(t, receiver.Cfg.PendingClientRefreshes())
								require.Equal(t, uint64(2), receiver.Cfg.RemoteAuthority().Revision)
							} else {
								require.Equal(t, uint64(1), receiver.Cfg.RemoteAuthority().Revision)
							}
							account, err := accounts.Active(t.Context(), "example.responses")
							require.NoError(t, err)
							switch mode {
							case "account-logout-during-exchange":
								require.Nil(t, account, "a completed exchange must not recreate the removed account")
							case "account-replacement-during-exchange":
								require.Equal(t, "manual-refresh", account.RefreshToken)
								require.Equal(t, "manual-access", account.AccessToken)
							case "account-identical-during-exchange", "account-switchback-during-exchange":
								require.Equal(t, accounts.CredentialID(entry), accounts.CredentialID(*account))
							default:
								require.Equal(t, expectedRefresh, account.RefreshToken)
							}
							require.Empty(t, receiver.Cfg.PendingClientRefreshes())
							local, _ := store.Config().Providers.Get("example-responses")
							if mode == "rejected-completion" {
								require.Equal(t, "synthetic-new-access", local.APIKey)
							} else {
								require.Equal(t, "synthetic-old-access", local.APIKey)
							}
							requestMu.Lock()
							require.NotContains(t, credentials, "Bearer synthetic-new-access", "an old request must not execute the changed provider definition")
							requestMu.Unlock()
						}
						return
					}
					require.Empty(t, finished.Payload.Error)
					require.Contains(t, finished.Payload.Text, "verified remote refresh")
					require.EqualValues(t, 1, exchanges.Load())
					persisted, err := accounts.Active(t.Context(), "example.responses")
					require.NoError(t, err)
					require.Equal(t, "synthetic-new-refresh", persisted.RefreshToken)
					local, _ := store.Config().Providers.Get("example-responses")
					require.Equal(t, persisted.Token(), local.OAuthToken)
					receiver, err := s.Backend().GetWorkspace(created.ID)
					require.NoError(t, err)
					require.Equal(t, uint64(2), receiver.Cfg.RemoteAuthority().Revision)
					if mode == "recovery-after-rotation" {
						cancel()
						require.Eventually(t, func() bool { return receiver.ConnectedClients() == 0 }, 5*time.Second, 10*time.Millisecond)
						verifyOAuthRecoveryInference(t, s, remote, c, w, created, session.ID, *persisted)
						require.EqualValues(t, 1, exchanges.Load(), "recreation must use the persisted rotation without another exchange")
					}
					requestMu.Lock()
					defer requestMu.Unlock()
					require.Contains(t, credentials, "Bearer synthetic-new-access")
					if mode == "expired" {
						require.NotContains(t, credentials, "Bearer synthetic-old-access")
					} else {
						require.Contains(t, credentials, "Bearer synthetic-old-access")
					}
					return
				case <-ctx.Done():
					t.Fatal("remote inference did not complete after client refresh")
				}
			}
		})
	}
}

func verifyOAuthRecoveryInference(t *testing.T, s *server.Server, remote *httptest.Server, c *client.Client, w *workspace.ClientWorkspace, created *proto.Workspace, sessionID string, expected accounts.Entry) {
	t.Helper()
	// Remove private fields from the public view before exercising the actual
	// reconnect loop. Recovery must use the client authority controller.
	publicEvents := make(chan any, 1)
	publicEvents <- pubsub.Event[proto.ConfigChanged]{Payload: proto.ConfigChanged{WorkspaceID: created.ID}}
	close(publicEvents)
	w.ConsumeEventsForTest(publicEvents, nil)
	t.Cleanup(workspace.SetSSEBackoffForTest(5*time.Millisecond, 25*time.Millisecond))
	done := make(chan struct{})
	go func() { w.RunSubscriptionForTest(func(tea.Msg) {}); close(done) }()
	receiver, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return receiver.ConnectedClients() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.DeleteWorkspace(t.Context(), created.ID))
	remote.CloseClientConnections()
	require.Eventually(t, func() bool { return w.WorkspaceIDForTest() != created.ID }, 10*time.Second, 20*time.Millisecond)
	recovered, err := c.GetWorkspace(t.Context(), w.WorkspaceIDForTest())
	require.NoError(t, err)
	require.Equal(t, created.DataDir, recovered.DataDir)
	require.Equal(t, created.Authority.Principal, recovered.Authority.Principal)
	require.Equal(t, uint64(2), recovered.Authority.Revision)
	receiver, err = s.Backend().GetWorkspace(recovered.ID)
	require.NoError(t, err)
	account, ok := receiver.Cfg.EphemeralAccount(created.Runtime.Credentials[0].Owner)
	require.True(t, ok)
	require.Equal(t, accounts.CredentialID(expected), accounts.CredentialID(*account))
	history, err := w.ListSessions(t.Context())
	require.NoError(t, err)
	found := false
	for _, session := range history {
		found = found || session.ID == sessionID
	}
	require.True(t, found, "OAuth recovery must preserve the same principal-scoped history")
	require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	events, err := c.SubscribeEvents(ctx, recovered.ID)
	require.NoError(t, err)
	require.NoError(t, c.SendMessageWithPermissionMode(ctx, recovered.ID, sessionID, "after-oauth-recovery", "Return the fixture response again.", proto.AgentPermissionDeny))
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok, "recovered stream ended before inference completed")
			if w.HandleClientRefreshEvent(ctx, event) {
				continue
			}
			finished, ok := event.(pubsub.Event[proto.RunComplete])
			if !ok || finished.Payload.RunID != "after-oauth-recovery" {
				continue
			}
			require.Empty(t, finished.Payload.Error)
			require.Contains(t, finished.Payload.Text, "verified remote refresh")
			w.Shutdown()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("recovery subscription did not stop")
			}
			return
		case <-ctx.Done():
			t.Fatal("recovered OAuth inference did not complete")
		}
	}
}

func installRefreshFixture(t *testing.T, endpoint, dataDir, cacheDir, mode string) {
	t.Helper()
	source := filepath.Join(t.TempDir(), "refresh.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	original := filepath.Join("..", "..", "docs", "provider-plugins", "examples", "responses-oauth.plugin")
	require.NoError(t, os.CopyFS(source, os.DirFS(original)))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	for i := range value.Capabilities.Endpoints {
		item := &value.Capabilities.Endpoints[i]
		item.BaseURL = endpoint
		if item.ID != "api" {
			item.BaseURL += "/" + item.ID
		}
		item.AllowedHosts, item.AllowedSchemes = []string{target.Hostname()}, []string{target.Scheme}
	}
	value.Capabilities.Operations[0].Retry.Authentication = "refresh-once"
	if mode == "never" {
		value.Capabilities.Operations[0].Retry.Authentication = "never"
	}
	if mode == "replacement" {
		value.Capabilities.Headers[1].Value.Value = "changed-responses"
	}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataDir, cacheDir))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, Update: mode == "replacement", ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
}

func writeRefreshFixtureSSE(w http.ResponseWriter, id string) {
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", id)
	_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
	_, _ = fmt.Fprint(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
	_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"delta\":\"verified remote refresh\"}\n\n")
	_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"verified remote refresh\",\"annotations\":[]}]}}\n\n")
	_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", id)
}
