package client

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/providerregistry"
)

// ProviderSurfaces returns the remote execution host's redacted registry
// presentation metadata for one workspace.
func (c *Client) ProviderSurfaces(ctx context.Context, workspaceID string) ([]providerregistry.Surface, error) {
	rsp, err := c.get(ctx, fmt.Sprintf("/workspaces/%s/providers", workspaceID), nil, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get workspace provider surfaces: %w", err)
	}
	defer rsp.Body.Close()
	if err := checkStatus(rsp); err != nil {
		return nil, fmt.Errorf("failed to get workspace provider surfaces: %w", err)
	}
	var surfaces []providerregistry.Surface
	if err := json.NewDecoder(rsp.Body).Decode(&surfaces); err != nil {
		return nil, fmt.Errorf("failed to decode workspace provider surfaces: %w", err)
	}
	for i := range surfaces {
		surfaces[i] = surfaces[i].Clone()
	}
	return surfaces, nil
}
