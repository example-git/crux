package cmd

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/stretchr/testify/require"
)

func TestConvertPartsPreservesProviderMetadata(t *testing.T) {
	payload := []byte(`{ "number" : 1.00e+2 }`)
	envelope := func(scope message.ProviderMetadataScope) message.ProviderMetadataEnvelope {
		value, err := message.NewProviderMetadataEnvelope("missing.plugin", 17, scope, payload)
		require.NoError(t, err)
		return value
	}

	parts := convertParts([]message.ContentPart{
		message.TextContent{Text: "text", ProviderMetadata: message.ProviderMetadata{envelope(message.ProviderMetadataScopeText), envelope(message.ProviderMetadataScopeCompaction)}},
		message.ReasoningContent{Thinking: "reasoning", ProviderMetadata: message.ProviderMetadata{envelope(message.ProviderMetadataScopeReasoning)}},
		message.ToolCall{ID: "call", Name: "tool", Input: "{}", ProviderExecuted: true, ProviderMetadata: message.ProviderMetadata{envelope(message.ProviderMetadataScopeToolCall)}},
		message.ToolResult{ToolCallID: "call", Name: "tool", Content: "result", ProviderExecuted: true, ProviderMetadata: message.ProviderMetadata{envelope(message.ProviderMetadataScopeToolResult)}},
		message.ProviderMetadataContent{ProviderMetadata: message.ProviderMetadata{envelope(message.ProviderMetadataScopeMessage), envelope(message.ProviderMetadataScopeContinuation)}},
	})

	require.Len(t, parts, 5)
	require.Equal(t, "provider_metadata", parts[4].Type)
	require.True(t, parts[2].ProviderExecuted)
	require.True(t, parts[3].ProviderExecuted)

	encoded, err := json.Marshal(parts)
	require.NoError(t, err)
	var decoded []sessionShowPart
	require.NoError(t, json.Unmarshal(encoded, &decoded))

	var count int
	for _, part := range decoded {
		for _, metadata := range part.ProviderMetadata {
			count++
			require.True(t, bytes.Equal(payload, metadata.Payload), "payload changed: %q", metadata.Payload)
		}
	}
	require.Equal(t, 7, count)
}
