package responses

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type lifecycleRequest struct {
	connection    int32
	authorization string
	frame         requestFrame
}

type flakyAcceptListener struct {
	net.Listener
	accepts atomic.Int32
}

func (l *flakyAcceptListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	if l.accepts.Add(1) == 2 {
		_ = conn.Close()
	}
	return conn, nil
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
		if connection < 2 {
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
	require.Equal(t, int32(2), connections.Load())
	for range 2 {
		request := <-requests
		require.Empty(t, request.frame.PreviousResponseID)
		require.Len(t, request.frame.Input, 1)
	}
}

func TestAbnormalClosureRetryDelayIsCapped(t *testing.T) {
	require.Equal(t, 100*time.Millisecond, abnormalClosureRetryDelay(1))
	require.Equal(t, 5*time.Second, abnormalClosureRetryDelay(1_000_000))
}

func TestClientKeepsRecoveringAfterAbnormalClosuresBeyondRetryLimit(t *testing.T) {
	requests := make(chan lifecycleRequest, 4)
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
		if connection < 4 {
			_ = conn.UnderlyingConn().Close()
			return
		}
		writeLifecycleResponse(conn, "resp_recovered", "answer")
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
	require.Equal(t, int32(4), connections.Load())
	for range 4 {
		request := <-requests
		require.Empty(t, request.frame.PreviousResponseID)
		require.Len(t, request.frame.Input, 1)
	}
}

func TestClientAbnormalClosureRecoveryRetriesFailedReconnectDial(t *testing.T) {
	requests := make(chan lifecycleRequest, 2)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			_ = conn.UnderlyingConn().Close()
			return
		}
		writeLifecycleResponse(conn, "resp_recovered", "answer")
	}))
	listener := &flakyAcceptListener{Listener: server.Listener}
	server.Listener = listener
	server.Start()
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
	require.Equal(t, int32(3), listener.accepts.Load())
	require.Equal(t, int32(2), connections.Load())
	for range 2 {
		request := <-requests
		require.Empty(t, request.frame.PreviousResponseID)
		require.Len(t, request.frame.Input, 1)
	}
}

func TestClientAbnormalClosureRecoveryStopsOnCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
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
		_ = conn.UnderlyingConn().Close()
		if connection == 3 {
			cancel()
		}
	}))
	defer server.Close()

	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModel(server, store, func() string { return "token" })
	_, err := model.Generate(ctx, fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(3), connections.Load())
}

func TestClientDoesNotReplayAbnormalClosureAfterResponseEvent(t *testing.T) {
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
		if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "partial"}); err != nil {
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
	require.Equal(t, int32(1), connections.Load())
}

func TestClientRetriesMessageTooBigWithTighterFullReplay(t *testing.T) {
	type observedRequest struct {
		connection int32
		frame      requestFrame
		bytes      int
	}
	requests := make(chan observedRequest, 2)
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connection := connections.Add(1)
		_, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var frame requestFrame
		if err := json.Unmarshal(data, &frame); err != nil {
			return
		}
		requests <- observedRequest{connection: connection, frame: frame, bytes: len(data)}
		if connection == 1 {
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
			return
		}
		writeLifecycleResponse(conn, "resp_retry", "answer")
	}))
	defer server.Close()

	policy := defaultImagePolicyDeclaration()
	policy.HistoryBudget.RequestBytes = 256 * 1024
	policy.HistoryBudget.RetryRequestBytes = 48 * 1024
	policy.HistoryBudget.PerImageTargets = []int64{128 * 1024, 24 * 1024}
	policy.HistoryBudget.OmitOldImages = false
	policy.HistoryBudget.RetainNewestImage = false
	budget, err := compileRequestBudget(policy)
	require.NoError(t, err)
	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModelWithPolicy(t, server, store, func() string { return "token" }, policy)
	imageData := noisyPNG(t, 512)
	_, err = model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("first", fantasy.FilePart{
			Filename: "noise.png", MediaType: "image/png", Data: imageData,
		})},
	})
	require.NoError(t, err)
	first := <-requests
	second := <-requests
	require.NotEqual(t, first.connection, second.connection)
	require.Empty(t, second.frame.PreviousResponseID)
	require.NotEmpty(t, first.frame.ClientMetadata)
	require.NotEmpty(t, second.frame.ClientMetadata)
	require.LessOrEqual(t, first.bytes, budget.requestBytes)
	require.Greater(t, first.bytes, budget.retryRequestBytes)
	require.LessOrEqual(t, second.bytes, budget.retryRequestBytes)
	require.Greater(t, first.bytes, second.bytes)
}

func TestClientDoesNotRetryMessageTooBigWithoutDeclaredRetryBudget(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connections.Add(1)
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, "message too big"), time.Now().Add(time.Second))
	}))
	defer server.Close()

	policy := defaultImagePolicyDeclaration()
	policy.HistoryBudget.RetryRequestBytes = 0
	store := NewSessionStore()
	defer store.Close()
	model := lifecycleModelWithPolicy(t, server, store, func() string { return "token" }, policy)
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorContains(t, err, "request budget is unavailable")
	require.Equal(t, int32(1), connections.Load())
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

func TestReadPumpPreservesBurstsBeyondOldBacklogLimit(t *testing.T) {
	const eventCount = 64
	serverDone := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		for i := 0; i < eventCount; i++ {
			require.NoError(t, conn.WriteJSON(map[string]any{
				"type":  "response.output_text.delta",
				"delta": "x",
			}))
		}
		close(serverDone)
	}))
	defer server.Close()

	url := strings.Replace(server.URL, "http://", "ws://", 1)
	conn, response, err := websocket.DefaultDialer.DialContext(t.Context(), url, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	require.NoError(t, err)
	client := &client{url: url}
	events, stop := client.startReadPump(conn, "test")
	defer func() {
		close(stop)
		_ = conn.Close()
	}()
	<-serverDone
	for i := 0; i < eventCount; i++ {
		select {
		case event := <-events:
			require.NoError(t, event.err)
			require.Contains(t, string(event.data), `"delta":"x"`)
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for event %d", i+1)
		}
	}
}

func TestClientReadIdleTimeoutReconnectsThenFailsClearly(t *testing.T) {
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
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
	}))
	defer server.Close()

	model := lifecycleModel(server, NewSessionStore(), func() string { return "token" })
	model.client.readTimeout = 25 * time.Millisecond
	started := time.Now()
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorContains(t, err, "idle timeout waiting for WebSocket activity")
	require.Equal(t, int32(2), connections.Load())
	require.Less(t, time.Since(started), time.Second)
}

func TestClientRequestTimeoutCancelsBlockedStream(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(response, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		var frame requestFrame
		if err := connection.ReadJSON(&frame); err != nil {
			return
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	model := lifecycleModel(server, NewSessionStore(), func() string { return "token" })
	model.client.requestTimeout = 25 * time.Millisecond
	started := time.Now()
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), time.Second)
}

func TestClientWriteDeadlineBoundsUnresponsivePeer(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if tcpConn, ok := conn.UnderlyingConn().(interface{ SetReadBuffer(int) error }); ok {
			_ = tcpConn.SetReadBuffer(1024)
		}
		<-release
	}))
	defer server.Close()
	defer close(release)

	model := lifecycleModel(server, NewSessionStore(), func() string { return "token" })
	model.client.writeTimeout = 25 * time.Millisecond
	started := time.Now()
	_, err := model.Generate(context.Background(), fantasy.Call{
		Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
		Prompt:  fantasy.Prompt{fantasy.NewUserMessage(strings.Repeat("x", 2<<20))},
	})
	require.ErrorContains(t, err, "i/o timeout")
	require.Less(t, time.Since(started), 2*time.Second)
}

func TestClientCancellationInterruptsBlockedWrite(t *testing.T) {
	release := make(chan struct{})
	writing := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if tcpConn, ok := conn.UnderlyingConn().(interface{ SetReadBuffer(int) error }); ok {
			_ = tcpConn.SetReadBuffer(1024)
		}
		_, reader, err := conn.NextReader()
		if err != nil {
			return
		}
		buffer := make([]byte, 1024)
		if _, err := reader.Read(buffer); err != nil {
			return
		}
		writing <- struct{}{}
		<-release
	}))
	defer server.Close()
	defer close(release)

	model := lifecycleModel(server, NewSessionStore(), func() string { return "token" })
	model.client.writeTimeout = time.Hour
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := model.Generate(ctx, fantasy.Call{
			Headers: map[string]string{"x-session-id": "conversation", "x-request-purpose": "conversation"},
			Prompt:  fantasy.Prompt{fantasy.NewUserMessage(strings.Repeat("x", 8<<20))},
		})
		result <- err
	}()
	<-writing
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("blocked WebSocket write ignored context cancellation")
	}
}

func TestSessionStateLockRespectsCancellation(t *testing.T) {
	state := &sessionState{}
	state.mu.Lock()
	defer state.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	started := time.Now()
	err := lockSessionState(ctx, state)
	require.ErrorIs(t, err, context.Canceled)
	require.Less(t, time.Since(started), 100*time.Millisecond)
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

func lifecycleModelWithPolicy(t *testing.T, server *httptest.Server, store *SessionStore, token func() string, policy *manifest.ImagePolicy) *languageModel {
	created, err := New(
		WithURL(strings.Replace(server.URL, "http://", "ws://", 1)),
		WithTokenSource(token),
		WithSessionStore(store),
		WithOwnerValidator(func() error { return nil }),
		WithImagePolicy(policy),
	)
	require.NoError(t, err)
	modelValue, err := created.LanguageModel(context.Background(), "gpt-test")
	require.NoError(t, err)
	model, ok := modelValue.(*languageModel)
	require.True(t, ok)
	return model
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
