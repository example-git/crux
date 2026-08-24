package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	DefaultMaxResponseBytes = int64(16 << 20)
	InteractionsPath        = "/v1beta/interactions"
)

// Client is a raw public Gemini HTTP/SSE transport. Credentials and identity
// are supplied through Headers; SDK/private retry behavior is not enabled.
type Client struct {
	BaseURL          string
	HTTPClient       *http.Client
	Headers          http.Header
	MaxResponseBytes int64
	SSE              SSEOptions
}

func (c Client) GenerateContent(ctx context.Context, model string, request json.RawMessage) (json.RawMessage, error) {
	return c.doJSON(ctx, generateContentPath(model, false), request)
}

func (c Client) StreamGenerateContent(ctx context.Context, model string, request json.RawMessage, yield func(json.RawMessage) error) error {
	return c.doSSE(ctx, generateContentPath(model, true), request, yield)
}

func (c Client) CreateInteraction(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	return c.doJSON(ctx, InteractionsPath, request)
}

func (c Client) StreamInteraction(ctx context.Context, request json.RawMessage, yield func(json.RawMessage) error) error {
	return c.doSSE(ctx, InteractionsPath, request, yield)
}

func generateContentPath(model string, stream bool) string {
	method := "generateContent"
	query := ""
	if stream {
		method = "streamGenerateContent"
		query = "?alt=sse"
	}
	return "/v1beta/models/" + url.PathEscape(model) + ":" + method + query
}

func (c Client) doJSON(ctx context.Context, path string, request json.RawMessage) (json.RawMessage, error) {
	response, err := c.do(ctx, path, request, "application/json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	limit := c.MaxResponseBytes
	if limit <= 0 {
		limit = DefaultMaxResponseBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Gemini response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("Gemini response exceeds %d bytes", limit)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("Gemini response is not valid JSON")
	}
	return json.RawMessage(bytes.Clone(body)), nil
}

func (c Client) doSSE(ctx context.Context, path string, request json.RawMessage, yield func(json.RawMessage) error) error {
	response, err := c.do(ctx, path, request, "text/event-stream")
	if err != nil {
		return err
	}
	defer response.Body.Close()
	return ParseSSE(ctx, response.Body, c.SSE, yield)
}

func (c Client) do(ctx context.Context, path string, body json.RawMessage, accept string) (*http.Response, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("Gemini request is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Gemini request must be a JSON object")
	}
	endpoint, err := c.resolve(path)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Gemini request: %w", err)
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
		return nil, fmt.Errorf("execute Gemini request: %w", err)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		limit := c.MaxResponseBytes
		if limit <= 0 {
			limit = DefaultMaxResponseBytes
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, limit))
		return nil, fmt.Errorf("Gemini request failed with HTTP %d", response.StatusCode)
	}
	return response, nil
}

func (c Client) resolve(path string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(c.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("Gemini base URL must be absolute")
	}
	reference, err := url.Parse(path)
	if err != nil {
		return "", fmt.Errorf("invalid Gemini operation path: %w", err)
	}
	return base.ResolveReference(reference).String(), nil
}
