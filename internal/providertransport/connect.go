package providertransport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"time"
)

var ErrStreamIdleTimeout = errors.New("stream idle timeout")

type connectTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

type connectTimeoutReadCloser struct {
	io.ReadCloser
	cancel context.CancelCauseFunc
}

func TransportWithConnectTimeout(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	return connectTimeoutTransport{base: base, timeout: timeout}
}

func (transport connectTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	if transport.timeout <= 0 {
		return base.RoundTrip(request)
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	timeoutErr := fmt.Errorf("connect timeout after %s", transport.timeout)
	timer := time.AfterFunc(transport.timeout, func() { cancel(timeoutErr) })
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { timer.Stop() },
	})
	response, err := base.RoundTrip(request.Clone(ctx))
	timer.Stop()
	if err != nil {
		cause := context.Cause(ctx)
		cancel(nil)
		if cause == timeoutErr {
			return response, timeoutErr
		}
		return response, err
	}
	if response == nil || response.Body == nil {
		cancel(nil)
		return response, nil
	}
	response.Body = &connectTimeoutReadCloser{ReadCloser: response.Body, cancel: cancel}
	return response, nil
}

func (body *connectTimeoutReadCloser) Close() error {
	err := body.ReadCloser.Close()
	body.cancel(nil)
	return err
}

type streamIdleTimeoutTransport struct {
	base    http.RoundTripper
	timeout time.Duration
}

type streamIdleTimeoutReadCloser struct {
	reader          *io.PipeReader
	source          io.ReadCloser
	state           *streamIdleState
	closeOnce       sync.Once
	closeErr        error
	sourceCloseOnce sync.Once
	sourceCloseErr  error
}

type streamIdleState struct {
	mu         sync.Mutex
	timer      *time.Timer
	timeout    time.Duration
	deadline   time.Time
	pausedAt   time.Time
	paused     bool
	done       bool
	timedOut   bool
	cancel     context.CancelCauseFunc
	timeoutErr error
	onTimeout  func()
}

type streamIdleBoundary struct {
	lineEmpty  bool
	previousCR bool
}

func TransportWithStreamIdleTimeout(base http.RoundTripper, timeout time.Duration) http.RoundTripper {
	return streamIdleTimeoutTransport{base: base, timeout: timeout}
}

func (transport streamIdleTimeoutTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	base := transport.base
	if base == nil {
		base = http.DefaultTransport
	}
	if transport.timeout <= 0 {
		return base.RoundTrip(request)
	}
	ctx, cancel := context.WithCancelCause(request.Context())
	response, err := base.RoundTrip(request.Clone(ctx))
	if err != nil {
		cancel(nil)
		return response, err
	}
	if response == nil || response.Body == nil {
		cancel(nil)
		return response, nil
	}
	if !isEventStream(response.Header.Get("Content-Type")) {
		response.Body = &connectTimeoutReadCloser{ReadCloser: response.Body, cancel: cancel}
		return response, nil
	}
	timeoutErr := fmt.Errorf("%w after %s", ErrStreamIdleTimeout, transport.timeout)
	response.Body = newStreamIdleTimeoutReadCloser(response.Body, cancel, transport.timeout, timeoutErr)
	return response, nil
}

func isEventStream(contentType string) bool {
	mediaType, _, _ := strings.Cut(contentType, ";")
	return strings.EqualFold(strings.TrimSpace(mediaType), "text/event-stream")
}

func newStreamIdleTimeoutReadCloser(source io.ReadCloser, cancel context.CancelCauseFunc, timeout time.Duration, timeoutErr error) *streamIdleTimeoutReadCloser {
	reader, writer := io.Pipe()
	body := &streamIdleTimeoutReadCloser{reader: reader, source: source}
	body.state = newStreamIdleState(timeout, cancel, timeoutErr, func() { _ = body.closeSource() })
	go body.pump(writer)
	return body
}

func newStreamIdleState(timeout time.Duration, cancel context.CancelCauseFunc, timeoutErr error, onTimeout func()) *streamIdleState {
	state := &streamIdleState{
		timeout:    timeout,
		deadline:   time.Now().Add(timeout),
		cancel:     cancel,
		timeoutErr: timeoutErr,
		onTimeout:  onTimeout,
	}
	state.mu.Lock()
	state.scheduleLocked(timeout)
	state.mu.Unlock()
	return state
}

func (state *streamIdleState) scheduleLocked(delay time.Duration) {
	if state.timer != nil {
		state.timer.Stop()
	}
	state.timer = time.AfterFunc(delay, state.expire)
}

func (state *streamIdleState) expire() {
	state.mu.Lock()
	if state.done || state.paused {
		state.mu.Unlock()
		return
	}
	remaining := time.Until(state.deadline)
	if remaining > 0 {
		state.scheduleLocked(remaining)
		state.mu.Unlock()
		return
	}
	state.done = true
	state.timedOut = true
	state.mu.Unlock()
	state.cancel(state.timeoutErr)
	state.onTimeout()
}

func (state *streamIdleState) recordEvent(now time.Time) bool {
	state.mu.Lock()
	if state.done {
		state.mu.Unlock()
		return false
	}
	if !now.Before(state.deadline) {
		state.done = true
		state.timedOut = true
		if state.timer != nil {
			state.timer.Stop()
		}
		state.mu.Unlock()
		state.cancel(state.timeoutErr)
		state.onTimeout()
		return false
	}
	state.deadline = now.Add(state.timeout)
	if !state.paused {
		state.scheduleLocked(state.timeout)
	}
	state.mu.Unlock()
	return true
}

func (state *streamIdleState) pause(now time.Time) bool {
	state.mu.Lock()
	if state.done {
		state.mu.Unlock()
		return false
	}
	if !now.Before(state.deadline) {
		state.done = true
		state.timedOut = true
		if state.timer != nil {
			state.timer.Stop()
		}
		state.mu.Unlock()
		state.cancel(state.timeoutErr)
		state.onTimeout()
		return false
	}
	state.paused = true
	state.pausedAt = now
	if state.timer != nil {
		state.timer.Stop()
	}
	state.mu.Unlock()
	return true
}

func (state *streamIdleState) resume(now time.Time) {
	state.mu.Lock()
	if state.done || !state.paused {
		state.mu.Unlock()
		return
	}
	state.deadline = state.deadline.Add(now.Sub(state.pausedAt))
	state.paused = false
	state.scheduleLocked(time.Until(state.deadline))
	state.mu.Unlock()
}

func (state *streamIdleState) finish() {
	state.mu.Lock()
	if state.done {
		state.mu.Unlock()
		return
	}
	state.done = true
	if state.timer != nil {
		state.timer.Stop()
	}
	state.mu.Unlock()
	state.cancel(nil)
}

func (state *streamIdleState) err() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.timedOut {
		return state.timeoutErr
	}
	return nil
}

func (boundary *streamIdleBoundary) observe(data []byte, event func()) {
	for _, value := range data {
		switch {
		case value == '\n' && boundary.previousCR:
			boundary.previousCR = false
		case value == '\r':
			if boundary.lineEmpty {
				event()
			}
			boundary.lineEmpty = true
			boundary.previousCR = true
		case value == '\n':
			if boundary.lineEmpty {
				event()
			}
			boundary.lineEmpty = true
			boundary.previousCR = false
		default:
			boundary.lineEmpty = false
			boundary.previousCR = false
		}
	}
}

func (body *streamIdleTimeoutReadCloser) pump(writer *io.PipeWriter) {
	var finalErr error
	defer func() {
		_ = body.closeSource()
		body.state.finish()
		if finalErr != nil {
			_ = writer.CloseWithError(finalErr)
			return
		}
		_ = writer.Close()
	}()
	boundary := streamIdleBoundary{lineEmpty: true}
	buffer := make([]byte, 32*1024)
	for {
		count, readErr := body.source.Read(buffer)
		if count > 0 {
			active := true
			now := time.Now()
			boundary.observe(buffer[:count], func() {
				if active {
					active = body.state.recordEvent(now)
				}
			})
			if !active {
				finalErr = body.state.err()
				return
			}
			if !body.state.pause(time.Now()) {
				finalErr = body.state.err()
				return
			}
			_, writeErr := writer.Write(buffer[:count])
			body.state.resume(time.Now())
			if writeErr != nil {
				return
			}
		}
		if readErr != nil {
			finalErr = body.state.err()
			if finalErr == nil && readErr != io.EOF {
				finalErr = readErr
			}
			return
		}
	}
}

func (body *streamIdleTimeoutReadCloser) closeSource() error {
	body.sourceCloseOnce.Do(func() { body.sourceCloseErr = body.source.Close() })
	return body.sourceCloseErr
}

func (body *streamIdleTimeoutReadCloser) Read(data []byte) (int, error) {
	return body.reader.Read(data)
}

func (body *streamIdleTimeoutReadCloser) Close() error {
	body.closeOnce.Do(func() {
		body.state.finish()
		_ = body.reader.Close()
		body.closeErr = body.closeSource()
	})
	return body.closeErr
}
