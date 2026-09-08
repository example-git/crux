package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/stretchr/testify/require"
)

func TestRequestCachePolicyHTTP(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, scenario := range []struct {
			name    string
			options ProviderOptions
			marker  int
		}{
			{name: "default", marker: 2},
			{name: "fork", options: ProviderOptions{SkipCacheWrite: true}, marker: 1},
			{name: "disabled", options: ProviderOptions{PromptCaching: new(false)}, marker: -1},
		} {
			t.Run(fmt.Sprintf("%s/stream=%v", scenario.name, streaming), func(t *testing.T) {
				var document map[string]any
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					require.NoError(t, json.NewDecoder(r.Body).Decode(&document))
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						for _, event := range []string{
							anthropicSSEEvent("message_start", `{"type":"message_start","message":{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":1,"output_tokens":0}}}`),
							anthropicSSEEvent("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
							anthropicSSEEvent("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}`),
							anthropicSSEEvent("content_block_stop", `{"type":"content_block_stop","index":0}`),
							anthropicSSEEvent("message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`),
							anthropicSSEEvent("message_stop", `{"type":"message_stop"}`),
						} {
							_, _ = fmt.Fprint(w, event)
						}
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-test","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
				}))
				defer server.Close()
				provider, err := New(WithBaseURL(server.URL), WithAPIKey("test"))
				require.NoError(t, err)
				model, err := provider.LanguageModel(context.Background(), "claude-test")
				require.NoError(t, err)
				call := fantasy.Call{
					Prompt:          fantasy.Prompt{fantasy.NewUserMessage("first"), {Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("next")},
					ProviderOptions: fantasy.ProviderOptions{Name: &scenario.options},
				}
				if streaming {
					stream, err := model.Stream(context.Background(), call)
					require.NoError(t, err)
					finished := false
					for part := range stream {
						require.NotEqual(t, fantasy.StreamPartTypeError, part.Type)
						finished = finished || part.Type == fantasy.StreamPartTypeFinish
					}
					require.True(t, finished)
				} else {
					_, err = model.Generate(context.Background(), call)
					require.NoError(t, err)
				}
				require.NotNil(t, document)
				for index, message := range cacheBlocks(document["messages"]) {
					blocks := cacheBlocks(message["content"])
					_, marked := blocks[len(blocks)-1]["cache_control"]
					require.Equal(t, index == scenario.marker, marked)
				}
			})
		}
	}
}

func TestCachePolicyPreservesContent(t *testing.T) {
	body := []byte(`{"system":[{"type":"text","text":"instructions","cache_control":{"type":"ephemeral","ttl":"1h"}}],"messages":[{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning","signature":"signed"},{"type":"redacted_thinking","data":"opaque"},{"type":"server_tool_use","id":"server","name":"web_search","input":{"query":"example"}},{"type":"web_search_tool_result","tool_use_id":"server","content":[]}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"image"}}],"custom":"preserved"}]}]}`)
	encoded, err := ApplyRequestCachePolicy(body, RequestCachePolicy{Enabled: true})
	require.NoError(t, err)
	var original, actual map[string]any
	require.NoError(t, json.Unmarshal(body, &original))
	require.NoError(t, json.Unmarshal(encoded, &actual))
	messages := cacheBlocks(actual["messages"])
	blocks := cacheBlocks(messages[1]["content"])
	require.Equal(t, map[string]any{"type": "ephemeral"}, blocks[0]["cache_control"])
	delete(blocks[0], "cache_control")
	require.Equal(t, original, actual)
}

func TestCacheMarkerValidation(t *testing.T) {
	for _, test := range []struct {
		name      string
		ttls      []any
		wantError string
	}{
		{name: "long then short", ttls: []any{"1h", "5m"}},
		{name: "short then long", ttls: []any{"5m", "1h"}, wantError: "must precede"},
		{name: "too many", ttls: []any{"5m", "5m", "5m", "5m", "5m"}, wantError: "maximum is 4"},
		{name: "invalid", ttls: []any{"2h"}, wantError: "TTL"},
		{name: "number", ttls: []any{60}, wantError: "TTL"},
		{name: "null", ttls: []any{nil}, wantError: "TTL"},
		{name: "empty", ttls: []any{""}, wantError: "TTL"},
	} {
		t.Run(test.name, func(t *testing.T) {
			blocks := make([]any, 0, len(test.ttls))
			for _, ttl := range test.ttls {
				blocks = append(blocks, map[string]any{"cache_control": map[string]any{"type": "ephemeral", "ttl": ttl}})
			}
			err := ValidateCacheMarkers(map[string]any{"system": blocks})
			if test.wantError != "" {
				require.ErrorContains(t, err, test.wantError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCachePolicyRejectsInvalidTTL(t *testing.T) {
	_, err := ParseOptions(map[string]any{"cache_ttl": "2h"})
	require.ErrorContains(t, err, "TTL")
	_, err = New(WithEfficiencyPolicy(EfficiencyPolicy{TTL: "2h"}))
	require.ErrorContains(t, err, "TTL")
	model := languageModel{options: options{efficiency: EfficiencyPolicy{PromptCaching: true}}}
	_, err = model.cacheContext(context.Background(), &ProviderOptions{CacheTTL: "invalid"})
	require.ErrorContains(t, err, "TTL")
}
