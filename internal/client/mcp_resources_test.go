package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPResourcesSDKThroughScopedRegisteredRoute(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "core-only")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	srv.Backend().SetCreateGrace(time.Minute)
	t.Cleanup(srv.Backend().Shutdown)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	c := captureClient(t, hs)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	create := func(label string) *proto.Workspace {
		mcp := sdk.NewServer(&sdk.Implementation{Name: label}, nil)
		mcp.AddResource(&sdk.Resource{Name: label, URI: "fixture://" + label, MIMEType: "text/plain"}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{}, nil
		})
		sdk.AddTool(mcp, &sdk.Tool{Name: "tool", Description: "Fixture"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{}, nil, nil
		})
		target := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return mcp }, nil))
		t.Cleanup(target.Close)
		cwd := t.TempDir()
		data, err := json.Marshal(map[string]any{"options": map[string]any{"disable_default_providers": true}, "providers": map[string]any{"fixture": map[string]any{"id": "fixture", "type": "openai-compat", "base_url": "http://127.0.0.1:1/v1", "api_key": "synthetic", "models": []map[string]any{{"id": "model", "name": "Model", "context_window": 8192, "default_max_tokens": 128}}}}, "mcp": map[string]any{"same-name": map[string]any{"type": "http", "url": target.URL}}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cwd, "crux.json"), data, 0o600))
		ws, err := c.CreateWorkspace(ctx, proto.Workspace{Path: cwd, DataDir: t.TempDir(), AuthorityMode: "server"})
		require.NoError(t, err)
		backend, err := srv.Backend().GetWorkspace(ws.ID)
		require.NoError(t, err)
		t.Cleanup(backend.Shutdown)
		require.NoError(t, mcptools.For(backend.Cfg).WaitForInit(ctx))
		require.NoError(t, c.MCPRefreshResources(ctx, ws.ID, "same-name"))
		return ws
	}
	first, second := create("first"), create("second")
	read := func(ws *proto.Workspace, label string) {
		resources, err := c.MCPResources(ctx, ws.ID)
		require.NoError(t, err)
		require.Equal(t, []proto.MCPResource{{MCPName: "same-name", URI: "fixture://" + label, Name: label, MIMEType: "text/plain"}}, resources)
	}
	read(first, "first")
	read(second, "second")
	_, err := c.MCPResources(ctx, "missing")
	require.Error(t, err)
	require.NoError(t, c.DeleteWorkspace(ctx, first.ID))
	read(second, "second")
}
