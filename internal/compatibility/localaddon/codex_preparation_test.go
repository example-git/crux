package localaddon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/example-git/crux/internal/server"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type preparationTokenTransport struct {
	host string
	base http.RoundTripper
}

func (transport preparationTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme != "https" || request.URL.Host != transport.host {
		return nil, errors.New("preparation fixture blocked nonfixture HTTP")
	}
	return transport.base.RoundTrip(request)
}

func codexPreparationAuthStore(t *testing.T, accountPath string) (*config.ConfigStore, accounts.Entry, *atomic.Int32, string) {
	t.Helper()
	root := t.TempDir()
	entry := accounts.Entry{ID: "synthetic-selected", AccessToken: "synthetic-preparation-token", RefreshToken: "synthetic-preparation-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	refreshes := &atomic.Int32{}
	tokens := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshes.Add(1)
		require.Equal(t, "/token", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "refresh_token", r.Form.Get("grant_type"))
		require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
		require.Equal(t, entry.RefreshToken, r.Form.Get("refresh_token"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"synthetic-refreshed-token","refresh_token":"synthetic-refreshed-refresh","expires_in":3600}`)
	}))
	t.Cleanup(tokens.Close)
	endpointURL, err := url.Parse(tokens.URL)
	require.NoError(t, err)
	previousClient, previousTransport := http.DefaultClient, http.DefaultTransport
	transport := preparationTokenTransport{host: endpointURL.Host, base: tokens.Client().Transport}
	http.DefaultClient, http.DefaultTransport = &http.Client{Transport: transport}, transport
	t.Cleanup(func() { http.DefaultClient, http.DefaultTransport = previousClient, previousTransport })
	registration := registrytest.Provider("codex")
	registration.Manifest.Capabilities.ExternalAuth = &manifest.ExternalAuthPolicy{Service: "openai", Credential: "account", Method: "chatgpt", TokenExport: "access-token"}
	for index := range registration.Manifest.Capabilities.Endpoints {
		endpoint := &registration.Manifest.Capabilities.Endpoints[index]
		if endpoint.ID == "token" {
			endpoint.BaseURL = tokens.URL + "/token"
			endpoint.AllowedHosts = []string{endpointURL.Hostname()}
		}
	}
	dataPath, cachePath := filepath.Join(root, "data"), filepath.Join(root, "cache")
	require.NoError(t, registrytest.Install(t.Context(), dataPath, cachePath, *registration.Manifest))
	document := map[string]any{
		"providers": map[string]any{"codex": map[string]any{
			"api_key": entry.AccessToken, "oauth": entry.Token(),
			"plugin": &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
			"owner":  &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter},
		}},
		"models": map[string]any{"large": map[string]string{"provider": "codex", "model": "fixture"}, "small": map[string]string{"provider": "codex", "model": "fixture"}},
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	configFile := filepath.Join(dataPath, "crux.json")
	require.NoError(t, os.WriteFile(configFile, data, 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountPath,
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": dataPath,
		"CRUX_CACHE_DIR": cachePath, "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginCompat),
	}))
	require.NoError(t, err)
	return store, entry, refreshes, configFile
}

func TestCodexPreparationStdioNativeRoutes(t *testing.T) {
	t.Run("declared policy", func(t *testing.T) { testCodexPreparationStdioNativeRoutes(t, true) })
	t.Run("no policy", func(t *testing.T) { testCodexPreparationStdioNativeRoutes(t, false) })
}

func testCodexPreparationStdioNativeRoutes(t *testing.T, declaredPolicy bool) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	globalPath := filepath.Join(root, "global")
	require.NoError(t, os.MkdirAll(globalPath, 0o700))
	globalFile := filepath.Join(globalPath, "crux.json")
	require.NoError(t, os.WriteFile(globalFile, []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-config-key","models":[{"id":"global-model","default_max_tokens":100},{"id":"project-model","default_max_tokens":100}]}},"models":{"large":{"provider":"fixture","model":"global-model"},"small":{"provider":"fixture","model":"global-model"}},"memories":{"consolidation_model":"global-memory"}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"models":{"large":{"provider":"fixture","model":"project-model"}},"memories":{"consolidation_model":"project-memory"}}`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), []byte(`{"models":null,"memories":null}`), 0o600))
	globalStore, err := config.LoadIsolated(root, filepath.Join(root, "workspace-data"), false, env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": globalPath, "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileCoreOnly),
	}))
	require.NoError(t, err)
	authStore, entry, refreshes, authFile := codexPreparationAuthStore(t, filepath.Join(root, "accounts"))
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	nativeApp := app.NewForTest(ctx)
	workspaceStore := globalStore
	if declaredPolicy {
		workspaceStore = authStore
	}
	defer nativeApp.ShutdownForTest()
	nativeServer := server.NewServer(globalStore, "unix", "")
	nativeServer.Backend().SetIdleShutdownDelay(time.Hour)
	defer nativeServer.Backend().Shutdown()
	workspace := &backend.Workspace{ID: uuid.NewString(), Path: root, App: nativeApp, Cfg: workspaceStore}
	backend.SetWorkspaceShutdownFnForTest(workspace, func() {})
	backend.InsertWorkspaceForTest(nativeServer.Backend(), workspace)
	socketDir, err := os.MkdirTemp("", "crx-prep-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(socketDir)) })
	socketPath := filepath.Join(socketDir, "native.sock")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	require.NoError(t, err)
	httpServer := &http.Server{Handler: nativeServer.Handler()}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	defer func() {
		require.NoError(t, httpServer.Close())
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	}()
	previous := codexClientFactory
	codexClientFactory = func(path string) (*client.Client, error) { return client.NewClient(path, "unix", socketPath) }
	defer func() { codexClientFactory = previous }()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	defer inputWriter.Close()
	defer outputReader.Close()
	done := make(chan error, 1)
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: root,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	go func() {
		done <- runCodexAppServer(ctx, protocolInvocation("unused", root, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	call := func(method string, params any) map[string]any {
		t.Helper()
		require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": method, "params": params}))
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, float64(1), response["id"])
		return response
	}
	require.NotContains(t, call("initialize", map[string]any{"clientInfo": map[string]any{"name": "preparation-test", "version": "1"}}), "error")
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized"}))
	for _, params := range []any{nil, map[string]any{}, map[string]any{"includeToken": nil, "refreshToken": nil}, map[string]any{"includeToken": false, "refreshToken": false}} {
		response := call("getAuthStatus", params)
		require.NotContains(t, response, "error")
		expected := codexLegacyAuthStatusResult()
		require.Equal(t, expected, response["result"])
	}
	response := call("getAuthStatus", map[string]any{"includeToken": true, "refreshToken": false})
	require.NotContains(t, response, "error")
	expected := codexLegacyAuthStatusResult()
	require.Equal(t, expected, response["result"])
	require.Zero(t, refreshes.Load())
	response = call("getAuthStatus", map[string]any{"includeToken": true, "refreshToken": true})
	require.NotContains(t, response, "error")
	require.Equal(t, codexAccountReadResult(), call("account/read", map[string]any{"refreshToken": true})["result"])
	require.Equal(t, map[string]any{"type": "apiKey"}, call("account/login/start", map[string]any{"type": "apiKey", "apiKey": "compatibility-only-test-key"})["result"])
	require.Equal(t, map[string]any{}, call("account/logout", nil)["result"])
	require.Equal(t, codexAccountReadResult(), call("account/read", nil)["result"])
	if declaredPolicy {
		require.Zero(t, refreshes.Load())
		active, err := accounts.Active(t.Context(), accounts.ProviderCodex)
		require.NoError(t, err)
		require.Equal(t, entry.AccessToken, active.AccessToken)
		require.Equal(t, entry.RefreshToken, active.RefreshToken)
		data, err := os.ReadFile(authFile)
		require.NoError(t, err)
		var saved config.Config
		require.NoError(t, json.Unmarshal(data, &saved))
		provider, ok := saved.Providers.Get("codex")
		require.True(t, ok)
		require.Equal(t, active.AccessToken, provider.APIKey)
		require.Equal(t, active.RefreshToken, provider.OAuthToken.RefreshToken)
	} else {
		require.Zero(t, refreshes.Load())
	}
	require.Equal(t, expected, response["result"])
	for _, params := range []any{[]any{}, map[string]any{"includeToken": "true"}, map[string]any{"refreshToken": 1}} {
		response := call("getAuthStatus", params)
		require.Equal(t, float64(-32600), response["error"].(map[string]any)["code"])
	}
	response = call("config/read", map[string]any{"includeLayers": false})
	require.NotContains(t, response, "error")
	result := response["result"].(map[string]any)
	require.Equal(t, map[string]any{"model": "FIXTURE-GLOBAL-MODEL", "model_provider": "crux", "model_providers": map[string]any{"crux": map[string]any{"name": "Crux"}}, "memories": map[string]any{"consolidation_model": "global-memory"}}, result["config"])
	require.NotContains(t, result, "layers")
	origins := result["origins"].(map[string]any)
	for _, field := range []string{"model", "model_provider", "memories.consolidation_model"} {
		origin := origins[field].(map[string]any)
		require.Equal(t, map[string]any{"type": "user", "file": globalFile, "profile": nil}, origin["name"])
		require.Regexp(t, `^sha256:[0-9a-f]{64}$`, origin["version"])
	}
	response = call("config/read", map[string]any{"includeLayers": true})
	require.NotContains(t, response, "error")
	require.Equal(t, result, response["result"])
	if !declaredPolicy {
		response = call("config/read", map[string]any{"cwd": root, "includeLayers": false})
		require.NotContains(t, response, "error")
		result = response["result"].(map[string]any)
		require.Equal(t, "FIXTURE-PROJECT-MODEL", result["config"].(map[string]any)["model"])
		require.Equal(t, "project-memory", result["config"].(map[string]any)["memories"].(map[string]any)["consolidation_model"])
	}
	response = call("model/list", map[string]any{})
	require.NotContains(t, response, "error")
	require.NotEmpty(t, response["result"].(map[string]any)["data"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}
