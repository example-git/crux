package workspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	tea "github.com/example-git/crux/foundation/bubbletea"
	"io"
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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestClientOAuthRefreshInferenceThroughTLS(t *testing.T) {
	for _, mode := range []string{"refresh-once", "expired", "never", "changed-definition", "changed-bundle", "definition-during-exchange", "bundle-during-exchange", "rejected-completion", "account-replacement-during-exchange", "account-identical-during-exchange", "account-switchback-during-exchange", "account-logout-during-exchange", "recovery-after-rotation", "runtime-controls", "controls-during-expiry", "controls-during-refresh", "expired-fresh-rejected"} {
		t.Run(mode, func(t *testing.T) {
			xdgIsolate(t)
			t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true") // Keep background extraction outside the foreground control transaction.
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
			var capturedTitleRequests atomic.Int32
			var changeDuringExchange atomic.Pointer[func()]
			var requestMu sync.Mutex
			var credentials []string
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					require.NoError(t, r.ParseForm())
					exchanges.Add(1)
					assert.Equal(t, "synthetic-old-refresh", r.Form.Get("refresh_token"))
					if change := changeDuringExchange.Load(); change != nil {
						(*change)()
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"synthetic-new-access","refresh_token":"synthetic-new-refresh","expires_in":3600}`))
					return
				}
				require.Equal(t, "/v1/responses", r.URL.Path)
				if mode == "runtime-controls" || strings.HasPrefix(mode, "controls-during-") {
					body, err := io.ReadAll(r.Body)
					require.NoError(t, err)
					want := "high"
					if strings.Contains(string(body), "controls-removal") {
						want = "medium"
					} else if strings.Contains(string(body), "controls-low") {
						want = "low"
					}
					assert.Equal(t, want, gjson.GetBytes(body, "reasoning.effort").String(), "captured reasoning control for purpose=%s model=%s", r.Header.Get("x-request-purpose"), gjson.GetBytes(body, "model").String())
					assert.Equal(t, want, gjson.GetBytes(body, "text.verbosity").String(), "captured verbosity control for purpose=%s model=%s", r.Header.Get("x-request-purpose"), gjson.GetBytes(body, "model").String())
					if r.Header.Get("x-request-purpose") == "title" && r.Header.Get("Authorization") == "Bearer synthetic-new-access" {
						capturedTitleRequests.Add(1)
					}
					if strings.Contains(string(body), "controls-instructions") {
						require.Contains(t, string(body), "You are a token engine.", "enabling the client instruction section must affect inference")
					}
				}
				credential := r.Header.Get("Authorization")
				requestMu.Lock()
				credentials = append(credentials, credential)
				requestID := fmt.Sprintf("fixture_%d", len(credentials))
				requestMu.Unlock()
				if credential != "Bearer synthetic-new-access" || mode == "expired-fresh-rejected" {
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
			if mode == "expired" || mode == "controls-during-expiry" || mode == "expired-fresh-rejected" {
				entry.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
			}
			require.NoError(t, accounts.Save(t.Context(), "example.responses", entry))
			configuration := `{"providers":{"example-responses":{"api_key":"synthetic-old-access","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(configuration), 0o600))
			store, err := config.Load(t.TempDir(), t.TempDir(), false)
			require.NoError(t, err)
			if mode == "runtime-controls" || strings.HasPrefix(mode, "controls-during-") {
				require.NoError(t, store.SetConfigFields(config.ScopeGlobal, map[string]any{
					"options.analysis_effort": "high", "options.response_verbosity": "high",
					"options.disable_auto_summarize": true, "options.summarization_context_cap": 8192,
					"options.summarization_max_tokens": 1024, "options.summarization_fast_mode": true,
					"options.instruction_mode": "project", "options.disabled_instruction_sections": []string{"identity"},
				}))
			}
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
			case "controls-during-expiry", "controls-during-refresh":
				changeControls := func() {
					require.NoError(t, store.SetConfigFields(config.ScopeGlobal, map[string]any{
						"options.analysis_effort": "low", "options.response_verbosity": "low",
					}))
				}
				changeDuringExchange.Store(&changeControls)
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
					if mode == "expired-fresh-rejected" {
						require.NotEmpty(t, finished.Payload.Error)
						require.EqualValues(t, 1, exchanges.Load(), "expiry and fresh-token rejection share one refresh allowance")
						require.EqualValues(t, 1, completions.Load())
						requestMu.Lock()
						defer requestMu.Unlock()
						require.NotEmpty(t, credentials)
						require.NotContains(t, credentials, "Bearer synthetic-old-access")
						return
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
					if strings.HasPrefix(mode, "controls-during-") {
						require.Eventually(t, func() bool { return capturedTitleRequests.Load() > 0 }, 5*time.Second, 10*time.Millisecond, "the captured title model must execute before checking the next request")
						require.Equal(t, "low", receiver.Cfg.Config().Options.AnalysisEffort)
						require.Equal(t, "low", receiver.Cfg.Config().Options.ResponseVerbosity)
						require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, "controls-low", "controls-low: return the fixture response.", proto.AgentPermissionDeny))
						awaitRefreshFixtureRun(t, ctx, events, w, "controls-low")
						require.EqualValues(t, 1, exchanges.Load())
					}
					if mode == "runtime-controls" {
						options := receiver.Cfg.Config().Options
						require.True(t, options.DisableAutoSummarize)
						require.EqualValues(t, 8192, options.SummarizationContextCap)
						require.EqualValues(t, 1024, options.SummarizationMaxTokens)
						require.True(t, options.SummarizationFastMode)
						require.Equal(t, "project", options.InstructionMode)
						require.NoError(t, w.SetCompactMode(config.ScopeGlobal, true))
						require.True(t, w.Config().Options.TUI.CompactMode)
						require.Equal(t, uint64(2), receiver.Cfg.RemoteAuthority().Revision, "a display preference must not replace provider authority")
						localBefore, err := os.ReadFile(config.GlobalConfigData())
						require.NoError(t, err)
						beforeInvalid := receiver.Cfg.RemoteAuthority()
						require.ErrorContains(t, w.SetConfigField(config.ScopeGlobal, "options.analysis_effort", "invalid"), "analysis_effort")
						localAfter, err := os.ReadFile(config.GlobalConfigData())
						require.NoError(t, err)
						require.Equal(t, localBefore, localAfter)
						require.Equal(t, beforeInvalid, receiver.Cfg.RemoteAuthority())
						for _, phase := range []string{"controls-low", "controls-removal"} {
							for _, field := range []string{"options.analysis_effort", "options.response_verbosity"} {
								if phase == "controls-low" {
									require.NoError(t, w.SetConfigField(config.ScopeGlobal, field, "low"))
								} else {
									require.NoError(t, w.RemoveConfigField(config.ScopeGlobal, field))
								}
							}
							require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, phase, phase+": return the fixture response.", proto.AgentPermissionDeny))
							awaitRefreshFixtureRun(t, ctx, events, w, phase)
						}
						instructions := func() string {
							snapshot, err := c.GetAgentInstructions(ctx, created.ID)
							require.NoError(t, err)
							data, err := json.Marshal(snapshot)
							require.NoError(t, err)
							return string(data)
						}
						require.NotContains(t, instructions(), "You are a token engine.")
						require.NoError(t, w.SetConfigField(config.ScopeGlobal, "options.instruction_mode", "native"))
						require.NotContains(t, instructions(), "You are a token engine.", "the explicitly disabled section must remain disabled")
						require.NoError(t, w.RemoveConfigField(config.ScopeGlobal, "options.disabled_instruction_sections"))
						require.Contains(t, instructions(), "You are a token engine.")
						require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, "controls-instructions", "controls-instructions: return the fixture response.", proto.AgentPermissionDeny))
						awaitRefreshFixtureRun(t, ctx, events, w, "controls-instructions")
						require.EqualValues(t, 1, exchanges.Load())
					}
					if mode == "recovery-after-rotation" {
						cancel()
						require.Eventually(t, func() bool { return receiver.ConnectedClients() == 0 }, 5*time.Second, 10*time.Millisecond)
						verifyOAuthRecoveryInference(t, s, remote, c, w, created, session.ID, *persisted)
						require.EqualValues(t, 1, exchanges.Load(), "recreation must use the persisted rotation without another exchange")
					}
					requestMu.Lock()
					defer requestMu.Unlock()
					require.Contains(t, credentials, "Bearer synthetic-new-access")
					if mode == "expired" || mode == "controls-during-expiry" || mode == "expired-fresh-rejected" {
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

func awaitRefreshFixtureRun(t *testing.T, ctx context.Context, events <-chan any, w *workspace.ClientWorkspace, runID string) {
	t.Helper()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok, "stream ended before control verification completed")
			if w.HandleClientRefreshEvent(ctx, event) {
				continue
			}
			finished, ok := event.(pubsub.Event[proto.RunComplete])
			if !ok || finished.Payload.RunID != runID {
				continue
			}
			require.Empty(t, finished.Payload.Error)
			require.Contains(t, finished.Payload.Text, "verified remote refresh")
			return
		case <-ctx.Done():
			t.Fatal("runtime-control inference did not complete")
		}
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
	if mode == "quota" {
		value.Capabilities.Operations = append(value.Capabilities.Operations, manifest.Operation{
			ID: "quota", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodGet, Path: "/quota",
		})
		value.Capabilities.Usage = &manifest.UsagePolicy{Source: "operation", Operation: "quota", Fallback: "unavailable",
			PlanPointers: []string{"/plan"}, Windows: []manifest.WindowMap{{ID: "weekly", RemainingFractionPointer: "/remaining"}},
		}
	}
	if mode == "runtime-controls" || strings.HasPrefix(mode, "controls-during-") {
		value.Capabilities.RuntimeControls = append(value.Capabilities.RuntimeControls, manifest.RuntimeControl{
			ID: "response_verbosity", Label: "Response verbosity", Type: "enum", Values: []string{"low", "medium", "high"},
			Default: "medium", Scope: "model", RequestPath: "/text/verbosity",
		})
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
