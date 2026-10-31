package client

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestPreparationReadsUseClientAuthority(t *testing.T) {
	root := t.TempDir()
	globalPath := filepath.Join(root, "global")
	require.NoError(t, os.MkdirAll(globalPath, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(globalPath, "crux.json"), []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-client-key","models":[{"id":"global-model","default_max_tokens":100},{"id":"project-model","default_max_tokens":100}]}},"models":{"large":{"provider":"fixture","model":"global-model"},"small":{"provider":"fixture","model":"global-model"}}}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"models":{"large":{"provider":"fixture","model":"project-model"}}}`), 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": globalPath, "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileCoreOnly),
	}))
	require.NoError(t, err)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("client-owned preparation read reached server")
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := captureClient(t, srv)
	client.SetLocalRuntimeStore(store)
	for workspaceID, expected := range map[string]string{"": "global-model", "remote-workspace": "project-model"} {
		view, err := client.ReadConfigView(t.Context(), workspaceID)
		require.NoError(t, err)
		require.Equal(t, expected, view.Values["models.large.model"])
	}
	authentication, err := client.ExternalAuthentication(t.Context(), "remote-workspace", config.ExternalAuthenticationRequest{Service: "openai", IncludeToken: true, RefreshToken: true})
	require.NoError(t, err)
	require.Equal(t, config.ExternalAuthentication{}, authentication)
	client.SetLocalRuntimeStore(config.NewTestStore(&config.Config{}))
	_, err = client.ReadConfigView(t.Context(), "remote-workspace")
	require.ErrorContains(t, err, "provenance is unavailable")
}
