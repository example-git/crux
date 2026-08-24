package responses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestStreamNormalizesCachedTokenUsage(t *testing.T) {
	tests := []struct {
		name           string
		providerInput  int64
		providerCached int64
		providerOutput int64
		wantInput      int64
		wantCacheRead  int64
		wantTotal      int64
	}{
		{name: "no cache", providerInput: 100, providerOutput: 10, wantInput: 100, wantTotal: 110},
		{name: "partial cache", providerInput: 100, providerCached: 80, providerOutput: 10, wantInput: 20, wantCacheRead: 80, wantTotal: 110},
		{name: "fully cached", providerInput: 100, providerCached: 100, providerOutput: 10, wantCacheRead: 100, wantTotal: 110},
		{name: "cached exceeds input", providerInput: 100, providerCached: 120, providerOutput: 10, wantCacheRead: 120, wantTotal: 110},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"usage": map[string]any{
							"input_tokens":  test.providerInput,
							"output_tokens": test.providerOutput,
							"input_tokens_details": map[string]any{
								"cached_tokens": test.providerCached,
							},
						},
					},
				}))
			}))
			defer server.Close()

			model := &languageModel{
				client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)},
			}
			response, err := model.Generate(context.Background(), fantasy.Call{})
			require.NoError(t, err)
			require.Equal(t, test.wantInput, response.Usage.InputTokens)
			require.Equal(t, test.wantCacheRead, response.Usage.CacheReadTokens)
			require.Equal(t, test.providerOutput, response.Usage.OutputTokens)
			require.Equal(t, test.wantTotal, response.Usage.TotalTokens)
		})
	}
}

func TestStreamMarksContextLengthErrors(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"error": map[string]any{
					"code":    "context_length_exceeded",
					"message": "input exceeds context",
				},
			},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	_, err := model.Generate(context.Background(), fantasy.Call{})
	require.Error(t, err)
	var providerError *fantasy.ProviderError
	require.ErrorAs(t, err, &providerError)
	require.True(t, providerError.IsContextTooLarge())
}

func TestStreamRejectsMalformedProtocolEvents(t *testing.T) {
	tests := map[string]string{
		"malformed frame":           `{`,
		"malformed output item":     `{"type":"response.output_item.done","item":"{"}`,
		"completed without payload": `{"type":"response.completed"}`,
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(event)))
			}))
			defer server.Close()

			model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
			_, err := model.Generate(context.Background(), fantasy.Call{})
			require.Error(t, err)
		})
	}
}

func TestStreamSeparatesCodexReasoningSummaryItems(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		for _, event := range []string{
			`{"type":"response.reasoning_summary_part.added"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Planning the first step"}`,
			`{"type":"response.reasoning_summary_part.done"}`,
			`{"type":"response.reasoning_summary_part.added"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Evaluating the next step"}`,
			`{"type":"response.reasoning_summary_part.done"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Using the legacy item boundary"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_2","encrypted_content":"opaque-2","summary":[]}}`,
			`{"type":"response.completed","response":{}}`,
		} {
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(event)))
		}
	}))
	defer server.Close()

	model := &languageModel{
		client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)},
	}
	stream, err := model.Stream(context.Background(), fantasy.Call{})
	require.NoError(t, err)

	var reasoning strings.Builder
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeReasoningDelta {
			reasoning.WriteString(part.Delta)
		}
	}
	require.Equal(t, "Planning the first step\n\nEvaluating the next step\n\nUsing the legacy item boundary", reasoning.String())
}
