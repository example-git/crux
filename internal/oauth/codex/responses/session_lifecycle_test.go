package responses

import (
	"context"
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

type lifecycleRequest struct {
	connection    int32
	authorization string
	frame         requestFrame
}

func TestClientCancellationInvalidatesUncertainState(t *testing.T) {
	requests := make(chan lifecycleRequest, 2)
	firstClosed := make(chan struct{}, 1)
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
		requests <- lifecycleRequest{connection: connection, frame: frame}
		if connection == 1 {
			if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "partial"}); err != nil {
				return
			}
			if _, _, err := conn.ReadMessage(); err != nil {
				firstClosed <- struct{}{}
			}
			return
		}
		writeLifecycleResponse(conn, "resp_2", "answer")
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	headers := map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"}
	ctx, cancel := context.WithCancel(context.Background())
	firstResult := make(chan error, 1)
	go func() {
		_, err := model.Generate(ctx, fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
		firstResult <- err
	}()

	first := <-requests
	cancel()
	require.ErrorIs(t, <-firstResult, context.Canceled)
	require.Eventually(t, func() bool {
		select {
		case <-firstClosed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	_, err := model.Generate(context.Background(), fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("second")}})
	require.NoError(t, err)
	second := <-requests
	require.NotEqual(t, first.connection, second.connection)
	require.Empty(t, second.frame.PreviousResponseID)
	require.Equal(t, []inputItem{testMessageItem("user", "second")}, second.frame.Input)
}

func TestClientSerializesConcurrentRequestsForOneState(t *testing.T) {
	requests := make(chan lifecycleRequest, 2)
	releaseFirst := make(chan struct{})
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
			requestNumber := responses.Add(1)
			requests <- lifecycleRequest{connection: connection, frame: frame}
			if requestNumber == 1 {
				<-releaseFirst
			}
			writeLifecycleResponse(conn, "resp", "answer")
		}
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	headers := map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"}
	firstResult := make(chan error, 1)
	secondResult := make(chan error, 1)
	go func() {
		_, err := model.Generate(context.Background(), fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
		firstResult <- err
	}()
	first := <-requests
	go func() {
		_, err := model.Generate(context.Background(), fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("second")}})
		secondResult <- err
	}()

	select {
	case request := <-requests:
		t.Fatalf("concurrent request reached transport before serialization: %+v", request)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseFirst)
	require.NoError(t, <-firstResult)
	second := <-requests
	require.NoError(t, <-secondResult)
	require.Equal(t, first.connection, second.connection)
}

func TestClientOpaqueCredentialRefreshClosesOldStateAndFullReplays(t *testing.T) {
	requests := make(chan lifecycleRequest, 2)
	firstClosed := make(chan struct{}, 1)
	var connections atomic.Int32
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
				if connection == 1 {
					firstClosed <- struct{}{}
				}
				return
			}
			requests <- lifecycleRequest{connection: connection, authorization: r.Header.Get("Authorization"), frame: frame}
			writeLifecycleResponse(conn, "resp_"+string(rune('0'+connection)), "answer")
		}
	}))
	defer server.Close()

	var token atomic.Value
	token.Store("opaque-token-a")
	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return token.Load().(string) })
	headers := map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"}
	firstResponse, err := model.Generate(context.Background(), fantasy.Call{Headers: headers, Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")}})
	require.NoError(t, err)
	first := <-requests
	require.Equal(t, "Bearer opaque-token-a", first.authorization)

	token.Store("opaque-token-b")
	_, err = model.Generate(context.Background(), fantasy.Call{
		Headers: headers,
		Prompt: fantasy.Prompt{
			fantasy.NewUserMessage("first"),
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: firstResponse.Content[0].(fantasy.TextContent).Text}}},
			fantasy.NewUserMessage("second"),
		},
	})
	require.NoError(t, err)
	second := <-requests
	require.NotEqual(t, first.connection, second.connection)
	require.Equal(t, "Bearer opaque-token-b", second.authorization)
	require.Empty(t, second.frame.PreviousResponseID)
	require.Len(t, second.frame.Input, 3)
	require.Eventually(t, func() bool {
		select {
		case <-firstClosed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
	store.mu.Lock()
	require.Len(t, store.sessions, 1)
	store.mu.Unlock()
}

func TestClientReconnectsAfterAbnormalClosureAndResumesProcessing(t *testing.T) {
	requests := make(chan lifecycleRequest, 3)
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
		requests <- lifecycleRequest{connection: connection, frame: frame}
		if connection < 3 {
			_ = conn.UnderlyingConn().Close()
			return
		}
		writeLifecycleResponse(conn, "resp_reconnected", "answer")
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	response, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	require.Equal(t, "answer", response.Content[0].(fantasy.TextContent).Text)
	require.Equal(t, int32(3), connections.Load())
	for range 3 {
		request := <-requests
		require.Empty(t, request.frame.PreviousResponseID)
		require.Len(t, request.frame.Input, 1)
	}
}

func TestClientStopsAfterAbnormalClosureReconnectLimit(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		var frame requestFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		_ = conn.UnderlyingConn().Close()
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorContains(t, err, "1006")
	require.Equal(t, int32(1+maxAbnormalClosureReconnects), connections.Load())
}

func TestClientRetriesMessageTooBigWithTighterFullReplay(t *testing.T) {
	requests := make(chan lifecycleRequest, 2)
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
		requests <- lifecycleRequest{connection: connection, frame: frame}
		if connection == 1 {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
			return
		}
		writeLifecycleResponse(conn, "resp_retry", "answer")
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	first := <-requests
	second := <-requests
	require.NotEqual(t, first.connection, second.connection)
	require.Empty(t, second.frame.PreviousResponseID)
}

func TestClientReportsRepeatedMessageTooBigClearly(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var frame requestFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorContains(t, err, "server rejected the request as too large")
	require.ErrorContains(t, err, "compact the session")
}

func TestSessionStoreCloseClosesReusableSocket(t *testing.T) {
	peerClosed := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var frame requestFrame
		if err := conn.ReadJSON(&frame); err != nil {
			return
		}
		writeLifecycleResponse(conn, "resp", "answer")
		if _, _, err := conn.ReadMessage(); err != nil {
			peerClosed <- struct{}{}
		}
	}))
	defer server.Close()

	store := NewSessionStore()
	model := lifecycleModel(server, store, func() string { return "token" })
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	store.Close()
	require.Eventually(t, func() bool {
		select {
		case <-peerClosed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)
}

func lifecycleModel(server *httptest.Server, store *SessionStore, token func() string) *languageModel {
	return &languageModel{
		modelID:  "gpt-test",
		provider: Name,
		client: &client{
			url:          strings.Replace(server.URL, "http://", "ws://", 1),
			token:        token,
			sessionStore: store,
		},
	}
}

func writeLifecycleResponse(conn *websocket.Conn, responseID, text string) {
	item := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{{
			"type": "output_text",
			"text": text,
		}},
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": text}); err != nil {
		return
	}
	if err := conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item}); err != nil {
		return
	}
	_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "output": []any{}}})
}
