package responses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type capturedCompactionRequest struct {
	connection int32
	frame      requestFrame
	raw        []byte
}

func TestCompactUsesRemoteV2ResponsesRequestAndUsage(t *testing.T) {
	requests := make(chan capturedCompactionRequest, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/responses", r.URL.Path)
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		var frame requestFrame
		require.NoError(t, json.Unmarshal(raw, &frame))
		requests <- capturedCompactionRequest{connection: 1, frame: frame, raw: raw}
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{
				"type":    "message",
				"role":    "assistant",
				"content": []map[string]any{{"type": "output_text", "text": "ignored"}},
			},
		}))
		writeRemoteV2CompactionResponse(t, conn, "opaque-v2", "resp_compact", map[string]any{
			"input_tokens":  120,
			"output_tokens": 12,
			"total_tokens":  132,
			"input_tokens_details": map[string]any{
				"cached_tokens":      80,
				"cache_write_tokens": 10,
			},
			"output_tokens_details": map[string]any{"reasoning_tokens": 4},
		})
	}))
	defer server.Close()

	model := testCompactionModel(server.URL, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := model.Compact(ctx, fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation"},
		Prompt: fantasy.Prompt{
			fantasy.NewSystemMessage("instructions\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\nToday's date: 8/21/2026\n</env>"),
			fantasy.NewUserMessage("message"),
		},
		Tools: []fantasy.Tool{
			fantasy.FunctionTool{Name: "zeta"},
			fantasy.FunctionTool{Name: "alpha"},
		},
		ProviderOptions: fantasy.ProviderOptions{
			Name: &ProviderOptions{ReasoningEffort: "high", ResponseVerbosity: "low"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, CompactionRemoteV2, result.Implementation)
	require.True(t, result.UsageAvailable)
	require.Equal(t, fantasy.Usage{
		InputTokens:         40,
		OutputTokens:        12,
		TotalTokens:         132,
		ReasoningTokens:     4,
		CacheReadTokens:     80,
		CacheCreationTokens: 10,
	}, result.Usage)
	require.Equal(t, remoteCompactionSummary, result.Summary)
	require.Equal(t, []inputItem{
		testMessageItem("user", "message"),
		{Type: "compaction", EncryptedContent: "opaque-v2"},
	}, result.History.Items)
	require.Positive(t, result.ActiveInputTokens)

	request := <-requests
	require.Equal(t, "response.create", request.frame.Type)
	require.Equal(t, "gpt-test", request.frame.Model)
	require.Equal(t, "instructions\n\n<env>\nWorking directory: /workspace\nIs directory a git repo: no\nPlatform: linux\n</env>", request.frame.Instructions)
	require.Equal(t, []inputItem{
		dynamicEnvironmentItem("Today's date: 8/21/2026"),
		testMessageItem("user", "message"),
		{Type: "compaction_trigger"},
	}, request.frame.Input)
	require.Equal(t, []string{"alpha", "zeta"}, []string{request.frame.Tools[0].Name, request.frame.Tools[1].Name})
	require.Equal(t, "auto", request.frame.ToolChoice)
	require.True(t, request.frame.ParallelToolCalls)
	require.True(t, request.frame.Stream)
	require.Equal(t, []string{"reasoning.encrypted_content"}, request.frame.Include)
	require.Equal(t, "high", request.frame.Reasoning.Effort)
	require.Equal(t, "low", request.frame.Text.Verbosity)
	require.NotEmpty(t, request.frame.PromptCacheKey)
	require.Empty(t, request.frame.PreviousResponseID)
	require.Equal(t, "compaction", requestKindFromMetadata(t, request.frame.ClientMetadata))

	var raw map[string]any
	require.NoError(t, json.Unmarshal(request.raw, &raw))
	store, present := raw["store"]
	require.True(t, present)
	require.Equal(t, false, store)
	require.NotContains(t, string(request.raw), "/responses/compact")
}

func TestCompactRemoteV2UsageIsUnavailableWhenOmitted(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		writeRemoteV2CompactionResponse(t, conn, "opaque", "resp", nil)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := testCompactionModel(server.URL, nil).Compact(ctx, fantasy.Call{})
	require.NoError(t, err)
	require.False(t, result.UsageAvailable)
	require.Equal(t, fantasy.Usage{}, result.Usage)
}

func TestCompactRemoteV2RequiresOneValidCompactionItemAndResponseID(t *testing.T) {
	tests := []struct {
		name       string
		items      []inputItem
		responseID string
		want       string
	}{
		{name: "missing compaction", items: []inputItem{testMessageItem("assistant", "ignored")}, responseID: "resp", want: "expected exactly one"},
		{name: "multiple compactions", items: []inputItem{{Type: "compaction", EncryptedContent: "one"}, {Type: "compaction", EncryptedContent: "two"}}, responseID: "resp", want: "expected exactly one"},
		{name: "empty encrypted content", items: []inputItem{{Type: "compaction"}}, responseID: "resp", want: "expected exactly one"},
		{name: "missing response id", items: []inputItem{{Type: "compaction", EncryptedContent: "opaque"}}, want: "without a response id"},
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
				for _, item := range test.items {
					require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item}))
				}
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type":     "response.completed",
					"response": map[string]any{"id": test.responseID},
				}))
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := testCompactionModel(server.URL, nil).Compact(ctx, fantasy.Call{})
			require.Error(t, err)
			require.Nil(t, result)
			require.Contains(t, err.Error(), test.want)
		})
	}
}

func TestCompactRemoteV2ReusesConversationChainAndDefersReset(t *testing.T) {
	requests := make(chan capturedCompactionRequest, 2)
	var connections atomic.Int32
	var requestCount atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		connection := connections.Add(1)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var frame requestFrame
			require.NoError(t, json.Unmarshal(raw, &frame))
			requests <- capturedCompactionRequest{connection: connection, frame: frame, raw: raw}
			if requestCount.Add(1) == 1 {
				require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "answer"}))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type": "response.output_item.done",
					"item": testMessageItem("assistant", "answer"),
				}))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type":     "response.completed",
					"response": map[string]any{"id": "resp_turn"},
				}))
				continue
			}
			writeRemoteV2CompactionResponse(t, conn, "opaque", "resp_compact", nil)
		}
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := testCompactionModel(server.URL, store)
	headers := map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"}
	prompt := fantasy.Prompt{
		fantasy.NewSystemMessage("instructions"),
		fantasy.NewUserMessage("first"),
	}
	tools := []fantasy.Tool{fantasy.FunctionTool{Name: "view"}}
	options := fantasy.ProviderOptions{Name: &ProviderOptions{ReasoningEffort: "high"}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	response, err := model.Generate(ctx, fantasy.Call{
		Headers:         headers,
		Prompt:          prompt,
		Tools:           tools,
		ProviderOptions: options,
	})
	require.NoError(t, err)
	firstRequest := <-requests
	require.Equal(t, "turn", requestKindFromMetadata(t, firstRequest.frame.ClientMetadata))

	prompt = append(prompt, fantasy.Message{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: response.Content[0].(fantasy.TextContent).Text}},
	})
	result, err := model.Compact(ctx, fantasy.Call{
		Headers: map[string]string{
			"x-session-id":      "conversation",
			"x-request-purpose": "compaction",
		},
		Prompt:          prompt,
		Tools:           tools,
		ProviderOptions: options,
	})
	require.NoError(t, err)
	secondRequest := <-requests
	require.Equal(t, firstRequest.connection, secondRequest.connection)
	require.Equal(t, "resp_turn", secondRequest.frame.PreviousResponseID)
	require.Equal(t, []inputItem{{Type: "compaction_trigger"}}, secondRequest.frame.Input)
	require.Equal(t, "compaction", requestKindFromMetadata(t, secondRequest.frame.ClientMetadata))

	account := accountDiscriminator("account", "token")
	key := newTransportStateKey(model.client.url, model.provider, account, model.modelID, "conversation", "conversation", model.client.transportIdentity())
	state := store.state(key)
	titleState := store.state(newTransportStateKey(model.client.url, model.provider, account, model.modelID, "conversation", "title", model.client.transportIdentity()))
	titleState.mu.Lock()
	titleState.chain = &responseChain{responseID: "title-response"}
	titleState.mu.Unlock()

	state.mu.Lock()
	require.NotNil(t, state.chain)
	require.NotNil(t, state.conn)
	state.mu.Unlock()
	result.Finalize()
	result.Finalize()
	state.mu.Lock()
	require.Nil(t, state.chain)
	require.NotNil(t, state.conn)
	state.mu.Unlock()
	titleState.mu.Lock()
	require.NotNil(t, titleState.chain)
	titleState.mu.Unlock()
	require.Equal(t, int32(1), connections.Load())
}

func TestCompactRemoteV2RetriesPrematureStreamClosure(t *testing.T) {
	requests := make(chan capturedCompactionRequest, 2)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		connection := connections.Add(1)
		_, raw, err := conn.ReadMessage()
		require.NoError(t, err)
		var frame requestFrame
		require.NoError(t, json.Unmarshal(raw, &frame))
		requests <- capturedCompactionRequest{connection: connection, frame: frame, raw: raw}
		if connection == 1 {
			require.NoError(t, conn.WriteJSON(map[string]any{
				"type": "response.output_item.done",
				"item": inputItem{Type: "compaction", EncryptedContent: "discarded"},
			}))
			return
		}
		writeRemoteV2CompactionResponse(t, conn, "accepted", "resp", nil)
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := testCompactionModel(server.URL, store).Compact(ctx, fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("message")},
	})
	require.NoError(t, err)
	require.Equal(t, "accepted", result.History.Items[len(result.History.Items)-1].EncryptedContent)
	first := <-requests
	second := <-requests
	require.NotEqual(t, first.connection, second.connection)
	require.Empty(t, first.frame.PreviousResponseID)
	require.Empty(t, second.frame.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "message"), {Type: "compaction_trigger"}}, first.frame.Input)
	require.Equal(t, first.frame.Input, second.frame.Input)
}

func TestBuildRemoteV2CompactedHistoryRetainsOnlyNewestClientMessages(t *testing.T) {
	boundaryTail := "BOUNDARY-TAIL-🙂"
	boundary := "BOUNDARY-HEAD" + strings.Repeat("boundary", remoteCompactionV2RetainedMessageTokens*compactionBytesPerToken) + boundaryTail
	input := []inputItem{
		testMessageItem("user", "too-old"),
		testMessageItem("assistant", "assistant"),
		{Type: "reasoning", EncryptedContent: "reasoning"},
		{Type: "function_call", CallID: "call"},
		{Type: "function_call_output", CallID: "call"},
		{Type: "compaction", EncryptedContent: "old"},
		{Type: "compaction_trigger"},
		testMessageItem("developer", boundary),
		testMessageItem("system", "system-new"),
		testMessageItem("user", "user-new"),
	}
	compaction := inputItem{Type: "compaction", EncryptedContent: "new"}
	history := buildRemoteV2CompactedHistory(input, compaction)
	require.NotEmpty(t, history)
	require.Equal(t, compaction, history[len(history)-1])

	var retainedText strings.Builder
	for _, item := range history[:len(history)-1] {
		require.Equal(t, "message", item.Type)
		require.Contains(t, []string{"user", "developer", "system"}, item.Role)
		for _, content := range item.Content {
			retainedText.WriteString(content.Text)
		}
	}
	text := retainedText.String()
	require.Contains(t, text, remoteCompactionV2TruncationTag)
	require.Contains(t, text, "BOUNDARY-HEAD")
	require.Contains(t, text, boundaryTail)
	require.Contains(t, text, "system-new")
	require.Contains(t, text, "user-new")
	require.NotContains(t, text, "too-old")
	require.NotContains(t, text, "assistant")
	require.True(t, utf8.ValidString(text))

	tokens := 0
	for _, item := range history[:len(history)-1] {
		tokens += messageItemTextTokens(item)
	}
	require.LessOrEqual(t, tokens, remoteCompactionV2RetainedMessageTokens)
}

func writeRemoteV2CompactionResponse(t *testing.T, conn *websocket.Conn, encryptedContent, responseID string, usage map[string]any) {
	t.Helper()
	require.NoError(t, conn.WriteJSON(map[string]any{
		"type": "response.output_item.done",
		"item": inputItem{Type: "compaction", EncryptedContent: encryptedContent},
	}))
	response := map[string]any{"id": responseID}
	if usage != nil {
		response["usage"] = usage
	}
	require.NoError(t, conn.WriteJSON(map[string]any{
		"type":     "response.completed",
		"response": response,
	}))
}

func requestKindFromMetadata(t *testing.T, metadata map[string]string) string {
	t.Helper()
	var turnMetadata map[string]string
	require.NoError(t, json.Unmarshal([]byte(metadata["x-codex-turn-metadata"]), &turnMetadata))
	return turnMetadata["request_kind"]
}

func testCompactionModel(serverURL string, store *SessionStore) *languageModel {
	return &languageModel{
		modelID:  "gpt-test",
		provider: Name,
		client: &client{
			url:          strings.Replace(serverURL, "http://", "ws://", 1) + "/responses",
			token:        func() string { return "token" },
			accountID:    func() string { return "account" },
			sessionStore: store,
		},
	}
}
