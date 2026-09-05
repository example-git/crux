package responses

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestSessionStateUsesDeltaOnlyForExactContinuation(t *testing.T) {
	initial := testRequestFrame(testMessageItem("user", "first"))
	state := &sessionState{}
	output := json.RawMessage(`{"type":"message","id":"msg_server","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	require.True(t, state.commitLocked(initial, &wireResponse{ID: "resp_1", Output: []json.RawMessage{output}}))

	continuation := testRequestFrame(
		testMessageItem("user", "first"),
		testMessageItem("assistant", "answer"),
		testMessageItem("user", "second"),
	)
	wire, incremental, reason := state.wireRequestLocked(continuation)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, "resp_1", wire.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "second")}, wire.Input)
}

func TestSessionStateChainsOnlyWhenImageHistoryIsUnchanged(t *testing.T) {
	image := messageContent{Type: "input_image", ImageURL: "data:image/png;base64,iVBORw==", Detail: "high"}
	initialItem := inputItem{Type: "message", Role: "user", Content: []messageContent{{Type: "input_text", Text: "inspect"}, image}}
	output := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)

	state := &sessionState{}
	require.True(t, state.commitLocked(testRequestFrame(initialItem), &wireResponse{ID: "resp_1", Output: []json.RawMessage{output}}))
	continuation := testRequestFrame(
		initialItem,
		testMessageItem("assistant", "answer"),
		testMessageItem("user", "next"),
	)
	wire, incremental, reason := state.wireRequestLocked(continuation)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, "resp_1", wire.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "next")}, wire.Input)

	state = &sessionState{}
	require.True(t, state.commitLocked(testRequestFrame(initialItem), &wireResponse{ID: "resp_1", Output: []json.RawMessage{output}}))
	changed := cloneRequestFrame(continuation)
	changed.Input[0].Content[1].ImageURL = "data:image/png;base64,changed"
	wire, incremental, reason = state.wireRequestLocked(changed)
	require.False(t, incremental)
	require.NotEmpty(t, reason)
	require.Empty(t, wire.PreviousResponseID)
}

func TestSessionStateAppendsOnlyChangedDynamicEnvironment(t *testing.T) {
	initial := testRequestFrame(testMessageItem("user", "first"))
	initial.DynamicContext = "Today's date: 8/21/2026\n\nStatus: clean"
	state := &sessionState{}

	full, incremental, _ := state.wireRequestLocked(initial)
	require.False(t, incremental)
	require.Equal(t, []inputItem{
		dynamicEnvironmentItem(initial.DynamicContext),
		testMessageItem("user", "first"),
	}, full.Input)

	firstOutput := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)
	require.True(t, state.commitLocked(initial, &wireResponse{ID: "resp_1", Output: []json.RawMessage{firstOutput}}))

	unchanged := testRequestFrame(
		testMessageItem("user", "first"),
		testMessageItem("assistant", "answer"),
		testMessageItem("user", "second"),
	)
	unchanged.DynamicContext = initial.DynamicContext
	wire, incremental, reason := state.wireRequestLocked(unchanged)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, []inputItem{testMessageItem("user", "second")}, wire.Input)

	changed := cloneRequestFrame(unchanged)
	changed.DynamicContext = "Today's date: 8/22/2026\n\nStatus:\n M main.go"
	wire, incremental, reason = state.wireRequestLocked(changed)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, []inputItem{
		dynamicEnvironmentItem(changed.DynamicContext),
		testMessageItem("user", "second"),
	}, wire.Input)

	secondOutput := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second answer"}]}`)
	require.True(t, state.commitLocked(changed, &wireResponse{ID: "resp_2", Output: []json.RawMessage{secondOutput}}))
	next := testRequestFrame(
		testMessageItem("user", "first"),
		testMessageItem("assistant", "answer"),
		testMessageItem("user", "second"),
		testMessageItem("assistant", "second answer"),
		testMessageItem("user", "third"),
	)
	next.DynamicContext = changed.DynamicContext
	wire, incremental, reason = state.wireRequestLocked(next)
	require.True(t, incremental)
	require.Empty(t, reason)
	require.Equal(t, []inputItem{testMessageItem("user", "third")}, wire.Input)
}

func TestSessionStateFallsBackWhenContextChanges(t *testing.T) {
	base := testRequestFrame(testMessageItem("user", "first"))
	output := json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}`)

	tests := map[string]func(*requestFrame){
		"instructions": func(frame *requestFrame) { frame.Instructions = "changed" },
		"tools": func(frame *requestFrame) {
			frame.Tools = []wireTool{{Type: "function", Name: "new", Parameters: map[string]any{"type": "object"}}}
		},
		"reasoning":        func(frame *requestFrame) { frame.Reasoning.Effort = "high" },
		"verbosity":        func(frame *requestFrame) { frame.Text.Verbosity = "high" },
		"prompt cache key": func(frame *requestFrame) { frame.PromptCacheKey = "changed" },
		"rewritten history": func(frame *requestFrame) {
			frame.Input[0].Content[0].Text = "rewritten"
		},
		"shortened history": func(frame *requestFrame) {
			frame.Input = frame.Input[:1]
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			state := &sessionState{}
			require.True(t, state.commitLocked(base, &wireResponse{ID: "resp_1", Output: []json.RawMessage{output}}))
			continuation := testRequestFrame(
				testMessageItem("user", "first"),
				testMessageItem("assistant", "answer"),
				testMessageItem("user", "second"),
			)
			mutate(continuation)

			wire, incremental, reason := state.wireRequestLocked(continuation)
			require.False(t, incremental)
			require.NotEmpty(t, reason)
			require.Empty(t, wire.PreviousResponseID)
			require.Equal(t, continuation.Input, wire.Input)
			require.Nil(t, state.chain)
		})
	}
}

func TestSessionStateRejectsUnusableCompletedOutput(t *testing.T) {
	logical := testRequestFrame(testMessageItem("user", "first"))
	for name, response := range map[string]*wireResponse{
		"missing response id": {Output: []json.RawMessage{json.RawMessage(`{"type":"message"}`)}},
		"malformed output":    {ID: "resp_1", Output: []json.RawMessage{json.RawMessage(`{`)}},
		"unknown output":      {ID: "resp_1", Output: []json.RawMessage{json.RawMessage(`{"type":"computer_call"}`)}},
	} {
		t.Run(name, func(t *testing.T) {
			state := &sessionState{}
			require.False(t, state.commitLocked(logical, response))
			require.Nil(t, state.chain)
		})
	}
}

func TestSessionStoreIsolatesIdentityAndPurpose(t *testing.T) {
	store := NewSessionStore()
	defer store.Close()

	base := newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation-a", "conversation", "transport-a")
	require.Same(t, store.state(base), store.state(base))
	for name, key := range map[string]transportStateKey{
		"provider":     newTransportStateKey("endpoint", "other", "account-a", "model-a", "conversation-a", "conversation", "transport-a"),
		"account":      newTransportStateKey("endpoint", Name, "account-b", "model-a", "conversation-a", "conversation", "transport-a"),
		"model":        newTransportStateKey("endpoint", Name, "account-a", "model-b", "conversation-a", "conversation", "transport-a"),
		"conversation": newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation-b", "conversation", "transport-a"),
		"purpose":      newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation-a", "title", "transport-a"),
		"handshake":    newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation-a", "conversation", "transport-b"),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotSame(t, store.state(base), store.state(key))
		})
	}
}

func TestSessionStoreInvalidatesReplacedIdentityWithinPurpose(t *testing.T) {
	store := NewSessionStore()
	defer store.Close()

	baseKey := newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation", "conversation", "transport-a")
	titleKey := newTransportStateKey("endpoint", Name, "account-a", "model-a", "conversation", "title", "transport-a")
	base := store.state(baseKey)
	title := store.state(titleKey)
	base.chain = &responseChain{responseID: "conversation-response"}
	title.chain = &responseChain{responseID: "title-response"}

	replacementKey := newTransportStateKey("endpoint", Name, "account-b", "model-b", "conversation", "conversation", "transport-b")
	replacement := store.state(replacementKey)

	require.Nil(t, base.chain)
	require.NotNil(t, title.chain)
	require.NotSame(t, base, replacement)
	store.mu.Lock()
	require.NotContains(t, store.sessions, baseKey)
	require.Contains(t, store.sessions, titleKey)
	require.Contains(t, store.sessions, replacementKey)
	store.mu.Unlock()
}

func TestPromptCacheKeyIsStableOpaqueAndIsolated(t *testing.T) {
	key := promptCacheKey(Name, "account-a", "conversation-a", "conversation")
	require.Equal(t, key, promptCacheKey(Name, "account-a", "conversation-a", "conversation"))
	require.NotEqual(t, key, promptCacheKey("other", "account-a", "conversation-a", "conversation"))
	require.NotEqual(t, key, promptCacheKey(Name, "account-b", "conversation-a", "conversation"))
	require.NotEqual(t, key, promptCacheKey(Name, "account-a", "conversation-b", "conversation"))
	require.NotEqual(t, key, promptCacheKey(Name, "account-a", "conversation-a", "title"))
	require.NotContains(t, key, "account-a")
	require.NotContains(t, key, "conversation-a")
	require.Len(t, key, 64)
}

func TestAccountDiscriminatorIsolatesOpaqueCredentials(t *testing.T) {
	first := accountDiscriminator("", "opaque-token-a")
	require.Equal(t, first, accountDiscriminator("", "opaque-token-a"))
	require.NotEqual(t, first, accountDiscriminator("", "opaque-token-b"))
	require.NotContains(t, first, "opaque-token-a")
	require.NotEqual(t, promptCacheKey(Name, first, "conversation", "conversation"), promptCacheKey(Name, accountDiscriminator("", "opaque-token-b"), "conversation", "conversation"))

	explicit := accountDiscriminator("acct-secret-value", "opaque-token-a")
	require.Equal(t, explicit, accountDiscriminator("acct-secret-value", "opaque-token-b"))
	require.NotContains(t, explicit, "acct-secret-value")
}

func TestSessionStoreResetConversationAndClose(t *testing.T) {
	store := NewSessionStore()
	matchingConversation := store.state(newTransportStateKey("endpoint", Name, "account", "model", "conversation", "conversation", "transport"))
	matchingTitle := store.state(newTransportStateKey("endpoint", Name, "account", "model", "conversation", "title", "transport"))
	otherConversation := store.state(newTransportStateKey("endpoint", Name, "account", "model", "other", "conversation", "transport"))
	matchingConversation.chain = &responseChain{responseID: "resp_conversation"}
	matchingTitle.chain = &responseChain{responseID: "resp_title"}
	otherConversation.chain = &responseChain{responseID: "resp_other"}

	store.resetConversation("endpoint", Name, "account", "conversation")
	require.Nil(t, matchingConversation.chain)
	require.Nil(t, matchingTitle.chain)
	require.NotNil(t, otherConversation.chain)

	store.Close()
	store.Close()
	require.Empty(t, store.sessions)
	require.Nil(t, otherConversation.chain)
}

func TestClientReusesSocketAndChainsOnlyWithinPurpose(t *testing.T) {
	type receivedRequest struct {
		connection int32
		frame      requestFrame
	}
	received := make(chan receivedRequest, 8)
	var connections atomic.Int32
	var responses atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connection := connections.Add(1)
		for {
			var frame requestFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			received <- receivedRequest{connection: connection, frame: frame}
			responseNumber := responses.Add(1)
			answer := "answer-" + string(rune('0'+responseNumber))
			item := map[string]any{
				"type": "message",
				"id":   "msg_server",
				"role": "assistant",
				"content": []map[string]any{{
					"type": "output_text",
					"text": answer,
				}},
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": answer}); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item}); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     "resp_" + string(rune('0'+responseNumber)),
					"output": []any{},
				},
			}); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := &languageModel{
		modelID:  "gpt-test",
		provider: Name,
		client: &client{
			url:          strings.Replace(server.URL, "http://", "ws://", 1),
			token:        func() string { return "token" },
			accountID:    func() string { return "account" },
			sessionStore: store,
		},
	}
	headers := map[string]string{"x-session-id": "conversation-a", "x-request-purpose": "conversation"}
	first, err := model.Generate(context.Background(), fantasy.Call{
		Headers: headers,
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	require.Equal(t, "answer-1", first.Content[0].(fantasy.TextContent).Text)
	firstRequest := <-received
	require.Empty(t, firstRequest.frame.PreviousResponseID)
	require.Len(t, firstRequest.frame.Input, 1)

	second, err := model.Generate(context.Background(), fantasy.Call{
		Headers: headers,
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("first"),
			{
				Role: fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "answer-1"},
				},
			},
			fantasy.NewUserMessage("second"),
		},
	})
	require.NoError(t, err)
	require.Equal(t, "answer-2", second.Content[0].(fantasy.TextContent).Text)
	secondRequest := <-received
	require.Equal(t, firstRequest.connection, secondRequest.connection)
	require.Equal(t, "resp_1", secondRequest.frame.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "second")}, secondRequest.frame.Input)

	_, err = model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation-a", "x-request-purpose": "title"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("title")},
	})
	require.NoError(t, err)
	titleRequest := <-received
	require.NotEqual(t, firstRequest.connection, titleRequest.connection)
	require.Empty(t, titleRequest.frame.PreviousResponseID)
	require.Len(t, titleRequest.frame.Input, 1)

	_, err = model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation-b", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("other")},
	})
	require.NoError(t, err)
	otherRequest := <-received
	require.NotEqual(t, firstRequest.connection, otherRequest.connection)
	require.NotEqual(t, titleRequest.connection, otherRequest.connection)
	require.Equal(t, int32(3), connections.Load())
}

func TestClientRejectsOwnerReplacementBeforeReusedSocketWrite(t *testing.T) {
	received := make(chan struct{}, 2)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		for {
			var frame requestFrame
			if err := conn.ReadJSON(&frame); err != nil {
				return
			}
			received <- struct{}{}
			if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "answer"}); err != nil {
				return
			}
			if err := conn.WriteJSON(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id": "resp_1",
					"output": []map[string]any{{
						"type":    "message",
						"role":    "assistant",
						"content": []map[string]any{{"type": "output_text", "text": "answer"}},
					}},
				},
			}); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)

	var ownerCurrent atomic.Bool
	ownerCurrent.Store(true)
	store := NewSessionStore()
	t.Cleanup(store.Close)
	model := &languageModel{
		modelID:  "gpt-test",
		provider: Name,
		client: &client{
			url:          strings.Replace(server.URL, "http://", "ws://", 1),
			token:        func() string { return "token" },
			accountID:    func() string { return "account" },
			sessionStore: store,
			ownerValidator: func() error {
				if !ownerCurrent.Load() {
					return errors.New("owner changed")
				}
				return nil
			},
		},
	}
	headers := map[string]string{"x-session-id": "conversation-a", "x-request-purpose": "conversation"}
	first, err := model.Generate(t.Context(), fantasy.Call{
		Headers: headers,
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	require.Equal(t, "answer", first.Content[0].(fantasy.TextContent).Text)
	select {
	case <-received:
	case <-time.After(time.Second):
		t.Fatal("first WebSocket frame was not received")
	}

	ownerCurrent.Store(false)
	_, err = model.Generate(t.Context(), fantasy.Call{
		Headers: headers,
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("first"),
			{
				Role:    fantasy.MessageRoleAssistant,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}},
			},
			fantasy.NewUserMessage("second"),
		},
	})
	require.ErrorContains(t, err, "owner changed before WebSocket write")
	require.EqualValues(t, 1, connections.Load())
	select {
	case <-received:
		t.Fatal("stale owner wrote a second frame")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestClientReconnectForcesFullReplay(t *testing.T) {
	type receivedRequest struct {
		connection int32
		frame      requestFrame
	}
	received := make(chan receivedRequest, 4)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connection := connections.Add(1)
		var frame requestFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		received <- receivedRequest{connection: connection, frame: frame}
		item := map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": []map[string]any{{"type": "output_text", "text": "answer"}},
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "answer"})
		_ = conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item})
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp", "output": []any{}}})
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := &languageModel{
		modelID:  "gpt-test",
		provider: Name,
		client: &client{
			url:          strings.Replace(server.URL, "http://", "ws://", 1),
			sessionStore: store,
		},
	}
	headers := map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"}
	_, err := model.Generate(context.Background(), fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
	require.NoError(t, err)
	first := <-received

	require.Eventually(t, func() bool {
		account := accountDiscriminator("", "")
		state := store.state(newTransportStateKey(model.client.url, model.provider, account, model.modelID, "conversation", "conversation", model.client.transportIdentity()))
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.conn != nil && len(state.readEvents) > 0
	}, time.Second, 10*time.Millisecond)

	_, err = model.Generate(context.Background(), fantasy.Call{
		Headers: headers,
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("first"),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}},
			fantasy.NewUserMessage("second"),
		},
	})
	require.NoError(t, err)
	second := <-received
	require.NotEqual(t, first.connection, second.connection)
	require.Empty(t, second.frame.PreviousResponseID)
	require.Len(t, second.frame.Input, 3)
}

func testRequestFrame(items ...inputItem) *requestFrame {
	frame := &requestFrame{
		Type:               "response.create",
		Model:              "gpt-test",
		Instructions:       "instructions",
		Input:              cloneInputItems(items),
		ParallelToolCalls:  true,
		Reasoning:          &wireReasoning{Effort: "medium", Summary: "auto"},
		Stream:             true,
		Include:            []string{"reasoning.encrypted_content"},
		Text:               &wireTextFormat{Verbosity: "medium"},
		PromptCacheKey:     "cache-key",
		PreviousResponseID: "",
	}
	frame.Text.Format.Type = "text"
	return frame
}

func testMessageItem(role, text string) inputItem {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return inputItem{
		Type:    "message",
		Role:    role,
		Content: []messageContent{{Type: contentType, Text: text}},
	}
}
