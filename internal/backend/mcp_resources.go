package backend

import (
	"cmp"
	"context"
	"slices"

	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/proto"
)

func (b *Backend) MCPResources(ctx context.Context, workspaceID string) ([]proto.MCPResource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	cached, err := mcp.For(ws.Cfg).ResourceSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	result := []proto.MCPResource{}
	for name, resources := range cached {
		for _, resource := range resources {
			result = append(result, proto.MCPResource{MCPName: name, URI: resource.URI, Name: resource.Name, MIMEType: resource.MIMEType})
		}
	}
	slices.SortFunc(result, func(a, b proto.MCPResource) int {
		if n := cmp.Compare(a.MCPName, b.MCPName); n != 0 {
			return n
		}
		return cmp.Compare(a.URI, b.URI)
	})
	return result, nil
}
