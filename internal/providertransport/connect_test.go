package providertransport

import (
	"context"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type connectRoundTripFunc func(*http.Request) (*http.Response, error)

func (function connectRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func TestTransportWithConnectTimeoutStopsBlockedConnection(t *testing.T) {
	called := false
	transport := TransportWithConnectTimeout(connectRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		<-request.Context().Done()
		return nil, context.Cause(request.Context())
	}), 20*time.Millisecond)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	if response != nil && response.Body != nil {
		defer response.Body.Close()
	}
	require.Nil(t, response)
	require.ErrorContains(t, err, "connect timeout after 20ms")
	require.True(t, called)
}

func TestTransportWithConnectTimeoutStopsAfterConnection(t *testing.T) {
	var requestContext context.Context
	transport := TransportWithConnectTimeout(connectRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestContext = request.Context()
		trace := httptrace.ContextClientTrace(request.Context())
		require.NotNil(t, trace)
		require.NotNil(t, trace.GotConn)
		trace.GotConn(httptrace.GotConnInfo{})
		select {
		case <-request.Context().Done():
			return nil, context.Cause(request.Context())
		case <-time.After(40 * time.Millisecond):
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     make(http.Header),
		}, nil
	}), 20*time.Millisecond)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	require.NotNil(t, response)
	select {
	case <-requestContext.Done():
		require.Fail(t, "request context canceled before response body close", context.Cause(requestContext))
	default:
	}
	require.NoError(t, response.Body.Close())
	select {
	case <-requestContext.Done():
	case <-time.After(time.Second):
		require.Fail(t, "request context remained active after response body close")
	}
}

func TestTransportWithStreamIdleTimeoutStopsBeforeFirstEvent(t *testing.T) {
	source, writer := io.Pipe()
	defer writer.Close()
	transport := TransportWithStreamIdleTimeout(connectRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       source,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}, nil
	}), 20*time.Millisecond)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.ErrorContains(t, err, "stream idle timeout after 20ms")
	require.NoError(t, response.Body.Close())
}

func TestTransportWithStreamIdleTimeoutResetsAfterCompleteEvents(t *testing.T) {
	source, writer := io.Pipe()
	stream := "data: one\r\n\r\ndata: two\n\ndata: three\r\rdata: four\n\n"
	transport := TransportWithStreamIdleTimeout(connectRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       source,
			Header:     http.Header{"Content-Type": []string{"text/event-stream; charset=utf-8"}},
		}, nil
	}), 400*time.Millisecond)
	go func() {
		for _, event := range []string{"data: one\r\n\r\n", "data: two\n\n", "data: three\r\r", "data: four\n\n"} {
			if _, err := io.WriteString(writer, event); err != nil {
				return
			}
			time.Sleep(150 * time.Millisecond)
		}
		_ = writer.Close()
	}()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, stream, string(body))
	require.NoError(t, response.Body.Close())
}

func TestTransportWithStreamIdleTimeoutDoesNotResetForPartialEventBytes(t *testing.T) {
	source, writer := io.Pipe()
	transport := TransportWithStreamIdleTimeout(connectRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       source,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		}, nil
	}), 50*time.Millisecond)
	go func() {
		defer writer.Close()
		for _, fragment := range []string{"d", "a", "t", "a", ":", " ", "{"} {
			if _, err := io.WriteString(writer, fragment); err != nil {
				return
			}
			time.Sleep(15 * time.Millisecond)
		}
	}()

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	_, err = io.ReadAll(response.Body)
	require.ErrorContains(t, err, "stream idle timeout after 50ms")
	require.NoError(t, response.Body.Close())
}

func TestTransportWithStreamIdleTimeoutIgnoresDownstreamBackpressure(t *testing.T) {
	stream := ": heartbeat\r\n\r\ndata: one\n\ndata: two\r\n\r\n"
	var requestContext context.Context
	transport := TransportWithStreamIdleTimeout(connectRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestContext = request.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(stream)),
			Header:     http.Header{"Content-Type": []string{"Text/Event-Stream"}},
		}, nil
	}), 250*time.Millisecond)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	time.Sleep(750 * time.Millisecond)
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, stream, string(body))
	select {
	case <-requestContext.Done():
	case <-time.After(time.Second):
		require.Fail(t, "request context remained active after stream EOF")
	}
	require.NoError(t, response.Body.Close())
}

func TestTransportWithStreamIdleTimeoutLeavesNonStreamResponsesUnchanged(t *testing.T) {
	var requestContext context.Context
	transport := TransportWithStreamIdleTimeout(connectRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestContext = request.Context()
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("ok")),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
		}, nil
	}), 20*time.Millisecond)

	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://example.invalid", nil)
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	time.Sleep(60 * time.Millisecond)
	select {
	case <-requestContext.Done():
		require.Fail(t, "non-stream request context canceled by idle timeout", context.Cause(requestContext))
	default:
	}
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, "ok", string(body))
	require.NoError(t, response.Body.Close())
	select {
	case <-requestContext.Done():
	case <-time.After(time.Second):
		require.Fail(t, "non-stream request context remained active after body close")
	}
}
