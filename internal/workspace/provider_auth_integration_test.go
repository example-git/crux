package workspace_test

import (
	"encoding/json"
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
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthenticationThroughTLS(t *testing.T) {
	for _, mode := range []string{"server", "client", "client-expression"} {
		t.Run(mode, func(t *testing.T) { testProviderAuthenticationThroughTLS(t, mode) })
	}
}

func testProviderAuthenticationThroughTLS(t *testing.T, mode string) {
	clientOwned := strings.HasPrefix(mode, "client")
	xdgIsolate(t)
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	serverAccounts, clientAccounts := t.TempDir(), t.TempDir()
	t.Setenv("AI_CLI_DIR", serverAccounts)
	serverConfig := filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json")
	expressionMarker := filepath.Join(t.TempDir(), "key-expansions")
	t.Setenv("AUTH_STATUS_ENDPOINT", "https://initial.invalid/v1")
	writeFixture := func(path, who string) accounts.Entry {
		t.Helper()
		require.NoError(t, registrytest.Install(t.Context(), os.Getenv("CRUX_GLOBAL_DATA"), os.Getenv("CRUX_CACHE_DIR"), *registrytest.Provider("codex").Manifest))
		entry := accounts.Entry{ID: who + "-account", DisplayName: who + " account", AccessToken: "synthetic-" + who + "-access", RefreshToken: "synthetic-" + who + "-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"private":"synthetic-raw"}`)}
		require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
		key, endpoint := "synthetic-"+who+"-key", "https://initial.invalid/v1"
		if who == "client" && mode == "client-expression" {
			key = "$(printf x >> '" + expressionMarker + "'; printf synthetic-client-key)"
			endpoint = "$AUTH_STATUS_ENDPOINT"
		}
		codex := map[string]any{"api_key": entry.AccessToken, "oauth": entry.Token(), "plugin": map[string]any{"id": "test.codex", "version": "1.1.0"}, "owner": map[string]any{"type": "plugin", "construction": "integrated-codex", "compatibility_adapter": "integrated-codex"}, "models": []map[string]string{{"id": "fixture", "name": "Fixture"}}}
		if who == "server" && mode == "server" {
			// The positive logout case owns credentials in its writable scope.
			// Inherited credentials are separately tested as a prewrite refusal.
			credentials, err := json.Marshal(map[string]any{"providers": map[string]any{"codex": map[string]any{"api_key": entry.AccessToken, "oauth": entry.Token()}}})
			require.NoError(t, err)
			require.NoError(t, os.MkdirAll(os.Getenv("CRUX_GLOBAL_DATA"), 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(os.Getenv("CRUX_GLOBAL_DATA"), "crux.json"), credentials, 0o600))
			delete(codex, "api_key")
			delete(codex, "oauth")
		}
		data, err := json.Marshal(map[string]any{
			"providers": map[string]any{
				"initial": map[string]any{"id": "initial", "type": "openai-compat", "api_key": key, "base_url": endpoint, "models": []map[string]string{{"id": "fixture", "name": "Fixture"}}},
				"codex":   codex,
			},
			"models": map[string]any{"large": map[string]string{"provider": "initial", "model": "fixture"}, "small": map[string]string{"provider": "initial", "model": "fixture"}},
		})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		return entry
	}
	serverEntry := writeFixture(serverConfig, "server")
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("auth-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "auth-client", identity.Certificate))
	foreign, err := connection.NewClientIdentity("auth-foreign")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "auth-foreign", foreign.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	var publications, authReads atomic.Int32
	var reject atomic.Bool
	handler := s.Handler()
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
			publications.Add(1)
			if reject.Load() {
				http.Error(w, "synthetic publication rejection", http.StatusBadRequest)
				return
			}
		}
		if strings.HasSuffix(r.URL.Path, "/auth") || strings.HasSuffix(r.URL.Path, "/auth/accounts") {
			authReads.Add(1)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, r)
			for _, secret := range []string{"synthetic-server", "synthetic-client", "synthetic-raw", "account_namespace", "accessToken", "refreshToken", serverAccounts, clientAccounts} {
				assert.NotContains(t, recorder.Body.String(), secret)
			}
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
			return
		}
		handler.ServeHTTP(w, r)
	}))
	remote.TLS = tlsConfig
	remote.StartTLS()
	t.Cleanup(func() { remote.Close(); _ = s.Close() })

	clientConfigDir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", clientConfigDir)
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_CACHE_DIR", t.TempDir())
	t.Setenv("AI_CLI_DIR", clientAccounts)
	clientConfig := filepath.Join(clientConfigDir, "crux.json")
	clientEntry := writeFixture(clientConfig, "client")
	local, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	args := proto.Workspace{Path: t.TempDir(), AuthorityMode: "server"}
	if clientOwned {
		args.AuthorityMode = "client"
		proposal, err := local.CollectRemoteRuntime(t.Context(), 1)
		require.NoError(t, err)
		args.Runtime = &proposal
		c.SetLocalRuntimeStore(local)
	}
	created, err := c.CreateWorkspace(t.Context(), args)
	require.NoError(t, err)
	w := workspace.NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	receiver, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	// Neither store may follow a later process-global environment change.
	poison := t.TempDir()
	t.Setenv("AI_CLI_DIR", poison)
	t.Setenv("HOME", poison)
	readFile := func(path string) []byte {
		t.Helper()
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		return data
	}
	beforeServerAccounts, beforeClientAccounts := readFile(filepath.Join(serverAccounts, "accounts.json")), readFile(filepath.Join(clientAccounts, "accounts.json"))
	beforeServerConfig, beforeClientConfig := readFile(serverConfig), readFile(clientConfig)
	var markerBefore []byte
	if mode == "client-expression" {
		markerBefore = readFile(expressionMarker)
		require.NotEmpty(t, markerBefore)
	}
	first, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	second, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, second)
	status := providerAuthenticationStatus(t, first, "codex")
	expected := clientEntry
	if mode == "server" {
		expected = serverEntry
	}
	require.Equal(t, expected.ID, status.ActiveAccountID)
	require.Equal(t, "in-sync", status.AccountState)
	target := providerauth.Target{WorkspaceID: created.ID, Owner: status.Owner, Generation: first.Generation}
	list, err := w.ProviderAccounts(t.Context(), target)
	require.NoError(t, err)
	require.Len(t, list.Accounts, 1)
	require.Equal(t, expected.ID, list.Accounts[0].ID)
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-")
	require.NotContains(t, string(encoded), "account_namespace")
	if clientOwned {
		require.Zero(t, authReads.Load(), "owning client reads retained local authority")
		_, err = c.ProviderAuthentication(t.Context(), created.ID)
		require.ErrorContains(t, err, "owned by the connected client")
		_, err = c.ProviderAccounts(t.Context(), created.ID, target)
		require.ErrorContains(t, err, "owned by the connected client")
	} else {
		require.EqualValues(t, 3, authReads.Load())
	}
	require.Equal(t, beforeServerAccounts, readFile(filepath.Join(serverAccounts, "accounts.json")))
	require.Equal(t, beforeClientAccounts, readFile(filepath.Join(clientAccounts, "accounts.json")))
	require.Equal(t, beforeServerConfig, readFile(serverConfig))
	require.Equal(t, beforeClientConfig, readFile(clientConfig))
	if mode == "client-expression" {
		require.Equal(t, markerBefore, readFile(expressionMarker), "authentication reads must not reevaluate key commands")
	}
	_, err = os.Stat(filepath.Join(poison, "accounts.json.lock"))
	require.True(t, os.IsNotExist(err))
	foreignClient, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: foreign})
	require.NoError(t, err)
	_, err = foreignClient.ProviderAuthentication(t.Context(), created.ID)
	require.Error(t, err)
	_, err = foreignClient.ProviderAccounts(t.Context(), created.ID, target)
	require.Error(t, err)

	// Supported identical saves still invalidate observed targets, even when the
	// selected credential bytes and redacted status remain the same.
	accountRoot := clientAccounts
	if mode == "server" {
		accountRoot = serverAccounts
	}
	t.Setenv("AI_CLI_DIR", accountRoot)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, expected))
	_, err = w.ProviderAccounts(t.Context(), target)
	require.ErrorContains(t, err, "authentication changed")
	third, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Greater(t, third.Generation.Sequence, first.Generation.Sequence)
	require.Equal(t, first.Generation.Epoch, third.Generation.Epoch)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "other", DisplayName: "Other", AccessToken: "synthetic-other"}))
	fourth, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	require.Equal(t, "out-of-sync", providerAuthenticationStatus(t, fourth, "codex").AccountState)
	if mode == "server" {
		configured, _ := receiver.Cfg.Config().Providers.Get("codex")
		require.Equal(t, expected.AccessToken, configured.APIKey)
	} else {
		configured, _ := local.Config().Providers.Get("codex")
		require.Equal(t, expected.AccessToken, configured.APIKey)
	}
	require.Zero(t, publications.Load(), "status and account reads never publish a provider runtime")
	if clientOwned {
		reject.Store(true)
		owner, ok := local.Config().ProviderOwner("initial")
		require.True(t, ok)
		err := w.SetProviderAPIKey(config.ScopeGlobal, "initial", config.ProviderAPIKeyCredential{Owner: owner, APIKey: "synthetic-pending-key"})
		require.Error(t, err)
		putsBefore := publications.Load()
		require.EqualValues(t, 1, putsBefore)
		_, err = w.ProviderAuthentication(t.Context())
		require.ErrorContains(t, err, "not acknowledged")
		_, err = w.ProviderAuthentication(t.Context())
		require.ErrorContains(t, err, "not acknowledged")
		require.Equal(t, putsBefore, publications.Load(), "read must not republish saved changes")
		accepted, _ := receiver.Cfg.Config().Providers.Get("initial")
		require.Equal(t, "synthetic-client-key", accepted.APIKey)
	}
	if mode == "server" {
		// Exercise the public server-owned Workspace mutation path through
		// mutual TLS. Its view must bind the retained complete host owners.
		selected := accounts.Entry{ID: "selected-server", DisplayName: "Selected server", AccessToken: "synthetic-selected-server", RefreshToken: "synthetic-selected-server-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
		require.NoError(t, accounts.SaveWithoutActivating(t.Context(), accounts.ProviderCodex, selected))
		current, err := w.ProviderAuthentication(t.Context())
		require.NoError(t, err)
		currentStatus := providerAuthenticationStatus(t, current, "codex")
		request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: current.WorkspaceID, Generation: current.Generation, Owner: currentStatus.Owner}, AccountID: selected.ID}
		models := w.Config().Models
		outcome, err := w.SwitchProviderAccount(t.Context(), request)
		require.NoError(t, err)
		require.NoError(t, outcome.ValidateSwitch(request))
		require.False(t, outcome.Superseded)
		require.Equal(t, models, w.Config().Models)
		configured, _ := receiver.Cfg.Config().Providers.Get("codex")
		require.Equal(t, selected.AccessToken, configured.APIKey)
		retained, ok := receiver.Cfg.RuntimeSnapshot().ProviderOwner("codex")
		require.True(t, ok)
		bound, ok := w.Config().ProviderOwner("codex")
		require.True(t, ok)
		require.Equal(t, retained, bound)
		logout := providerauth.LogoutRequest{OperationID: strings.Repeat("b", 32), Target: outcome.Change.Current.Target}
		outcome, err = w.LogoutProvider(t.Context(), logout)
		require.NoError(t, err)
		require.NoError(t, outcome.ValidateLogout(logout))
		require.Equal(t, models, w.Config().Models)
		require.Equal(t, beforeClientAccounts, readFile(filepath.Join(clientAccounts, "accounts.json")))
		require.Equal(t, beforeClientConfig, readFile(clientConfig))
		require.Zero(t, publications.Load(), "server-owned authentication must not publish a client runtime")
	}
	t.Logf("%s authority: exact account status/list, stable and stale generations, credential-free TLS, foreign principal rejection, captured paths, and zero read-triggered publication verified", mode)
}

func providerAuthenticationStatus(t *testing.T, snapshot providerauth.Snapshot, id string) providerauth.Status {
	t.Helper()
	for _, status := range snapshot.Providers {
		if status.Owner.ProviderID == id {
			return status
		}
	}
	t.Fatalf("provider %s absent from authentication snapshot", id)
	return providerauth.Status{}
}
