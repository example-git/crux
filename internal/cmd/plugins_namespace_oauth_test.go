package cmd

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerdiagnostics"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPluginsTestNamespaceOAuthRunsActualHTTPSDiagnostics(t *testing.T) {
	const providerID = "namespace-cli"
	const token = "cli-selected-oauth-token"
	accountRoot := filepath.Join(t.TempDir(), "account-storage")
	require.NoError(t, os.WriteFile(accountRoot, []byte("must not be read as accounts"), 0o600))
	t.Setenv("AI_CLI_DIR", accountRoot)
	var requests atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/quota", r.URL.Path)
		assert.Equal(t, "Bearer "+token, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"remaining":0.5}`))
	}))
	t.Cleanup(host.Close)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	target, err := url.Parse(host.URL)
	require.NoError(t, err)
	declaration := manifest.Manifest{ManifestVersion: 1, ID: "namespace.cli", Version: "1.0.0", Provider: manifest.Provider{ID: providerID, Name: "Namespace CLI"}, Capabilities: manifest.Capabilities{
		Credentials: []manifest.Credential{{ID: "token", Kind: "oauth2"}},
		Endpoints:   []manifest.Endpoint{{ID: "api", BaseURL: host.URL, AllowedSchemes: []string{"https"}, AllowedHosts: []string{target.Hostname()}, Override: "forbidden", Credential: "token"}},
		Operations: []manifest.Operation{
			{ID: "inference", Kind: "inference", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/inference"},
			{ID: "quota", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodGet, Path: "/quota"},
		},
		Usage: &manifest.UsagePolicy{Source: "operation", Operation: "quota", Fallback: "unavailable", Windows: []manifest.WindowMap{{ID: "usage", RemainingFractionPointer: "/remaining"}}},
	}}
	registration, err := providerregistry.FromManifest(declaration)
	require.NoError(t, err)
	provider := config.ProviderConfig{ID: providerID, Type: catalog.TypeOpenAICompat, APIKey: token, OAuthToken: &oauth.Token{AccessToken: token, RefreshToken: "cli-refresh", Client: &oauth.OAuthClient{ClientSecret: "cli-client-secret"}}, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction}, Plugin: &config.ProviderPluginReference{ID: declaration.ID, Version: declaration.Version}}
	store := config.NewTestStoreWithRegistrations(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider})}, registration)
	previousLoader, previousRunner := loadPluginDiagnosticRuntime, runLivePluginDiagnostics
	previousAccount, previousChecks, previousJSON := pluginTestAccount, pluginTestChecks, pluginOutputJSON
	t.Cleanup(func() {
		loadPluginDiagnosticRuntime, runLivePluginDiagnostics = previousLoader, previousRunner
		pluginTestAccount, pluginTestChecks, pluginOutputJSON = previousAccount, previousChecks, previousJSON
	})
	// Only runtime loading is supplied by the fixture. Dispatch, Run, quota
	// execution, result formatting and CLI failure handling remain production.
	loadPluginDiagnosticRuntime = func(*cobra.Command) (providerdiagnostics.Runtime, error) { return store, nil }
	runLivePluginDiagnostics = providerdiagnostics.Run
	pluginTestAccount, pluginTestChecks, pluginOutputJSON = "", []string{"usage"}, true
	command, _, err := rootCmd.Find([]string{"plugins", "test"})
	require.NoError(t, err)
	require.Same(t, pluginsTestCmd, command)
	previousContext := command.Context()
	var output bytes.Buffer
	command.SetContext(t.Context())
	command.SetOut(&output)
	t.Cleanup(func() { command.SetContext(previousContext); command.SetOut(nil) })
	require.NoError(t, command.RunE(command, []string{providerID}))
	var report providerdiagnostics.Report
	require.NoError(t, json.Unmarshal(output.Bytes(), &report))
	require.True(t, report.Valid)
	require.Equal(t, providerdiagnostics.AccountResult{Loaded: true, Source: "configured"}, report.Account)
	require.EqualValues(t, 1, requests.Load())
	require.NotContains(t, output.String(), token)
	pluginTestAccount = "explicit-account-is-unavailable"
	require.ErrorContains(t, command.RunE(command, []string{providerID}), "account selection is unavailable")
	require.EqualValues(t, 1, requests.Load())
	data, err := os.ReadFile(accountRoot)
	require.NoError(t, err)
	require.Equal(t, "must not be read as accounts", string(data))
}
