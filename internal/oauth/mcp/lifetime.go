package mcpoauth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
)

// NewSessionHTTPClient owns its HTTP connection pool and binds even SDK-created
// detached requests to the MCP session lifetime. Canceling it never closes a
// different workspace's pool.
func NewSessionHTTPClient(ctx context.Context) *http.Client {
	base := http.DefaultTransport
	if transport, ok := base.(*http.Transport); ok {
		owned := transport.Clone()
		context.AfterFunc(ctx, owned.CloseIdleConnections)
		base = owned
	}
	return &http.Client{Transport: &lifetimeTransport{ctx: ctx, base: base}}
}

type lifetimeTransport struct {
	ctx  context.Context
	base http.RoundTripper
}

func (t *lifetimeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := t.ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(req.Context())
	stop := context.AfterFunc(t.ctx, cancel)
	done := func() { stop(); cancel() }
	if t.ctx.Err() != nil {
		done()
		return nil, t.ctx.Err()
	}
	response, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		done()
		return nil, err
	}
	if response == nil {
		done()
		return nil, errors.New("MCP HTTP transport returned no response")
	}
	if response.Body == nil {
		// Preserve net/http's contract for custom transports with an empty
		// response, while rejecting an impossible declared nonempty body.
		if response.ContentLength > 0 && req.Method != http.MethodHead {
			done()
			return nil, errors.New("MCP HTTP transport returned no response body")
		}
		response.Body = http.NoBody
	}
	response.Body = &lifetimeBody{ReadCloser: response.Body, done: done}
	return response, nil
}

type lifetimeBody struct {
	io.ReadCloser
	done func()
	once sync.Once
}

func (b *lifetimeBody) Close() error { defer b.once.Do(b.done); return b.ReadCloser.Close() }
