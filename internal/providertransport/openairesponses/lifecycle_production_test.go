package openairesponses

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestLifecycleModelProductionObjectContinuationAndMetadata(t *testing.T) {
	const namespace = "synthetic.responses.continuation"
	var mu sync.Mutex
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		var document map[string]any
		require.NoError(t, json.Unmarshal(body, &document))
		mu.Lock()
		requests = append(requests, document)
		index := len(requests)
		mu.Unlock()

		response.Header().Set("Content-Type", "text/event-stream")
		responseID := fmt.Sprintf("resp_%d", index)
		value := "one"
		if index == 2 {
			value = "two"
		}
		_, _ = fmt.Fprintf(response, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", responseID)
		_, _ = fmt.Fprintf(response, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg_%d\",\"delta\":%q}\n\n", index, fmt.Sprintf(`{"answer":%q}`, value))
		_, _ = fmt.Fprintf(response, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", responseID)
	}))
	defer server.Close()

	provider, err := openai.New(
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(func(string) bool { return true }),
	)
	require.NoError(t, err)
	inner, err := provider.LanguageModel(t.Context(), "gpt-test")
	require.NoError(t, err)
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		MetadataNamespace:    namespace,
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy)
	schema := fantasy.Schema{
		Type:       "object",
		Properties: map[string]*fantasy.Schema{"answer": {Type: "string"}},
		Required:   []string{"answer"},
	}
	first := fantasy.ObjectCall{
		Prompt:  testPrompt("one"),
		Schema:  schema,
		Headers: map[string]string{"x-session-id": "production-object-session"},
	}
	firstFinish := consumeObjectStream(t, model, first)
	require.Equal(t, "resp_1", metadataResponseID(t, firstFinish.ProviderMetadata, namespace))

	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: `{"answer":"one"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
	)
	secondFinish := consumeObjectStream(t, model, followup)
	require.Equal(t, "resp_2", metadataResponseID(t, secondFinish.ProviderMetadata, namespace))

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 2)
	require.Equal(t, true, requests[0]["store"])
	require.NotContains(t, requests[0], "previous_response_id")
	require.Equal(t, "resp_1", requests[1]["previous_response_id"])
	require.Equal(t, true, requests[1]["store"])
	input, ok := requests[1]["input"].([]any)
	require.True(t, ok)
	require.Len(t, input, 1)
}

func consumeObjectStream(t *testing.T, model fantasy.LanguageModel, call fantasy.ObjectCall) fantasy.ObjectStreamPart {
	t.Helper()
	stream, err := model.StreamObject(t.Context(), call)
	require.NoError(t, err)
	var finish fantasy.ObjectStreamPart
	for part := range stream {
		require.NotEqual(t, fantasy.ObjectStreamPartTypeError, part.Type)
		if part.Type == fantasy.ObjectStreamPartTypeFinish {
			finish = part
		}
	}
	require.Equal(t, fantasy.ObjectStreamPartTypeFinish, finish.Type)
	return finish
}
