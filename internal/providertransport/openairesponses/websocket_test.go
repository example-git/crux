package openairesponses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestWebSocketMultiplexesInterleavedStreamIDs(t *testing.T) {
	server := websocketServer(t, func(conn *websocket.Conn) {
		ids := make([]string, 0, 2)
		for range 2 {
			_, data, err := conn.ReadMessage()
			require.NoError(t, err)
			var frame struct {
				Type     string `json:"type"`
				StreamID string `json:"stream_id"`
			}
			require.NoError(t, json.Unmarshal(data, &frame))
			require.Equal(t, CreateEventType, frame.Type)
			ids = append(ids, frame.StreamID)
		}
		writeEvent(t, conn, ids[1], "response.output_text.delta", `,"delta":"second"`)
		writeEvent(t, conn, ids[0], "response.output_text.delta", `,"delta":"first"`)
		writeEvent(t, conn, ids[1], "response.completed", `,"response":{"id":"resp_b"}`)
		writeEvent(t, conn, ids[0], "response.completed", `,"response":{"id":"resp_a"}`)
	})
	defer server.Close()

	client := dialWebSocket(t, server, WebSocketOptions{IdleTimeout: time.Second})
	defer client.Close()
	first, err := client.Open(context.Background(), "stream-a", json.RawMessage(`{"model":"a"}`))
	require.NoError(t, err)
	second, err := client.Open(context.Background(), "stream-b", json.RawMessage(`{"model":"b"}`))
	require.NoError(t, err)

	event, err := first.Recv(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stream-a", event.StreamID)
	require.Contains(t, string(event.Raw), `"delta":"first"`)
	event, err = second.Recv(context.Background())
	require.NoError(t, err)
	require.Equal(t, "stream-b", event.StreamID)
	require.Contains(t, string(event.Raw), `"delta":"second"`)

	event, err = first.Recv(context.Background())
	require.NoError(t, err)
	require.Equal(t, "response.completed", event.Type)
	event, err = second.Recv(context.Background())
	require.NoError(t, err)
	require.Equal(t, "response.completed", event.Type)
}

func TestWebSocketRejectsDuplicateStreamAndIgnoresLateUnknownStream(t *testing.T) {
	server := websocketServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
		writeEvent(t, conn, "already-canceled", "response.output_text.delta", `,"delta":"late"`)
		writeEvent(t, conn, "stream-a", "response.completed", `,"response":{"id":"resp_a"}`)
	})
	defer server.Close()

	client := dialWebSocket(t, server, WebSocketOptions{IdleTimeout: time.Second})
	defer client.Close()
	stream, err := client.Open(context.Background(), "stream-a", json.RawMessage(`{"model":"a"}`))
	require.NoError(t, err)
	_, err = client.Open(context.Background(), "stream-a", json.RawMessage(`{"model":"a"}`))
	require.ErrorContains(t, err, "already active")
	event, err := stream.Recv(context.Background())
	require.NoError(t, err)
	require.Equal(t, "response.completed", event.Type)
}

func TestWebSocketMissingStreamIDClosesActiveStreams(t *testing.T) {
	server := websocketServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed"}`)))
	})
	defer server.Close()

	client := dialWebSocket(t, server, WebSocketOptions{IdleTimeout: time.Second})
	stream, err := client.Open(context.Background(), "stream-a", json.RawMessage(`{"model":"a"}`))
	require.NoError(t, err)
	_, err = stream.Recv(context.Background())
	require.ErrorContains(t, err, "missing stream_id")
}

func TestWebSocketEnforcesEventBound(t *testing.T) {
	server := websocketServer(t, func(conn *websocket.Conn) {
		_, _, err := conn.ReadMessage()
		require.NoError(t, err)
		payload := `{"type":"response.output_text.delta","stream_id":"stream-a","delta":"` + strings.Repeat("x", 256) + `"}`
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
	})
	defer server.Close()

	client := dialWebSocket(t, server, WebSocketOptions{MaxEventBytes: 64, IdleTimeout: time.Second})
	stream, err := client.Open(context.Background(), "stream-a", json.RawMessage(`{"model":"a"}`))
	require.NoError(t, err)
	_, err = stream.Recv(context.Background())
	require.Error(t, err)
}

func TestWebSocketStreamIdleAndCancellationAreIsolated(t *testing.T) {
	server := websocketServer(t, func(conn *websocket.Conn) {
		for range 2 {
			_, _, err := conn.ReadMessage()
			require.NoError(t, err)
		}
		time.Sleep(200 * time.Millisecond)
	})
	defer server.Close()

	client := dialWebSocket(t, server, WebSocketOptions{IdleTimeout: 25 * time.Millisecond})
	defer client.Close()
	idle, err := client.Open(context.Background(), "idle", json.RawMessage(`{"model":"a"}`))
	require.NoError(t, err)
	canceled, err := client.Open(context.Background(), "canceled", json.RawMessage(`{"model":"b"}`))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = canceled.Recv(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = idle.Recv(context.Background())
	require.ErrorIs(t, err, ErrStreamIdle)
}

func websocketServer(t *testing.T, run func(*websocket.Conn)) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		run(conn)
	}))
}

func dialWebSocket(t *testing.T, server *httptest.Server, options WebSocketOptions) *WebSocket {
	t.Helper()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	conn, response, err := websocket.DefaultDialer.Dial(endpoint, nil)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.NoError(t, err)
	client, err := NewWebSocket(conn, options)
	require.NoError(t, err)
	return client
}

func writeEvent(t *testing.T, conn *websocket.Conn, streamID, eventType, fields string) {
	t.Helper()
	payload := `{"type":"` + eventType + `","stream_id":"` + streamID + `"` + fields + `}`
	require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
}
