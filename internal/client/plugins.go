package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/proto"
)

// PluginSnapshot returns the authoritative redacted provider-plugin status for
// the remote execution host.
func (c *Client) PluginSnapshot(ctx context.Context) (proto.PluginSnapshot, error) {
	rsp, err := c.get(ctx, "/plugins", nil, nil)
	if err != nil {
		return proto.PluginSnapshot{}, fmt.Errorf("failed to get provider plugin status: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return proto.PluginSnapshot{}, fmt.Errorf("failed to get provider plugin status: %w", err)
	}
	var snapshot proto.PluginSnapshot
	if err := json.NewDecoder(rsp.Body).Decode(&snapshot); err != nil {
		return proto.PluginSnapshot{}, fmt.Errorf("failed to decode provider plugin status: %w", err)
	}
	return snapshot, nil
}
