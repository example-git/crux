// Derived from Catwalk v0.51.23 and modified by the Crux project.
package foundation

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDecodeProviderPresetAcceptsCatwalkFormat(t *testing.T) {
	preset, err := DecodeProviderPreset([]byte(`{
		"name":"DeepSeek",
		"id":"deepseek",
		"type":"openai-compat",
		"api_key":"$DEEPSEEK_API_KEY",
		"api_endpoint":"https://api.deepseek.com/v1",
		"default_large_model_id":"deepseek-chat",
		"default_small_model_id":"deepseek-chat",
		"models":[{
			"id":"deepseek-chat",
			"name":"DeepSeek Chat",
			"context_window":128000,
			"default_max_tokens":8192,
			"can_reason":true,
			"reasoning_levels":["low","high"],
			"default_reasoning_effort":"high",
			"supports_attachments":false
		}]
	}`))
	require.NoError(t, err)
	require.Equal(t, ProviderID("deepseek"), preset.ID)
	require.Equal(t, ProviderTypeOpenAICompat, preset.Type)
	require.Equal(t, "$DEEPSEEK_API_KEY", preset.APIKey)
	require.Equal(t, "deepseek-chat", preset.Models[0].ID)
}

func TestDecodeProviderPresetRejectsInvalidData(t *testing.T) {
	valid := ProviderPreset{
		Name:                "DeepSeek",
		ID:                  "deepseek",
		Type:                ProviderTypeOpenAICompat,
		APIKey:              "$DEEPSEEK_API_KEY",
		APIEndpoint:         "https://api.deepseek.com/v1",
		DefaultLargeModelID: "deepseek-chat",
		DefaultSmallModelID: "deepseek-chat",
		Models: []ModelPreset{{
			ID: "deepseek-chat", Name: "DeepSeek Chat", ContextWindow: 128000, DefaultMaxTokens: 8192,
		}},
	}
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "unknown field", edit: func(value map[string]any) { value["entrypoint"] = "provider" }, want: "unknown field"},
		{name: "literal secret", edit: func(value map[string]any) { value["api_key"] = "secret" }, want: "environment variable"},
		{name: "unknown type", edit: func(value map[string]any) { value["type"] = "arbitrary" }, want: "unsupported"},
		{name: "missing default", edit: func(value map[string]any) { value["default_large_model_id"] = "missing" }, want: "unknown model"},
		{name: "insecure endpoint", edit: func(value map[string]any) { value["api_endpoint"] = "http://api.example.com" }, want: "HTTPS"},
		{name: "endpoint credentials", edit: func(value map[string]any) { value["api_endpoint"] = "https://user:pass@api.example.com" }, want: "without credentials"},
		{name: "negative cost", edit: func(value map[string]any) { value["models"].([]any)[0].(map[string]any)["cost_per_1m_in"] = -1 }, want: "nonnegative"},
		{name: "header injection", edit: func(value map[string]any) { value["default_headers"] = map[string]any{"X-Test": "ok\r\nbad"} }, want: "invalid value"},
		{name: "no models", edit: func(value map[string]any) { value["models"] = []any{} }, want: "between 1 and 512"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(valid)
			require.NoError(t, err)
			var value map[string]any
			require.NoError(t, json.Unmarshal(data, &value))
			test.edit(value)
			data, err = json.Marshal(value)
			require.NoError(t, err)
			_, err = DecodeProviderPreset(data)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestDecodeProviderPresetAcceptsLoopbackHTTP(t *testing.T) {
	preset := ProviderPreset{
		Name:                "Local",
		ID:                  "local",
		Type:                ProviderTypeOpenAICompat,
		APIEndpoint:         "http://127.0.0.1:8080/v1",
		DefaultLargeModelID: "local",
		DefaultSmallModelID: "local",
		Models:              []ModelPreset{{ID: "local", Name: "Local", ContextWindow: 8192, DefaultMaxTokens: 1024}},
	}
	data, err := json.Marshal(preset)
	require.NoError(t, err)
	_, err = DecodeProviderPreset(data)
	require.NoError(t, err)
}
