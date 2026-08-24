package gemini

import (
	"context"
	"encoding/json"
	"io"

	providersse "github.com/example-git/crux/internal/providertransport/sse"
)

const DefaultMaxEventBytes = providersse.DefaultMaxEventBytes

var ErrMissingTerminal = providersse.ErrMissingTerminal

type SSEOptions = providersse.Options

// ParseSSE applies provider-neutral bounded SSE framing to public Gemini JSON
// events. Private malformed-stream recovery remains outside this package.
func ParseSSE(ctx context.Context, reader io.Reader, options SSEOptions, yield func(json.RawMessage) error) error {
	return providersse.Parse(ctx, reader, options, yield)
}
