package mcp

import (
	"context"
	"errors"
	"iter"
	"log/slog"

	"github.com/example-git/crux/internal/config"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Resource = mcp.Resource

type ResourceContents = mcp.ResourceContents

// Resources returns all available MCP resources.
func (runtime *Manager) Resources() iter.Seq2[string, []*Resource] {
	if runtime.ctx.Err() != nil {
		return func(func(string, []*Resource) bool) {}
	}
	return runtime.allResources.Seq2()
}

// ResourceSnapshot reads only this workspace's cached metadata. A closed
// runtime is an error, distinct from a live workspace with no cached resources.
func (runtime *Manager) ResourceSnapshot(ctx context.Context) (map[string][]*Resource, error) {
	ctx, done, err := runtime.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	resources := runtime.allResources.Copy()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return resources, nil
}

// ListResources returns the current resources for an MCP server.
func (runtime *Manager) ListResources(ctx context.Context, cfg *config.ConfigStore, name string) ([]*Resource, error) {
	if err := runtime.requireStore(cfg); err != nil {
		return nil, err
	}
	ctx, done, admissionErr := runtime.admit(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
	session, err := runtime.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		return nil, err
	}

	_, release, err := runtime.serverOperation(ctx, name)
	if err != nil {
		return nil, err
	}
	defer release()
	if current, ok := runtime.sessions.Get(name); !ok || current != session {
		return nil, errors.New("MCP server changed while listing resources")
	}
	resourceCount := runtime.updateResources(name, resources)
	prev, _ := runtime.states.Get(name)
	prev.Counts.Resources = resourceCount
	runtime.updateState(name, StateConnected, nil, session, prev.Counts)
	return resources, nil
}

// ReadResource reads the contents of a resource from an MCP server.
func (runtime *Manager) ReadResource(ctx context.Context, cfg *config.ConfigStore, name, uri string) ([]*ResourceContents, error) {
	if err := runtime.requireStore(cfg); err != nil {
		return nil, err
	}
	ctx, done, admissionErr := runtime.admit(ctx)
	if admissionErr != nil {
		return nil, admissionErr
	}
	defer done()
	session, err := runtime.getOrRenewClient(ctx, cfg, name)
	if err != nil {
		return nil, err
	}
	result, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: uri})
	if err != nil {
		return nil, err
	}
	return result.Contents, nil
}

// RefreshResources gets the updated list of resources from the MCP and updates the
// workspace state.
func (runtime *Manager) RefreshResources(ctx context.Context, name string) {
	ctx, done, admissionErr := runtime.serverOperation(ctx, name)
	if admissionErr != nil {
		return
	}
	defer done()
	session, ok := runtime.sessions.Get(name)
	if !ok {
		slog.Warn("Refresh resources: no session", "name", name)
		return
	}

	resources, err := getResources(ctx, session)
	if err != nil {
		runtime.updateState(name, StateError, err, nil, Counts{})
		return
	}

	resourceCount := runtime.updateResources(name, resources)

	prev, _ := runtime.states.Get(name)
	prev.Counts.Resources = resourceCount
	runtime.updateState(name, StateConnected, nil, session, prev.Counts)
}

func getResources(ctx context.Context, c *ClientSession) ([]*Resource, error) {
	if c.InitializeResult().Capabilities.Resources == nil {
		return nil, nil
	}
	result, err := c.ListResources(ctx, &mcp.ListResourcesParams{})
	if err != nil {
		// Handle "Method not found" errors from MCP servers that don't support resources/list.
		if isMethodNotFoundError(err) {
			slog.Warn("MCP server does not support resources/list", "error", err)
			return nil, nil
		}
		return nil, err
	}
	return result.Resources, nil
}

// isMethodNotFoundError checks if the error is a JSON-RPC "Method not found" error.
func isMethodNotFoundError(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr != nil && rpcErr.Code == jsonrpc.CodeMethodNotFound
}

func (runtime *Manager) updateResources(name string, resources []*Resource) int {
	if len(resources) == 0 {
		runtime.allResources.Del(name)
		return 0
	}
	runtime.allResources.Set(name, resources)
	return len(resources)
}
