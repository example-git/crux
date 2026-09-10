package workspace

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/proto"
)

func (w *AppWorkspace) MCPResources(ctx context.Context) ([]proto.MCPResource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cached, err := mcp.For(w.store).ResourceSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	result := []proto.MCPResource{}
	for name, resources := range cached {
		for _, r := range resources {
			result = append(result, proto.MCPResource{MCPName: name, URI: r.URI, Name: r.Name, MIMEType: r.MIMEType})
		}
	}
	return result, nil
}

func (w *ClientWorkspace) MCPResources(ctx context.Context) ([]proto.MCPResource, error) {
	if err := w.subCtx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(w.subCtx, cancel)
	defer func() { stop(); cancel() }()
	id := w.workspaceID()
	result, err := w.client.MCPResources(ctx, id)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if w.workspaceID() != id {
		return nil, fmt.Errorf("workspace changed while reading MCP resources")
	}
	return result, nil
}
