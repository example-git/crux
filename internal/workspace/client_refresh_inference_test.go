package workspace_test

import (
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
	for _, mode := range []string{"refresh-once", "expired", "never"} {
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
			remote := httptest.NewUnstartedServer(s.Handler())
			remote.TLS = tlsConfig
			remote.StartTLS()
			t.Cleanup(func() { remote.Close(); _ = s.Close() })

			var exchanges atomic.Int32
			var requestMu sync.Mutex
			var credentials []string
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-old-refresh", r.Form.Get("refresh_token"))
					exchanges.Add(1)
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
					if mode == "never" {
						require.NotEmpty(t, finished.Payload.Error)
						require.Zero(t, exchanges.Load())
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
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataDir, cacheDir))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
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
