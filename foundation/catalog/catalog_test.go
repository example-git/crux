package catalog

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderAndModelJSONCompatibility(t *testing.T) {
	temperature := 0.2
	topP := 0.7
	topK := int64(40)
	frequencyPenalty := 0.1
	presencePenalty := 0.3
	provider := Provider{
		Name:                "Example",
		ID:                  "example",
		APIKey:              "example-key",
		APIEndpoint:         "https://example.invalid/v1",
		Type:                TypeOpenAICompat,
		DefaultLargeModelID: "example-large",
		DefaultSmallModelID: "example-small",
		Models: []Model{{
			ID:                     "example-large",
			Name:                   "Example Large",
			CostPer1MIn:            1.25,
			CostPer1MOut:           2.5,
			CostPer1MInCached:      0.25,
			CostPer1MOutCached:     0.5,
			ContextWindow:          128000,
			DefaultMaxTokens:       8192,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "high"},
			DefaultReasoningEffort: "high",
			SupportsImages:         true,
			Options: ModelOptions{
				Temperature:      &temperature,
				TopP:             &topP,
				TopK:             &topK,
				FrequencyPenalty: &frequencyPenalty,
				PresencePenalty:  &presencePenalty,
				ProviderOptions:  map[string]any{"mode": "fast", "nested": map[string]any{"enabled": true}},
			},
		}},
		DefaultHeaders: map[string]string{"X-Example": "value"},
	}

	data, err := json.Marshal(provider)
	require.NoError(t, err)
	require.JSONEq(t, `{
		"name":"Example",
		"id":"example",
		"api_key":"example-key",
		"api_endpoint":"https://example.invalid/v1",
		"type":"openai-compat",
		"default_large_model_id":"example-large",
		"default_small_model_id":"example-small",
		"models":[{
			"id":"example-large",
			"name":"Example Large",
			"cost_per_1m_in":1.25,
			"cost_per_1m_out":2.5,
			"cost_per_1m_in_cached":0.25,
			"cost_per_1m_out_cached":0.5,
			"context_window":128000,
			"default_max_tokens":8192,
			"can_reason":true,
			"reasoning_levels":["low","high"],
			"default_reasoning_effort":"high",
			"supports_attachments":true,
			"options":{
				"temperature":0.2,
				"top_p":0.7,
				"top_k":40,
				"frequency_penalty":0.1,
				"presence_penalty":0.3,
				"provider_options":{"mode":"fast","nested":{"enabled":true}}
			}
		}],
		"default_headers":{"X-Example":"value"}
	}`, string(data))

	var decoded Provider
	require.NoError(t, json.Unmarshal(data, &decoded))
	require.Equal(t, provider, decoded)

	data, err = json.Marshal(Model{ID: "empty", Name: "Empty"})
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &fields))
	require.Contains(t, fields, "supports_attachments")
	require.NotContains(t, fields, "options")
}

func TestProviderIDConstants(t *testing.T) {
	require.Equal(t, []ProviderID{
		"openai",
		"anthropic",
		"synthetic",
		"gemini",
		"azure",
		"bedrock",
		"bedrock-europe",
		"vertexai",
		"xai",
		"zai",
		"deepseek",
		"zhipu",
		"zhipu-coding",
		"groq",
		"openrouter",
		"cerebras",
		"venice",
		"chutes",
		"huggingface",
		"aihubmix",
		"kimi-coding",
		"copilot",
		"cortecs",
		"vercel",
		"minimax",
		"minimax-china",
		"ionet",
		"qiniucloud",
		"avian",
		"nebius",
		"neuralwatt",
		"opencode-zen",
		"opencode-go",
		"alibaba-singapore",
		"alibaba-us",
		"fireworks",
		"baseten",
		"moonshot",
		"atlascloud",
	}, []ProviderID{
		ProviderOpenAI,
		ProviderAnthropic,
		ProviderSynthetic,
		ProviderGemini,
		ProviderAzure,
		ProviderBedrock,
		ProviderBedrockEurope,
		ProviderVertexAI,
		ProviderXAI,
		ProviderZAI,
		ProviderDeepSeek,
		ProviderZhipu,
		ProviderZhipuCoding,
		ProviderGROQ,
		ProviderOpenRouter,
		ProviderCerebras,
		ProviderVenice,
		ProviderChutes,
		ProviderHuggingFace,
		ProviderAIHubMix,
		ProviderKimiCoding,
		ProviderCopilot,
		ProviderCortecs,
		ProviderVercel,
		ProviderMiniMax,
		ProviderMiniMaxChina,
		ProviderIoNet,
		ProviderQiniuCloud,
		ProviderAvian,
		ProviderNebius,
		ProviderNeuralwatt,
		ProviderOpenCodeZen,
		ProviderOpenCodeGo,
		ProviderAlibabaSingapore,
		ProviderAlibabaUS,
		ProviderFireworks,
		ProviderBaseten,
		ProviderMoonshot,
		ProviderAtlasCloud,
	})
}

func TestKnownProvidersAndTypes(t *testing.T) {
	require.Equal(t, []ProviderID{
		ProviderOpenAI,
		ProviderSynthetic,
		ProviderAnthropic,
		ProviderGemini,
		ProviderAzure,
		ProviderBedrock,
		ProviderBedrockEurope,
		ProviderVertexAI,
		ProviderXAI,
		ProviderZAI,
		ProviderZhipu,
		ProviderZhipuCoding,
		ProviderGROQ,
		ProviderOpenRouter,
		ProviderCerebras,
		ProviderVenice,
		ProviderChutes,
		ProviderHuggingFace,
		ProviderAIHubMix,
		ProviderKimiCoding,
		ProviderCopilot,
		ProviderCortecs,
		ProviderVercel,
		ProviderMiniMax,
		ProviderMiniMaxChina,
		ProviderQiniuCloud,
		ProviderAvian,
		ProviderNebius,
		ProviderNeuralwatt,
		ProviderOpenCodeZen,
		ProviderOpenCodeGo,
		ProviderFireworks,
		ProviderBaseten,
		ProviderMoonshot,
		ProviderAtlasCloud,
	}, KnownProviders())
	require.Equal(t, []Type{
		TypeOpenAI,
		TypeOpenAICompat,
		TypeOpenRouter,
		TypeVercel,
		TypeAnthropic,
		TypeGoogle,
		TypeAzure,
		TypeBedrock,
		TypeVertexAI,
	}, KnownProviderTypes())
}
