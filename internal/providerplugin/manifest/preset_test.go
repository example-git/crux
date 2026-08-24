package manifest

import (
	"encoding/json"
	"testing"

	validator "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestDecodePresetStrictAcceptsDataOnlyCatwalkShape(t *testing.T) {
	t.Parallel()

	data := readRepoFile(t, "docs", "provider-plugins", "examples", "deepseek-preset.plugin", "manifest.json")
	value, err := DecodePresetStrict(data)
	require.NoError(t, err)
	require.Equal(t, PluginTypeProviderPreset, value.PluginType)
	require.Equal(t, "deepseek", string(value.Preset.ID))
	require.Equal(t, "openai-compat", string(value.Preset.Type))
	require.Len(t, value.Preset.Models, 2)
}

func TestDecodePresetStrictRejectsUnknownFieldsAndLiteralCredentials(t *testing.T) {
	t.Parallel()

	data := readRepoFile(t, "docs", "provider-plugins", "examples", "deepseek-preset.plugin", "manifest.json")
	var object map[string]any
	require.NoError(t, json.Unmarshal(data, &object))
	object["operations"] = []any{}
	unknown, err := json.Marshal(object)
	require.NoError(t, err)
	_, err = DecodePresetStrict(unknown)
	require.ErrorContains(t, err, "unknown field")

	object = map[string]any{}
	require.NoError(t, json.Unmarshal(data, &object))
	object["preset"].(map[string]any)["api_key"] = "embedded-secret"
	literal, err := json.Marshal(object)
	require.NoError(t, err)
	_, err = DecodePresetStrict(literal)
	require.ErrorContains(t, err, "environment variable reference")
}

func TestPresetSchemaIsStrictAndCurrent(t *testing.T) {
	t.Parallel()

	schemaData, err := PresetSchemaJSON()
	require.NoError(t, err)
	compiled, err := validator.NewCompiler().Compile(schemaData)
	require.NoError(t, err)
	manifestData := readRepoFile(t, "docs", "provider-plugins", "examples", "deepseek-preset.plugin", "manifest.json")
	require.True(t, compiled.ValidateJSON(manifestData).IsValid())

	var object map[string]any
	require.NoError(t, json.Unmarshal(manifestData, &object))
	object["oauth"] = []any{}
	invalid, err := json.Marshal(object)
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(invalid).IsValid())

	checkedIn := readRepoFile(t, "provider-preset-plugin.schema.json")
	require.Equal(t, normalizeSchemaLineEndings(schemaData), normalizeSchemaLineEndings(checkedIn), "run `task schema` to update provider-preset-plugin.schema.json")
}
