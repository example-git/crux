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
)

const (
	PublicCompactionPath         = "responses/compact"
	DefaultMaxCompactionResponse = int64(16 << 20)
)

// CompactionClient calls the documented public Responses compaction operation.
// Authentication and consumer identity are supplied as ordinary host-owned
// headers; this type defines no provider-specific defaults.
type CompactionClient struct {
	BaseURL          string
	HTTPClient       *http.Client
	Headers          http.Header
	MaxResponseBytes int64
}

// Compact posts a public compaction request and returns the complete successful
// JSON result for protocol normalization by the caller.
func (c CompactionClient) Compact(ctx context.Context, request json.RawMessage) (json.RawMessage, error) {
	if !json.Valid(request) {
		return nil, fmt.Errorf("Responses compaction request is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(request, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Responses compaction request must be a JSON object")
	}
	endpoint, err := compactionURL(c.BaseURL)
	if err != nil {
		return nil, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(request))
	if err != nil {
		return nil, fmt.Errorf("create Responses compaction request: %w", err)
	}
	for key, values := range c.Headers {
		for _, value := range values {
			httpRequest.Header.Add(key, value)
		}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return nil, fmt.Errorf("execute Responses compaction request: %w", err)
	}
	defer response.Body.Close()
	limit := c.MaxResponseBytes
	if limit <= 0 {
		limit = DefaultMaxCompactionResponse
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read Responses compaction response: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("Responses compaction response exceeds %d bytes", limit)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("Responses compaction failed with HTTP %d", response.StatusCode)
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("Responses compaction response is not valid JSON")
	}
	return json.RawMessage(bytes.Clone(body)), nil
}

func compactionURL(base string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("Responses base URL must be absolute")
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + PublicCompactionPath
	return parsed.String(), nil
}
