package localaddon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type websocketJSONWriter struct {
	connection *websocket.Conn
	mu         sync.Mutex
	buffer     bytes.Buffer
}

func (w *websocketJSONWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	written := len(data)
	if _, err := w.buffer.Write(data); err != nil {
		return 0, err
	}
	for {
		line, err := w.buffer.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				_, _ = w.buffer.Write(line)
				return written, nil
			}
			return 0, err
		}
		line = bytes.TrimSuffix(line, []byte{'\n'})
		if len(line) == 0 {
			continue
		}
		if err := w.connection.WriteMessage(websocket.TextMessage, line); err != nil {
			return 0, err
		}
	}
}

func runClaudeSDK(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	endpoint := request.Metadata["sdk-url"]
	connection, response, err := websocket.DefaultDialer.DialContext(ctx, endpoint, nil)
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	if err != nil {
		if response != nil {
			return fmt.Errorf("connect Claude SDK transport: status %s: %w", response.Status, err)
		}
		return fmt.Errorf("connect Claude SDK transport: %w", err)
	}
	defer connection.Close()

	reader, writer := io.Pipe()
	readErrors := make(chan error, 1)
	go func() {
		defer writer.Close()
		if request.Prompt.Text != "" {
			initial, _ := json.Marshal(map[string]any{"type": "user", "uuid": uuid.NewString(), "message": map[string]any{"role": "user", "content": request.Prompt.Text}})
			if _, err := writer.Write(append(initial, '\n')); err != nil {
				readErrors <- err
				return
			}
		}
		for {
			messageType, data, err := connection.ReadMessage()
			if err != nil {
				readErrors <- err
				return
			}
			if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
				continue
			}
			data = bytes.TrimRight(data, "\r\n")
			if len(data) == 0 {
				continue
			}
			if _, err := writer.Write(append(data, '\n')); err != nil {
				readErrors <- err
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		_ = connection.Close()
	}()

	remoteInvocation := invocation
	remoteInvocation.Stdin = reader
	remoteInvocation.Stdout = &websocketJSONWriter{connection: connection}
	request.Prompt.Stdin = reader
	runErr := runClaudeStream(ctx, remoteInvocation, request)
	if runErr != nil {
		return runErr
	}
	select {
	case readErr := <-readErrors:
		if websocket.IsCloseError(readErr, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			return nil
		}
		return readErr
	default:
		return nil
	}
}
