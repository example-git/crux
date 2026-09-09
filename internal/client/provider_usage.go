package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/example-git/crux/internal/config"
)

func (c *Client) ProviderUsage(ctx context.Context, id string, request config.ProviderUsageRequest) (*config.ProviderUsageResult, error) {
	rsp, err := c.post(ctx, fmt.Sprintf("/workspaces/%s/providers/usage", id), nil, jsonBody(request), http.Header{"Content-Type": []string{"application/json"}})
	if err != nil {
		return nil, fmt.Errorf("fetch provider usage: %w", err)
	}
	defer rsp.Body.Close()
	if rsp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch provider usage: status code %d", rsp.StatusCode)
	}
	var result config.ProviderUsageResult
	if err := json.NewDecoder(io.LimitReader(rsp.Body, 1<<20)).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode provider usage: %w", err)
	}
	if result.Revision != request.Revision || result.Digest != request.Digest {
		return nil, config.ErrProviderUsageAuthority
	}
	return &result, nil
}
