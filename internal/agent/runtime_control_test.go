package agent

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestRuntimeControlConstructionPreservesExactNumbers(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.count", Type: "integer", Default: json.Number("9007199254740993"), Scope: "provider", RequestPath: "/vendor/count"}
	registration, err := providerregistry.FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
		Capabilities: manifest.Capabilities{
			Endpoints:       []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations:      []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: "openai-responses", Transport: "sse", Endpoint: "api", Path: "/v1/responses", Retry: &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"}}},
			RuntimeControls: []manifest.RuntimeControl{control},
		},
	})
	require.NoError(t, err)
	defaults, err := runtimeControlValues(registration.RuntimeControls, nil)
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), defaults["/vendor/count"])
	model := Model{CatalogModel: catalog.Model{ID: "test-model"}, ModelCfg: config.SelectedModel{Provider: "synthetic"}}
	provider := config.ProviderConfig{ID: "synthetic", ProviderOptions: map[string]any{"vendor.count": json.Number("9007199254740995")}}
	options, err := getProviderOptions(model, provider, registration)
	require.NoError(t, err)
	native, ok := options[openai.Name].(*openai.ResponsesProviderOptions)
	require.True(t, ok)
	require.Equal(t, json.Number("9007199254740995"), native.RuntimeControls["/vendor/count"])
	require.Error(t, validateRuntimeControlValue(control, json.Number("0.5")))
}
