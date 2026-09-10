package backend

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	mcptools "github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/google/uuid"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

func TestMCPWorkspaceAppTeardownIsolation(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	t.Setenv("CRUX_PROVIDER_PROFILE", "core-only")
	t.Setenv("HERDR_PANE_ID", "")
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	b := New(ctx, nil, func() {})
	b.SetCreateGrace(time.Minute)
	t.Cleanup(func() { drainBackend(t, b) })
	makeWorkspace := func(label string) *Workspace {
		server := sdk.NewServer(&sdk.Implementation{Name: label}, nil)
		sdk.AddTool(server, &sdk.Tool{Name: "identify", Description: "Workspace identity"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: label}}}, nil, nil
		})
		server.AddResource(&sdk.Resource{Name: label, URI: "fixture://" + label}, func(context.Context, *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
			return &sdk.ReadResourceResult{}, nil
		})
		host := httptest.NewServer(sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, nil))
		t.Cleanup(host.Close)
		cwd, data := t.TempDir(), t.TempDir()
		body, err := json.Marshal(map[string]any{"options": map[string]any{"disable_default_providers": true}, "providers": map[string]any{"fixture": map[string]any{"id": "fixture", "name": "Fixture", "type": "openai-compat", "base_url": "http://127.0.0.1:1/v1", "api_key": "synthetic", "models": []map[string]any{{"id": "model", "name": "Model", "context_window": 8192, "default_max_tokens": 128}}}}, "mcp": map[string]any{"same-name": map[string]any{"type": "http", "url": host.URL}}})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(cwd, "crux.json"), body, 0o600))
		ws, _, err := b.CreateWorkspace(protoWS(cwd, data, uuid.NewString()))
		require.NoError(t, err)
		t.Cleanup(ws.Shutdown)
		require.NoError(t, mcptools.For(ws.Cfg).WaitForInit(ctx))
		mcptools.For(ws.Cfg).RefreshResources(ctx, "same-name")
		return ws
	}
	first, second := makeWorkspace("first"), makeWorkspace("second")
	call := func(ws *Workspace, want string) {
		b.MCPRefreshResources(ctx, ws.ID, "same-name")
		result, err := mcptools.For(ws.Cfg).RunTool(ctx, ws.Cfg, "same-name", "identify", "{}")
		require.NoError(t, err)
		require.Equal(t, want, result.Content)
		states := b.MCPGetStates(ws.ID)
		require.Len(t, states, 1)
		require.Equal(t, mcptools.StateConnected, states["same-name"].State)
		resources, err := b.MCPResources(ctx, ws.ID)
		require.NoError(t, err)
		require.Equal(t, []proto.MCPResource{{MCPName: "same-name", URI: "fixture://" + want, Name: want}}, resources)
	}
	call(first, "first")
	call(second, "second")
	require.NoError(t, b.SetConfigField(first.ID, config.ScopeWorkspace, "mcp.same-name.enabled_tools", []string{"identify"}))
	require.Eventually(t, func() bool {
		state := b.MCPGetStates(first.ID)["same-name"]
		return state.State == mcptools.StateConnected && len(state.Config.EnabledTools) == 1
	}, 3*time.Second, 5*time.Millisecond)
	call(first, "first")
	call(second, "second")
	// Client-owned Apps have no MCP declarations in the current proposal schema.
	// Their shutdown must nevertheless never touch a host workspace's clients.
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: "fixture", Name: "Fixture", Type: catalog.TypeOpenAICompat, BaseURL: "http://127.0.0.1:1/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 128}}}}},
		Models:    map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "fixture", Model: "model"}, config.SelectedModelTypeSmall: {Provider: "fixture", Model: "model"}}, Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-fixture"}},
	}
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	clientArgs := protoWS(t.TempDir(), t.TempDir(), uuid.NewString())
	clientArgs.AuthorityMode = "client"
	clientArgs.AuthenticatedPrincipal = strings.Repeat("a", 64)
	clientArgs.Runtime = &proposal
	clientOwned, _, err := b.CreateWorkspace(clientArgs)
	require.NoError(t, err)
	t.Cleanup(clientOwned.Shutdown)
	require.NotNil(t, clientOwned.Cfg.RemoteAuthority())
	clientOwned.Shutdown()
	clientOwned.Shutdown()
	call(first, "first")
	call(second, "second")
	// Direct local App shutdown uses the same production cleanup as backend teardown.
	first.App.Shutdown()
	first.App.Shutdown()
	_, err = b.MCPResources(ctx, first.ID)
	require.ErrorIs(t, err, mcptools.ErrClosed)
	require.ErrorIs(t, mcptools.For(first.Cfg).InitializeSingle(ctx, "same-name", first.Cfg), mcptools.ErrClosed)
	call(second, "second")
	second.Shutdown()
	second.Shutdown()
	require.Empty(t, mcptools.For(second.Cfg).GetStates())
}
