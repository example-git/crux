package workspace_test

import (
	"context"
	"crypto/tls"
	"encoding/json"
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
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCopilotImportThroughTLS(t *testing.T) {
	for _, mode := range []string{"client", "no-source", "account-replacement", "empty-logout", "disk-change", "canceled", "rejected-ack", "provider-error", "server-owned"} {
		t.Run(mode, func(t *testing.T) {
			xdgIsolate(t)
			t.Setenv("AI_CLI_DIR", t.TempDir())
			t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
			t.Setenv("COPILOT_CLI_VERSION", "1.0.0")
			serverHome, clientHome := t.TempDir(), t.TempDir()
			writeSource := func(root, value string) {
				t.Helper()
				for _, path := range []string{filepath.Join(root, ".config/github-copilot/apps.json"), filepath.Join(root, "github-copilot/apps.json")} {
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, os.WriteFile(path, []byte(fmt.Sprintf(`{"github.com:Iv1.b507a08c87ecfe98":{"oauth_token":%q}}`, value)), 0o600))
				}
			}
			writeSource(serverHome, "synthetic-server-github")
			if mode != "no-source" {
				writeSource(clientHome, "synthetic-client-github")
			}
			t.Setenv("HOME", serverHome)
			t.Setenv("LOCALAPPDATA", serverHome)
			configuration := `{"providers":{"initial":{"id":"initial","type":"openai-compat","api_key":"synthetic-initial","base_url":"https://initial.invalid/v1","models":[{"id":"fixture","name":"Fixture"}]},"copilot":{"owner":{"type":"core","construction":"integrated-copilot"},"models":[{"id":"fixture","name":"Fixture"}]}},"models":{"large":{"provider":"initial","model":"fixture"},"small":{"provider":"initial","model":"fixture"}}}`
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json"), []byte(configuration), 0o600))
			serverData := os.Getenv("CRUX_GLOBAL_DATA")
			serverCode, err := connection.EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			identity, err := connection.NewClientIdentity("import-client")
			require.NoError(t, err)
			require.NoError(t, connection.AuthorizeClient(t.Context(), "import-client", identity.Certificate))
			tlsConfig, err := connection.ServerTLSConfig(t.Context())
			require.NoError(t, err)
			s := server.NewServer(nil, "tcp", "127.0.0.1:0")
			require.NoError(t, s.EnableNetworkAuth(t.Context()))
			var puts, receiverImports atomic.Int32
			observed := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/import-copilot") {
					receiverImports.Add(1)
					recorder := httptest.NewRecorder()
					s.Handler().ServeHTTP(recorder, r)
					assert.NotContains(t, recorder.Body.String(), "synthetic-")
					assert.NotContains(t, recorder.Body.String(), `"token"`)
					for key, values := range recorder.Header() {
						w.Header()[key] = values
					}
					w.WriteHeader(recorder.Code)
					_, _ = w.Write(recorder.Body.Bytes())
					return
				}
				s.Handler().ServeHTTP(w, r)
			})
			proxyTLS, err := connection.ClientTLSConfig(connection.Connection{ServerCertificate: serverCode, Client: identity})
			require.NoError(t, err)
			remote := startWorkspaceChannelProxyServer(t, observed, tlsConfig, func(*http.Request) *tls.Config { return proxyTLS }, func(fromClient bool, frame proto.WorkspaceChannelFrame) workspaceChannelProxyDecision {
				if !fromClient || frame.Type != proto.WorkspaceChannelRuntimeReplaceFrame {
					return workspaceChannelProxyDecision{}
				}
				puts.Add(1)
				if mode != "rejected-ack" {
					return workspaceChannelProxyDecision{}
				}
				ack := proto.WorkspaceChannelFrame{Type: proto.WorkspaceChannelAcknowledgementFrame, CommandID: frame.CommandID, Acknowledgement: &proto.WorkspaceChannelAcknowledgement{Status: proto.WorkspaceChannelStatusInvalid, Message: "synthetic rejection"}}
				return workspaceChannelProxyDecision{drop: true, reply: &ack}
			})
			t.Cleanup(func() { _ = s.Close() })
			var exchanges, quotas atomic.Int32
			var duringExchange atomic.Pointer[func()]
			github := "synthetic-client-github"
			if mode == "server-owned" {
				github = "synthetic-server-github"
			}
			provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "Bearer "+github, r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/copilot_internal/user" {
					quotas.Add(1)
					_, _ = w.Write([]byte(`{"copilot_plan":"Pro","quota_snapshots":{"premium_interactions":{"entitlement":100,"percent_remaining":25}}}`))
					return
				}
				assert.Equal(t, "/copilot_internal/v2/token", r.URL.Path)
				exchanges.Add(1)
				if change := duringExchange.Load(); change != nil {
					(*change)()
				}
				if mode == "provider-error" {
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte(`{"error":"synthetic-provider-secret"}`))
					return
				}
				_, _ = fmt.Fprintf(w, `{"token":"synthetic-imported-access","expires_at":%d}`, time.Now().Add(time.Hour).Unix())
			}))
			t.Cleanup(provider.Close)
			// Route only the fixed Copilot service host through a trusted disposable
			// TLS endpoint. Both token exchange and quota run normal HTTP adapters.
			base := provider.Client().Transport.(*http.Transport).Clone()
			priorTransport := http.DefaultTransport
			http.DefaultTransport = refreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Host != "api.github.com" {
					return nil, fmt.Errorf("unexpected isolated import host %s", r.URL.Host)
				}
				clone := r.Clone(r.Context())
				clone.URL.Host = strings.TrimPrefix(provider.URL, "https://")
				clone.Host = clone.URL.Host
				return base.RoundTrip(clone)
			})
			t.Cleanup(func() { http.DefaultTransport = priorTransport; base.CloseIdleConnections() })
			clientConfig, clientData := t.TempDir(), t.TempDir()
			t.Setenv("CRUX_GLOBAL_CONFIG", clientConfig)
			t.Setenv("CRUX_GLOBAL_DATA", clientData)
			t.Setenv("CRUX_CACHE_DIR", t.TempDir())
			t.Setenv("HOME", clientHome)
			t.Setenv("LOCALAPPDATA", clientHome)
			require.NoError(t, os.WriteFile(filepath.Join(clientConfig, "crux.json"), []byte(configuration), 0o600))
			store, err := config.Load(t.TempDir(), t.TempDir(), false)
			require.NoError(t, err)
			owner, ok := store.RuntimeSnapshot().ProviderOwner("copilot")
			require.True(t, ok)
			proposal, err := store.CollectRemoteRuntimeWithUnavailable(t.Context(), 1, map[providerregistry.RegistrationOwner]bool{owner: true})
			require.NoError(t, err)
			c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
			require.NoError(t, err)
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
			// Change process-global HOME only after capturing the client's runtime.
			// A receiver/global-environment substitution must use the wrong file.
			t.Setenv("HOME", serverHome)
			t.Setenv("LOCALAPPDATA", serverHome)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			change := func() {
				switch mode {
				case "account-replacement":
					assert.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, accounts.Entry{ID: "default", AccessToken: "synthetic-manual-account"}))
				case "empty-logout":
					assert.NoError(t, accounts.RemoveProvider(t.Context(), owner.AccountNamespace))
				case "disk-change":
					assert.NoError(t, os.WriteFile(filepath.Join(clientData, "crux.json"), []byte(`{"providers":{"copilot":{"api_key":"synthetic-manual-config"}}}`), 0o600))
				case "canceled":
					cancel()
				}
			}
			duringExchange.Store(&change)
			if mode != "server-owned" {
				// Even a direct authenticated call cannot make the receiver read
				// its disk token for a client-owned workspace.
				_, err := c.ImportCopilot(t.Context(), created.ID, owner)
				require.ErrorContains(t, err, "status code 400")
				require.Zero(t, exchanges.Load())
			}
			importsBefore := receiverImports.Load()
			found, importErr := w.ImportCopilot(ctx, owner)
			switch mode {
			case "no-source":
				require.NoError(t, importErr)
				require.False(t, found)
				require.Zero(t, exchanges.Load())
				require.Zero(t, puts.Load())
				entries, err := accounts.List(t.Context(), owner.AccountNamespace)
				require.NoError(t, err)
				require.Empty(t, entries)
			case "client", "server-owned":
				require.NoError(t, importErr)
				require.True(t, found)
				require.EqualValues(t, 1, exchanges.Load())
				entry, err := accounts.Active(t.Context(), owner.AccountNamespace)
				require.NoError(t, err)
				require.NotNil(t, entry)
				require.Equal(t, github, entry.RefreshToken)
				require.Equal(t, "synthetic-imported-access", entry.AccessToken)
				persistedPath := filepath.Join(clientData, "crux.json")
				if mode == "server-owned" {
					persistedPath = filepath.Join(serverData, "crux.json")
				}
				persisted, err := os.ReadFile(persistedPath)
				require.NoError(t, err)
				require.Equal(t, entry.AccessToken, gjson.GetBytes(persisted, "providers.copilot.api_key").String())
				require.Equal(t, entry.RefreshToken, gjson.GetBytes(persisted, "providers.copilot.oauth.refresh_token").String())
				if mode == "client" {
					acknowledged, err := c.GetWorkspace(t.Context(), created.ID)
					require.NoError(t, err)
					require.EqualValues(t, 2, acknowledged.Authority.Revision)
					require.EqualValues(t, 1, puts.Load())
				} else {
					require.EqualValues(t, importsBefore+1, receiverImports.Load())
					require.Zero(t, puts.Load())
				}
				configuredProvider, configured := w.Config().Providers.Get("copilot")
				require.True(t, configured)
				require.NotEmpty(t, configuredProvider.Models)
				_, err = w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, config.SelectedModel{Provider: "copilot", Model: configuredProvider.Models[0].ID}, owner)
				require.NoError(t, err)
				u, err := w.PrepareProviderUsage(owner)(t.Context())
				require.NoError(t, err)
				require.Equal(t, 75, u.Windows[0].Percent)
				require.EqualValues(t, 1, quotas.Load())
				if mode == "client" {
					selected, err := c.GetWorkspace(t.Context(), created.ID)
					require.NoError(t, err)
					require.EqualValues(t, 3, selected.Authority.Revision)
					require.EqualValues(t, 2, puts.Load())
				}
				_, err = w.ImportCopilot(t.Context(), owner)
				require.NoError(t, err)
				require.EqualValues(t, 1, exchanges.Load(), "configured credentials must not be overwritten by automatic import")
			default:
				require.Error(t, importErr)
				require.False(t, found)
				require.NotContains(t, importErr.Error(), "synthetic-provider-secret")
				require.EqualValues(t, 1, exchanges.Load())
				remoteState, err := c.GetWorkspace(t.Context(), created.ID)
				require.NoError(t, err)
				require.EqualValues(t, 1, remoteState.Authority.Revision)
				if mode == "disk-change" {
					require.ErrorContains(t, importErr, "authentication configuration inputs changed")
					data, err := os.ReadFile(filepath.Join(clientData, "crux.json"))
					require.NoError(t, err)
					require.Contains(t, string(data), "synthetic-manual-config")
					entry, err := accounts.Active(t.Context(), owner.AccountNamespace)
					require.NoError(t, err)
					require.Nil(t, entry, "config preflight failure must precede the account save")
				}
				if mode == "empty-logout" {
					entries, err := accounts.List(t.Context(), owner.AccountNamespace)
					require.NoError(t, err)
					require.Empty(t, entries)
				}
				if mode == "account-replacement" {
					entry, err := accounts.Active(t.Context(), owner.AccountNamespace)
					require.NoError(t, err)
					require.Equal(t, "synthetic-manual-account", entry.AccessToken)
				}
				if mode == "rejected-ack" {
					entry, err := accounts.Active(t.Context(), owner.AccountNamespace)
					require.NoError(t, err)
					require.NotNil(t, entry)
					require.Equal(t, "synthetic-imported-access", entry.AccessToken)
				}
				if mode == "rejected-ack" {
					data, err := os.ReadFile(filepath.Join(clientData, "crux.json"))
					require.NoError(t, err)
					require.Equal(t, "synthetic-imported-access", gjson.GetBytes(data, "providers.copilot.api_key").String())
					require.ErrorContains(t, importErr, "acknowledgement is pending")
				}
			}
			if mode != "server-owned" {
				require.Equal(t, importsBefore, receiverImports.Load(), "owning client import must not use the receiver import endpoint")
				data, err := os.ReadFile(filepath.Join(serverData, "crux.json"))
				require.True(t, err == nil || os.IsNotExist(err))
				require.NotContains(t, string(data), "synthetic-imported")
			}
			public, err := c.GetWorkspace(t.Context(), created.ID)
			require.NoError(t, err)
			encoded, err := json.Marshal(public)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "synthetic-imported")
			require.NotContains(t, string(encoded), "synthetic-client-github")
		})
	}
}
