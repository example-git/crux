package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	providersse "github.com/example-git/crux/internal/providertransport/sse"
)

const (
	PublicResponsesPath    = "responses"
	DefaultMaxResponseBody = int64(16 << 20)
)

// Client is a raw transport for the documented public Responses HTTP and SSE
// operations. Authentication, endpoint identity, retries, and consumer policy
// are supplied by the host and are never inferred here.
type Client struct {
	BaseURL          string
	HTTPClient       *http.Client
	Headers          http.Header
	MaxResponseBytes int64
	SSE              providersse.Options
}

func (c Client) Create(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	response, err := c.do(ctx, request, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limit := c.responseLimit()
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Responses response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("Responses response exceeds %d bytes", limit)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("Responses response is not valid JSON")
	}
	return json.RawMessage(bytes.Clone(body)), nil
}

func (c Client) Stream(ctx context.Context, request json.RawMessage, yield func(Event) error) error {
	if yield == nil {
		return fmt.Errorf("Responses stream yield callback is nil")
	}
	response, err := c.do(ctx, request, "text/event-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	options := c.SSE
	if options.IsTerminal == nil {
		options.IsTerminal = func(raw json.RawMessage) bool {
			event, decodeErr := DecodeEvent(raw)
			return decodeErr == nil && terminalEvent(event.Type)
		}
	}
	return providersse.Parse(ctx, response.Body, options, func(raw json.RawMessage) error {
		event, err := DecodeEvent(raw)
		if err != nil {
			return err
		}
		if event.StreamID != "" {
			return fmt.Errorf("Responses HTTP event %q unexpectedly contains stream_id", event.Type)
		}
		return yield(event)
	})
}

func (c Client) do(ctx context.Context, body json.RawMessage, accept string) (*http.Response, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("Responses request is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	endpoint, err := responsesURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Responses request: %w", err)
	}
	for key, values := range c.Headers {
		for _, value := range values {
			request.Header.Add(key, value)
		}
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", accept)
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute Responses request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, c.responseLimit()))
		return nil, fmt.Errorf("Responses request failed with HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (c Client) responseLimit() int64 {
	if c.MaxResponseBytes > 0 {
		return c.MaxResponseBytes
	}
	return DefaultMaxResponseBody
}

func responsesURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("Responses base URL must be absolute")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + PublicResponsesPath
	return parsed.String(), nil
}
