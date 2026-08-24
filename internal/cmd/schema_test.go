package cmd

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/invopop/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestSchemaNoBrokenRefs(t *testing.T) {
	t.Parallel()

	reflector := new(jsonschema.Reflector)
	bts, err := json.Marshal(reflector.Reflect(&config.Config{}))
	require.NoError(t, err)

	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(bts, &schema))
	require.NotEmpty(t, schema.Defs, "schema should have definitions")

	for name := range schema.Defs {
		require.NotContains(t, name, "/", "schema $def key %q contains '/' which breaks JSON Pointer $ref resolution", name)
	}
}

func TestConfigurationSchemaIsIndependentOfRuntimeProviders(t *testing.T) {
	t.Parallel()

	encoded, err := configurationSchemaJSON(nil)
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	defs := document["$defs"].(map[string]any)
	cfg := defs["Config"].(map[string]any)
	providers := cfg["properties"].(map[string]any)["providers"].(map[string]any)
	require.Empty(t, providers["properties"], "checked-in schema must not expose locally installed provider IDs")
}

func TestConfigurationSchemaIncludesProviderSurface(t *testing.T) {
	t.Parallel()

	encoded, err := configurationSchemaJSON([]providerregistry.Surface{{
		ID: "synthetic",
		Configuration: map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"client_id": map[string]any{"type": "string"},
			},
		},
		ConfigurationUI: map[string]manifest.FieldDisplay{
			"client_id": {Label: "Client ID", Secret: true, Order: 10},
		},
	}})
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	defs := document["$defs"].(map[string]any)
	cfg := defs["Config"].(map[string]any)
	providers := cfg["properties"].(map[string]any)["providers"].(map[string]any)
	require.Contains(t, providers["properties"].(map[string]any), "synthetic")
	require.Contains(t, string(encoded), "x-crux-fields")
	require.Contains(t, string(encoded), "Client ID")
}

func TestConfigurationSchemaLimitsProviderTypes(t *testing.T) {
	t.Parallel()

	encoded, err := configurationSchemaJSON(nil)
	require.NoError(t, err)

	var document map[string]any
	require.NoError(t, json.Unmarshal(encoded, &document))
	defs := document["$defs"].(map[string]any)
	provider := defs["ProviderConfig"].(map[string]any)
	typeProperty := provider["properties"].(map[string]any)["type"].(map[string]any)
	enum := typeProperty["enum"].([]any)

	require.Contains(t, enum, "openai-compat")
	for _, removed := range []string{"openai", "anthropic", "azure", "bedrock", "google", "google-vertex", "openrouter", "vercel"} {
		require.NotContains(t, enum, removed)
	}
}

func TestSchemaProvidersHasAdditionalProperties(t *testing.T) {
	t.Parallel()

	reflector := new(jsonschema.Reflector)
	bts, err := json.Marshal(reflector.Reflect(&config.Config{}))
	require.NoError(t, err)

	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	require.NoError(t, json.Unmarshal(bts, &schema))

	var cfg struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(schema.Defs["Config"], &cfg))

	providersRaw, ok := cfg.Properties["providers"]
	require.True(t, ok, "Config should have a providers property")

	var providers struct {
		Type                 string          `json:"type"`
		AdditionalProperties json.RawMessage `json:"additionalProperties"`
	}
	require.NoError(t, json.Unmarshal(providersRaw, &providers))
	require.Equal(t, "object", providers.Type)
	require.True(t, strings.Contains(string(providers.AdditionalProperties), "ProviderConfig"),
		"providers should use additionalProperties with a ProviderConfig ref, got: %s", string(providers.AdditionalProperties))
}
