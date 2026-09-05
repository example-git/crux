package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	DefaultMaxEventBytes = int64(1 << 20)
	DefaultIdleTimeout   = 60 * time.Second
	defaultEventBuffer   = 32
)

var (
	ErrClosed        = errors.New("Responses WebSocket is closed")
	ErrStreamIdle    = errors.New("Responses WebSocket stream idle timeout")
	ErrStreamBacklog = errors.New("Responses WebSocket stream event backlog exceeded")
)

// WebSocketOptions are host-owned transport bounds. Zero values select bounded
// core defaults rather than consumer-specific policy.
type WebSocketOptions struct {
	MaxEventBytes int64
	IdleTimeout   time.Duration
	EventBuffer   int
}

// WebSocket multiplexes documented Responses streams over one connection.
// Exactly one goroutine reads and one mutex serializes writes, as required by
// gorilla/websocket.
type WebSocket struct {
	conn     *websocket.Conn
	endpoint string
	traceID  string
	opts     WebSocketOptions

	writeMu sync.Mutex
	mu      sync.Mutex
	streams map[string]*Stream
	closed  bool
	err     error
	done    chan struct{}
}

// Stream is one stream_id route on a shared Responses WebSocket.
type Stream struct {
	id     string
	owner  *WebSocket
	events chan streamResult
	once   sync.Once
}

type streamResult struct {
	event Event
	err   error
}

func NewWebSocket(conn *websocket.Conn, options WebSocketOptions) (*WebSocket, error) {
	if conn == nil {
		return nil, fmt.Errorf("Responses WebSocket connection is nil")
	}
	if options.MaxEventBytes <= 0 {
		options.MaxEventBytes = DefaultMaxEventBytes
	}
	if options.IdleTimeout <= 0 {
		options.IdleTimeout = DefaultIdleTimeout
	}
	if options.EventBuffer <= 0 {
		options.EventBuffer = defaultEventBuffer
	}
	conn.SetReadLimit(options.MaxEventBytes)
	client := &WebSocket{conn: conn, endpoint: conn.RemoteAddr().String(), traceID: cruxlog.NewWebSocketTraceID(), opts: options, streams: make(map[string]*Stream), done: make(chan struct{})}
	go client.readLoop()
	return client, nil
}

func (c *WebSocket) Open(ctx context.Context, streamID string, request json.RawMessage) (*Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if streamID == "" {
		streamID = uuid.NewString()
	}
	frame, err := CreateFrame(streamID, request)
	if err != nil {
		return nil, err
	}
	stream := &Stream{id: streamID, owner: c, events: make(chan streamResult, c.opts.EventBuffer)}

	c.mu.Lock()
	if c.closed {
		err := c.err
		if err == nil {
			err = ErrClosed
		}
		c.mu.Unlock()
		return nil, err
	}
	if _, exists := c.streams[streamID]; exists {
		c.mu.Unlock()
		return nil, fmt.Errorf("Responses WebSocket stream %q is already active", streamID)
	}
	c.streams[streamID] = stream
	c.mu.Unlock()

	c.writeMu.Lock()
	err = c.conn.WriteMessage(websocket.TextMessage, frame)
	c.writeMu.Unlock()
	cruxlog.TraceWebSocketFrame(c.traceID, "outbound", c.endpoint, websocket.TextMessage, frame, err)
	if err != nil {
		c.remove(streamID, stream)
		return nil, fmt.Errorf("write Responses WebSocket request: %w", err)
	}
	return stream, nil
}

func (c *WebSocket) Close() error {
	return c.shutdown(ErrClosed)
}

func (c *WebSocket) Done() <-chan struct{} { return c.done }

func (c *WebSocket) readLoop() {
	for {
		messageType, data, err := c.conn.ReadMessage()
		cruxlog.TraceWebSocketFrame(c.traceID, "inbound", c.endpoint, messageType, data, err)
		if err != nil {
			_ = c.shutdown(fmt.Errorf("read Responses WebSocket event: %w", err))
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			continue
		}
		event, err := DecodeEvent(data)
		if err != nil {
			_ = c.shutdown(err)
			return
		}
		if retryErr := classifyRetryableErrorEvent(event); retryErr != nil {
			if event.StreamID == "" {
				_ = c.shutdown(retryErr)
				return
			}
			c.mu.Lock()
			stream := c.streams[event.StreamID]
			c.mu.Unlock()
			if stream != nil {
				c.remove(event.StreamID, stream)
				stream.close(retryErr)
			}
			continue
		}
		if event.StreamID == "" {
			_ = c.shutdown(fmt.Errorf("Responses WebSocket event %q is missing stream_id", event.Type))
			return
		}

		c.mu.Lock()
		stream := c.streams[event.StreamID]
		c.mu.Unlock()
		if stream == nil {
			// A late event for a canceled or completed stream cannot be delivered,
			// but it must not disrupt unrelated multiplexed streams.
			continue
		}
		select {
		case stream.events <- streamResult{event: event}:
			if terminalEvent(event.Type) {
				c.remove(event.StreamID, stream)
				stream.close(nil)
			}
		default:
			c.remove(event.StreamID, stream)
			stream.close(ErrStreamBacklog)
		}
	}
}

func classifyRetryableErrorEvent(event Event) error {
	if event.Type != "error" {
		return nil
	}
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Error   *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(event.Raw, &payload); err != nil {
		return nil
	}
	if payload.Error != nil {
		payload.Code = payload.Error.Code
		payload.Message = payload.Error.Message
	}
	switch {
	case payload.Code == "overloaded_error" || payload.Message == fantasy.ServerOverloadMessage:
		return fantasy.NewServerOverloadError()
	case payload.Code == "websocket_connection_limit_reached" ||
		payload.Code == "connection_limit_reached" ||
		payload.Message == fantasy.ConnectionLimitMessage:
		return fantasy.NewConnectionLimitError()
	default:
		return nil
	}
}

func (c *WebSocket) remove(id string, expected *Stream) {
	c.mu.Lock()
	if c.streams[id] == expected {
		delete(c.streams, id)
	}
	c.mu.Unlock()
}

func (c *WebSocket) shutdown(cause error) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.err = cause
	streams := make([]*Stream, 0, len(c.streams))
	for _, stream := range c.streams {
		streams = append(streams, stream)
	}
	clear(c.streams)
	close(c.done)
	c.mu.Unlock()

	err := c.conn.Close()
	for _, stream := range streams {
		stream.close(cause)
	}
	return err
}

func (s *Stream) ID() string { return s.id }

// Recv waits for the next event, enforcing the stream idle bound independently
// for each multiplexed stream.
func (s *Stream) Recv(ctx context.Context) (Event, error) {
	timer := time.NewTimer(s.owner.opts.IdleTimeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		s.Cancel()
		return Event{}, ctx.Err()
	case <-timer.C:
		s.owner.remove(s.id, s)
		s.close(ErrStreamIdle)
		return Event{}, ErrStreamIdle
	case result, ok := <-s.events:
		if !ok {
			return Event{}, ErrClosed
		}
		return result.event, result.err
	}
}

func (s *Stream) Cancel() {
	s.owner.remove(s.id, s)
	s.close(context.Canceled)
}

func (s *Stream) close(err error) {
	s.once.Do(func() {
		if err != nil {
			select {
			case s.events <- streamResult{err: err}:
			default:
			}
		}
		close(s.events)
	})
}
