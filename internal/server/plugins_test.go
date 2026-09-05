package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
	"github.com/swaggo/swag"
)

type pluginOpenAPISpec struct {
	Paths map[string]struct {
		Get struct {
			Responses map[string]struct {
				Schema struct {
					Ref string `json:"$ref"`
				} `json:"schema"`
			} `json:"responses"`
		} `json:"get"`
	} `json:"paths"`
	Definitions map[string]struct {
		Properties map[string]json.RawMessage `json:"properties"`
	} `json:"definitions"`
}

func TestGetPluginsExposesRedactedHostSnapshot(t *testing.T) {
	root := t.TempDir()
	globalData := filepath.Join(root, "global")
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))

	bundles := filepath.Join(globalData, "plugins")
	validBundle := filepath.Join(bundles, "example.echo.plugin")
	require.NoError(t, os.MkdirAll(validBundle, 0o700))
	example, err := os.ReadFile(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(validBundle, "manifest.json"), example, 0o600))

	invalidBundle := filepath.Join(bundles, "broken.plugin")
	require.NoError(t, os.MkdirAll(invalidBundle, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(invalidBundle, "manifest.json"), []byte(`{"manifest_version":1,"unknown":"Bearer secret-value"}`), 0o600))

	srv := NewServer(nil, "tcp", "127.0.0.1:0")
	response := httptest.NewRecorder()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/plugins", nil)
	srv.Handler().ServeHTTP(response, request)

	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, "application/json", response.Header().Get("Content-Type"))
	var snapshot proto.PluginSnapshot
	require.NoError(t, json.NewDecoder(response.Body).Decode(&snapshot))
	require.Equal(t, uint64(1), snapshot.Revision)
	require.Len(t, snapshot.Plugins, 2)

	broken := snapshot.Plugins[0]
	require.Equal(t, "broken.plugin", broken.BundleName)
	require.Equal(t, "invalid", broken.State)
	require.NotEmpty(t, broken.Diagnostics)
	encodedBroken, err := json.Marshal(broken)
	require.NoError(t, err)
	require.NotContains(t, string(encodedBroken), "secret-value")
	require.NotContains(t, string(encodedBroken), root)

	plugin := snapshot.Plugins[1]
	require.Equal(t, "example.echo", plugin.ID)
	require.Equal(t, "provider", plugin.PluginType)
	require.Equal(t, "example-echo", plugin.ProviderID)
	require.Equal(t, "1.0.0", plugin.Version)
	require.Equal(t, "untrusted", plugin.State)
	require.Equal(t, "unknown", plugin.Trust)
	require.Equal(t, "compatible", plugin.Compatibility)
	require.Equal(t, []string{"endpoints", "operations"}, plugin.Capabilities)
	require.Len(t, plugin.Digest, 64)

	encoded, err := json.Marshal(snapshot)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), `"manifest":`)
	require.NotContains(t, string(encoded), `"path":`)
	require.NotContains(t, string(encoded), root)
}

func TestGetPluginsSwaggerExposesPublicSnapshotSchema(t *testing.T) {
	document, err := swag.ReadDoc()
	require.NoError(t, err)

	var spec pluginOpenAPISpec
	require.NoError(t, json.Unmarshal([]byte(document), &spec))
	require.Equal(t, "#/definitions/proto.PluginSnapshot", spec.Paths["/plugins"].Get.Responses["200"].Schema.Ref)

	require.ElementsMatch(t, []string{"profile", "enabled_providers", "revision", "scanned_at", "plugins"}, mapKeys(spec.Definitions["proto.PluginSnapshot"].Properties))
	require.ElementsMatch(t, []string{"bundle_name", "plugin_type", "id", "provider_id", "name", "version", "publisher_id", "digest", "state", "trust", "compatibility", "source_kind", "source_commit", "capabilities", "diagnostics", "installed_at"}, mapKeys(spec.Definitions["proto.PluginStatus"].Properties))
	require.ElementsMatch(t, []string{"code", "message"}, mapKeys(spec.Definitions["proto.PluginDiagnostic"].Properties))
}

func TestSwaggerExposesCompleteRegistrationOwnerSchema(t *testing.T) {
	document, err := swag.ReadDoc()
	require.NoError(t, err)

	var spec pluginOpenAPISpec
	require.NoError(t, json.Unmarshal([]byte(document), &spec))
	require.ElementsMatch(t, []string{
		"provider_id",
		"account_namespace",
		"construction",
		"compatibility_adapter",
		"has_oauth",
		"oauth_adapter",
		"oauth_flow_id",
		"has_manifest",
		"manifest_id",
		"manifest_version",
		"has_preset",
		"preset_id",
		"preset_version",
		"preset_digest",
	}, mapKeys(spec.Definitions["providerregistry.RegistrationOwner"].Properties))
}

func mapKeys(values map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	return keys
}
