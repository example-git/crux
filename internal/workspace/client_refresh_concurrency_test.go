package workspace_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestClientOAuthRefreshAcrossWorkspacesThroughTLS(t *testing.T) {
	for _, mode := range []string{"shared-client-store", "separate-client-stores", "different-principals", "expired-separate-stores", "lost-acknowledgements"} {
		t.Run(mode, func(t *testing.T) {
			xdgIsolate(t)
			t.Setenv("AI_CLI_DIR", t.TempDir())
			serverCode, err := connection.EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			identity, err := connection.NewClientIdentity("refresh-first")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(t.Context(), "refresh-first", identity.Certificate))
			peerIdentity := identity
			if mode == "different-principals" {
				peerIdentity, err = connection.NewClientIdentity("refresh-second")
				require.NoError(t, err)
				require.NoError(t, connection.AuthorizeClient(t.Context(), "refresh-second", peerIdentity.Certificate))
			}
			tlsConfig, err := connection.ServerTLSConfig(t.Context())
			require.NoError(t, err)
			s := server.NewServer(nil, "tcp", "127.0.0.1:0")
			require.NoError(t, s.EnableNetworkAuth(t.Context()))
			var puts, completions atomic.Int32
			var losePut, loseCompletion atomic.Bool
			losePut.Store(mode == "lost-acknowledgements")
			loseCompletion.Store(mode == "lost-acknowledgements")
			handler := s.Handler()
			remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lose := false
				if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
					puts.Add(1)
					lose = losePut.Swap(false)
				}
				if strings.HasSuffix(r.URL.Path, "/runtime/refresh-completion") {
					completions.Add(1)
					lose = loseCompletion.Swap(false)
				}
				if lose {
					recorder := httptest.NewRecorder()
					handler.ServeHTTP(recorder, r)
					require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, recorder.Code, recorder.Body.String())
					http.Error(w, "synthetic acknowledgement lost after commit", http.StatusBadGateway)
					return
				}
				handler.ServeHTTP(w, r)
			}))
			remote.TLS = tlsConfig
			remote.StartTLS()
			t.Cleanup(func() { remote.Close(); _ = s.Close() })

			var exchanges, oldDispatches, freshDispatches atomic.Int32
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-shared-refresh", r.Form.Get("refresh_token"))
					if exchanges.Add(1) != 1 {
						http.Error(w, "synthetic refresh token already consumed", http.StatusBadRequest)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"access_token":"synthetic-shared-fresh","refresh_token":"synthetic-shared-rotated","expires_in":3600}`))
					return
				}
				require.Equal(t, "/v1/responses", r.URL.Path)
				switch r.Header.Get("Authorization") {
				case "Bearer synthetic-shared-fresh":
					writeRefreshFixtureSSE(w, fmt.Sprintf("shared_%d", freshDispatches.Add(1)))
				case "Bearer synthetic-shared-old":
					oldDispatches.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"message":"synthetic expired credential","type":"authentication_error"}}`))
				default:
					t.Error("unexpected credential reached the disposable provider")
					http.Error(w, "unexpected credential", http.StatusForbidden)
				}
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
			installRefreshFixture(t, provider.URL, dataDir, cacheDir, "refresh-once")
			entry := accounts.Entry{ID: "shared", AccessToken: "synthetic-shared-old", RefreshToken: "synthetic-shared-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
			if mode == "expired-separate-stores" {
				entry.ExpiresAt = time.Now().Add(-time.Hour).UnixMilli()
			}
			require.NoError(t, accounts.Save(t.Context(), "example.responses", entry))
			configuration := `{"providers":{"example-responses":{"api_key":"synthetic-shared-old","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(configuration), 0o600))
			store, err := config.Load(t.TempDir(), t.TempDir(), false)
			require.NoError(t, err)
			stores := []*config.ConfigStore{store, store}
			if strings.Contains(mode, "separate") {
				stores[1], err = config.Load(t.TempDir(), t.TempDir(), false)
				require.NoError(t, err)
			}
			type participant struct {
				client    *client.Client
				workspace *workspace.ClientWorkspace
				created   *proto.Workspace
				sessionID string
			}
			var participants []participant
			for i, local := range stores {
				proposal, err := local.CollectRemoteRuntime(t.Context(), 1)
				require.NoError(t, err)
				selectedIdentity := identity
				if i == 1 {
					selectedIdentity = peerIdentity
				}
				c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: selectedIdentity})
				require.NoError(t, err)
				c.SetLocalRuntimeStore(local)
				created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), Runtime: &proposal, AuthorityMode: "client"})
				require.NoError(t, err)
				w := workspace.NewClientWorkspace(c, *created)
				t.Cleanup(w.Shutdown)
				require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
				session, err := c.CreateSession(t.Context(), created.ID, "Shared account refresh")
				require.NoError(t, err)
				participants = append(participants, participant{c, w, created, session.ID})
			}
			if mode == "different-principals" {
				require.NotEqual(t, participants[0].created.Authority.Principal, participants[1].created.Authority.Principal)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			ready, release := make(chan int, 2), make(chan struct{})
			type outcome struct {
				index int
				run   proto.RunComplete
				err   error
			}
			results := make(chan outcome, 2)
			for index, p := range participants {
				events, err := p.client.SubscribeEvents(ctx, p.created.ID)
				require.NoError(t, err)
				go func() {
					waiting := true
					for {
						select {
						case event, ok := <-events:
							if !ok {
								results <- outcome{index: index, err: fmt.Errorf("event stream closed")}
								return
							}
							if refresh, ok := event.(pubsub.Event[config.ClientRefreshRequest]); ok && waiting {
								if refresh.Payload.Principal != p.created.Authority.Principal || refresh.Payload.Revision != 1 || refresh.Payload.CredentialID != accounts.CredentialID(entry) {
									results <- outcome{index: index, err: fmt.Errorf("refresh request lost its initiating authority")}
									return
								}
								ready <- index
								select {
								case <-release:
								case <-ctx.Done():
									return
								}
								waiting = false
							}
							if p.workspace.HandleClientRefreshEvent(ctx, event) {
								continue
							}
							if finished, ok := event.(pubsub.Event[proto.RunComplete]); ok && finished.Payload.RunID == "shared-refresh" {
								results <- outcome{index: index, run: finished.Payload}
								return
							}
						case <-ctx.Done():
							results <- outcome{index: index, err: ctx.Err()}
							return
						}
					}
				}()
				require.NoError(t, p.client.SendMessageWithPermissionMode(ctx, p.created.ID, p.sessionID, "shared-refresh", "Return the fixture response.", proto.AgentPermissionDeny))
			}
			for range participants {
				select {
				case index := <-ready:
					receiver, err := s.Backend().GetWorkspace(participants[index].created.ID)
					require.NoError(t, err)
					require.Len(t, receiver.Cfg.PendingClientRefreshes(), 1)
				case result := <-results:
					t.Fatalf("workspace %d finished before both refresh requests: %v, %s", result.index, result.err, result.run.Error)
				case <-ctx.Done():
					t.Fatal("both workspaces did not request refresh")
				}
			}
			require.Zero(t, exchanges.Load())
			close(release)
			for range participants {
				select {
				case result := <-results:
					require.NoError(t, result.err)
					require.Empty(t, result.run.Error)
					require.Contains(t, result.run.Text, "verified remote refresh")
				case <-ctx.Done():
					t.Fatal("concurrent refresh did not complete")
				}
			}
			require.EqualValues(t, 1, exchanges.Load())
			require.EqualValues(t, 2, puts.Load(), "each workspace publishes once even after acknowledgement loss")
			if mode == "lost-acknowledgements" {
				require.Eventually(t, func() bool { return completions.Load() == 3 }, 5*time.Second, 10*time.Millisecond)
			}
			stored, err := accounts.Active(t.Context(), "example.responses")
			require.NoError(t, err)
			require.Equal(t, "synthetic-shared-rotated", stored.RefreshToken)
			for i, p := range participants {
				receiver, err := s.Backend().GetWorkspace(p.created.ID)
				require.NoError(t, err)
				require.Equal(t, uint64(2), receiver.Cfg.RemoteAuthority().Revision)
				require.Equal(t, p.created.Authority.Principal, receiver.Cfg.RemoteAuthority().Principal)
				owner := p.created.Runtime.Credentials[0].Owner
				accepted, ok := receiver.Cfg.EphemeralAccount(owner)
				require.True(t, ok)
				require.Equal(t, accounts.CredentialID(*stored), accounts.CredentialID(*accepted))
				local, _ := stores[i].Config().Providers.Get(owner.ProviderID)
				require.Equal(t, stored.Token(), local.OAuthToken)
			}
			if mode == "expired-separate-stores" {
				require.Zero(t, oldDispatches.Load())
			} else {
				require.GreaterOrEqual(t, oldDispatches.Load(), int32(2))
			}
			require.GreaterOrEqual(t, freshDispatches.Load(), int32(2))
		})
	}
}
