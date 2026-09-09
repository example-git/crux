package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/example-git/crux/internal/proto"
)

func (c *Client) MCPResources(ctx context.Context, id string) ([]proto.MCPResource, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/mcp/resources", id), nil, nil)
	if err != nil {
		return nil, err
	}
	defer rsp.Body.Close()
	if rsp.StatusCode < 200 || rsp.StatusCode >= 300 {
		bounded := *rsp
		bounded.Body = io.NopCloser(io.LimitReader(rsp.Body, 64<<10))
		return nil, checkStatus(&bounded)
	}
	data, err := io.ReadAll(io.LimitReader(rsp.Body, 1024*1024+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1024*1024 {
		return nil, fmt.Errorf("MCP resource response exceeds limit")
	}
	var result []proto.MCPResource
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}
