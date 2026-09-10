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
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type quotaBarrier struct {
	started  chan struct{}
	release  chan struct{}
	canceled chan struct{}
}

func TestProviderUsageThroughTLS(t *testing.T) {
	for _, mode := range []string{"plugin", "codex", "copilot", "server-owned"} {
		t.Run(mode, func(t *testing.T) {
			xdgIsolate(t)
			t.Setenv("AI_CLI_DIR", t.TempDir())
			t.Setenv("CODEX_VERSION", "1.0.0")
			t.Setenv("COPILOT_CLI_VERSION", "1.0.0")
			t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
			serverData, serverCache := os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR")
			if mode == "plugin" {
				t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
			}
			if mode == "server-owned" {
				serverConfig := os.Getenv("CRUX_GLOBAL_CONFIG")
				require.NoError(t, os.MkdirAll(serverConfig, 0o700))
				require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "selected", AccessToken: "synthetic-quota-access", RefreshToken: "synthetic-quota-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
				require.NoError(t, os.WriteFile(filepath.Join(serverConfig, "crux.json"), []byte(`{"providers":{"codex":{"api_key":"synthetic-quota-access","oauth":{"access_token":"synthetic-quota-access","refresh_token":"synthetic-quota-refresh"},"plugin":{"id":"test.codex","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-codex","compatibility_adapter":"integrated-codex"},"models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":"codex","model":"fixture"},"small":{"provider":"codex","model":"fixture"}}}`), 0o600))
			}
			serverCode, err := connection.EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			identity, err := connection.NewClientIdentity("quota-client")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(t.Context(), "quota-client", identity.Certificate))
			otherIdentity, err := connection.NewClientIdentity("other-client")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(t.Context(), "other-client", otherIdentity.Certificate))
			tlsConfig, err := connection.ServerTLSConfig(t.Context())
			require.NoError(t, err)
			s := server.NewServer(nil, "tcp", "127.0.0.1:0")
			require.NoError(t, s.EnableNetworkAuth(t.Context()))
			var wireMu sync.Mutex
			var wire []string
			remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/providers/usage") {
					recorder := httptest.NewRecorder()
					s.Handler().ServeHTTP(recorder, r)
					wireMu.Lock()
					wire = append(wire, recorder.Body.String())
					wireMu.Unlock()
					for key, values := range recorder.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
					return
				}
				s.Handler().ServeHTTP(w, r)
			}))
			remote.TLS = tlsConfig
			remote.StartTLS()
			t.Cleanup(func() { remote.Close(); _ = s.Close() })

			var calls atomic.Int32
			var barrier atomic.Pointer[quotaBarrier]
			var reject atomic.Bool
			var expected atomic.Value
			expected.Store("Bearer synthetic-quota-access")
			if mode == "copilot" {
				expected.Store("Bearer synthetic-quota-refresh")
			}
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, expected.Load(), r.Header.Get("Authorization"))
				assert.Contains(t, []string{"/quota", "/copilot_internal/user"}, r.URL.Path)
				if wait := barrier.Swap(nil); wait != nil {
					close(wait.started)
					select {
					case <-wait.release:
					case <-r.Context().Done():
						close(wait.canceled)
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				if reject.Load() {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"synthetic-provider-secret"}`))
					return
				}
				switch mode {
				case "plugin":
					_, _ = w.Write([]byte(`{"plan":"Pro","remaining":0.25,"raw_secret":"synthetic-provider-secret"}`))
				case "copilot":
					_, _ = w.Write([]byte(`{"copilot_plan":"Pro","quota_snapshots":{"premium_interactions":{"percent_remaining":25,"entitlement":100}},"raw_secret":"synthetic-provider-secret"}`))
				default:
					_, _ = w.Write([]byte(`{"plan_type":"Pro","rate_limit":{"primary_window":{"used_percent":75,"limit_window_seconds":18000}},"raw_secret":"synthetic-provider-secret"}`))
				}
			}))
			t.Cleanup(provider.Close)
			previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
			http.DefaultTransport = provider.Client().Transport.(*http.Transport).Clone()
			target, err := url.Parse(provider.URL)
			require.NoError(t, err)
			// Copilot retains its core URL; bundle usage uses the declared TLS URL.
			http.DefaultClient = &http.Client{Transport: refreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" || r.URL.Path != "/copilot_internal/user" {
					return nil, fmt.Errorf("unexpected isolated quota destination %s", r.URL.Host)
				}
				clone := r.Clone(r.Context())
				clone.URL.Scheme, clone.URL.Host, clone.Host = target.Scheme, target.Host, target.Host
				return provider.Client().Transport.RoundTrip(clone)
			})}
			t.Cleanup(func() { http.DefaultTransport, http.DefaultClient = previousTransport, previousClient })
			configDir, dataDir, cacheDir := t.TempDir(), t.TempDir(), t.TempDir()
			t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
			t.Setenv("CRUX_GLOBAL_DATA", dataDir)
			t.Setenv("CRUX_CACHE_DIR", cacheDir)
			id, namespace, construction := "codex", accounts.ProviderCodex, providerregistry.ConstructionCodex
			if mode == "copilot" {
				id, namespace, construction = "copilot", accounts.ProviderCopilot, providerregistry.ConstructionCopilot
			}
			configuration := fmt.Sprintf(`{"providers":{%q:{"api_key":"synthetic-quota-access","owner":{"type":"core","construction":%q},"models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":%q,"model":"fixture"},"small":{"provider":%q,"model":"fixture"}}}`, id, construction, id, id)
			if id == "codex" && mode != "plugin" {
				value := *registrytest.Provider("codex").Manifest
				for i := range value.Capabilities.Endpoints {
					e := &value.Capabilities.Endpoints[i]
					if e.ID == "usage" {
						e.BaseURL, e.AllowedHosts = provider.URL, []string{target.Hostname()}
					}
				}
				require.NoError(t, registrytest.Install(t.Context(), dataDir, cacheDir, value))
				if mode == "server-owned" {
					require.NoError(t, registrytest.Install(t.Context(), serverData, serverCache, value))
				}
				configuration = strings.Replace(configuration, `"owner":{"type":"core","construction":"integrated-codex"}`, `"plugin":{"id":"test.codex","version":"1.1.0"},"owner":{"type":"plugin","construction":"integrated-codex","compatibility_adapter":"integrated-codex"}`, 1)
			}
			if mode == "plugin" {
				installRefreshFixture(t, provider.URL, dataDir, cacheDir, "quota")
				id, namespace = "example-responses", "example.responses"
				configuration = `{"providers":{"example-responses":{"api_key":"synthetic-quota-access","plugin":{"id":"example.responses-oauth","version":"1.2.0"},"configuration":{"oauth_client_id":"synthetic-client"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner"},"small":{"provider":"example-responses","model":"example-small"}}}`
			}
			// Expiry does not authorize quota to exchange tokens. This matches the
			// previous exact-token UI helper and catches accidental refresh paths.
			entry := accounts.Entry{ID: "selected", AccessToken: "synthetic-quota-access", RefreshToken: "synthetic-quota-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
			require.NoError(t, accounts.Save(t.Context(), namespace, entry))
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(configuration), 0o600))
			store, err := config.Load(t.TempDir(), t.TempDir(), false)
			require.NoError(t, err)
			proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
			require.NoError(t, err)
			connect := func(identity connection.Identity) *client.Client {
				t.Helper()
				c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
				require.NoError(t, err)
				return c
			}
			c := connect(identity)
			args := proto.Workspace{Path: t.TempDir(), AuthorityMode: "client", Runtime: &proposal}
			if mode == "server-owned" {
				args.AuthorityMode, args.Runtime = "server", nil
			} else {
				c.SetLocalRuntimeStore(store)
			}
			created, err := c.CreateWorkspace(t.Context(), args)
			require.NoError(t, err)
			w := workspace.NewClientWorkspace(c, *created)
			t.Cleanup(w.Shutdown)
			owner, ok := store.Config().ProviderOwner(id)
			require.True(t, ok)
			discovery, err := c.GetWorkspace(t.Context(), created.ID)
			require.NoError(t, err)
			public, ok := discovery.Config.Providers.Get(id)
			require.True(t, ok)
			require.Empty(t, public.APIKey)
			require.Nil(t, public.OAuthToken)
			encoded, err := json.Marshal(discovery)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "synthetic-quota")
			if mode == "server-owned" {
				public, _ := w.Config().Providers.Get(id)
				require.Nil(t, public.OAuthToken, "remote usage must work from redacted discovery")
			}
			// A different active account is intentionally visible to this process.
			// Neither client-scoped nor captured server configuration may adopt it.
			require.NoError(t, accounts.Save(t.Context(), namespace, accounts.Entry{ID: "unrelated", AccessToken: "synthetic-wrong-account", RefreshToken: "synthetic-wrong-refresh"}))
			fetch := w.PrepareProviderUsage(owner)
			u, err := fetch(t.Context())
			require.NoError(t, err)
			require.NotNil(t, u)
			require.Equal(t, id, u.ProviderID)
			require.Equal(t, "Pro", u.Plan)
			require.Len(t, u.Windows, 1)
			require.Equal(t, 75, u.Windows[0].Percent)
			require.False(t, u.FetchedAt.IsZero())
			require.EqualValues(t, 1, calls.Load())
			request := config.ProviderUsageRequest{Owner: owner}
			if mode != "server-owned" {
				request.Revision, request.Digest = discovery.Authority.Revision, discovery.Authority.Digest
			}
			_, err = connect(otherIdentity).ProviderUsage(t.Context(), created.ID, request)
			require.ErrorContains(t, err, "status code 403")
			bad := request
			bad.Revision++
			_, err = c.ProviderUsage(t.Context(), created.ID, bad)
			require.ErrorContains(t, err, "status code 409")
			bad = request
			bad.Owner.Construction = "different-owner"
			_, err = c.ProviderUsage(t.Context(), created.ID, bad)
			require.ErrorContains(t, err, "status code 409")
			require.EqualValues(t, 1, calls.Load())
			reject.Store(true)
			_, err = fetch(t.Context())
			require.ErrorContains(t, err, "status code 502")
			require.EqualValues(t, 2, calls.Load(), "quota must not retry authentication or refresh")
			reject.Store(false)
			wireMu.Lock()
			for _, response := range wire {
				assert.NotContains(t, response, "synthetic-")
				assert.NotContains(t, response, "raw_secret")
			}
			wireMu.Unlock()
			if mode != "plugin" {
				return
			}

			startBlocked := func(fetch oauthusage.Request) (*quotaBarrier, <-chan error) {
				t.Helper()
				wait := &quotaBarrier{started: make(chan struct{}), release: make(chan struct{}), canceled: make(chan struct{})}
				barrier.Store(wait)
				done := make(chan error, 1)
				go func() { _, err := fetch(t.Context()); done <- err }()
				select {
				case <-wait.started:
				case <-time.After(5 * time.Second):
					t.Fatal("quota did not reach provider")
				}
				return wait, done
			}
			wait, done := startBlocked(fetch)
			// Publish a new exact credential while the old request is admitted.
			entry.AccessToken, entry.RefreshToken = "synthetic-next-access", "synthetic-next-refresh"
			require.NoError(t, accounts.Save(t.Context(), namespace, entry))
			require.NoError(t, w.SetProviderAPIKey(config.ScopeGlobal, id, config.ProviderOAuthCredential{Owner: owner, Token: entry.Token()}))
			close(wait.release)
			select {
			case err := <-done:
				require.ErrorIs(t, err, config.ErrProviderUsageAuthority)
			case <-time.After(5 * time.Second):
				t.Fatal("stale usage did not return")
			}
			before := calls.Load()
			_, err = fetch(t.Context())
			require.ErrorIs(t, err, config.ErrProviderUsageAuthority)
			require.Equal(t, before, calls.Load(), "stale prepared command must not dispatch")
			expected.Store("Bearer synthetic-next-access")
			u, err = w.PrepareProviderUsage(owner)(t.Context())
			require.NoError(t, err)
			require.Equal(t, 75, u.Windows[0].Percent)
			// Cancellation must propagate through TLS RPC and the actual provider request.
			cancelCtx, cancel := context.WithCancel(t.Context())
			fresh := w.PrepareProviderUsage(owner)
			wait, done = startBlocked(func(context.Context) (*oauthusage.Usage, error) { return fresh(cancelCtx) })
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("quota request ignored cancellation")
			}
			select {
			case <-wait.canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("provider request was not canceled")
			}
			// Explicit removal preserves absence even with another global account.
			require.NoError(t, w.RemoveProviderCredentials(config.ScopeGlobal, owner))
			before = calls.Load()
			u, err = w.PrepareProviderUsage(owner)(t.Context())
			require.ErrorContains(t, err, "status code 502")
			require.Nil(t, u)
			require.Equal(t, before, calls.Load())
			// Restore credentials and tear down while the server is inside quota.
			require.NoError(t, w.SetProviderAPIKey(config.ScopeGlobal, id, config.ProviderOAuthCredential{Owner: owner, Token: entry.Token()}))
			wait, done = startBlocked(w.PrepareProviderUsage(owner))
			require.NoError(t, c.DeleteWorkspace(t.Context(), created.ID))
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("teardown did not stop quota")
			}
			select {
			case <-wait.canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("teardown did not cancel provider request")
			}
		})
	}
}
