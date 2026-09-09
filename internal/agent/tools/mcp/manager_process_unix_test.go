//go:build !windows

package mcp

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
)

// This test binary is the real stdio MCP child; it writes only protocol bytes
// to stdout. No installed user executable or remote service is invoked.
func TestWorkspaceMCPStdioChild(t *testing.T) {
	if os.Getenv("CRUX_MCP_TEST_CHILD") != "1" {
		return
	}
	s := sdk.NewServer(&sdk.Implementation{Name: "fixture"}, nil)
	sdk.AddTool(s, &sdk.Tool{Name: "identify", Description: "Identify child"}, func(context.Context, *sdk.CallToolRequest, struct{}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf("%s:%d", os.Getenv("CRUX_MCP_TEST_OWNER"), os.Getpid())}}}, nil, nil
	})
	if err := s.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func TestWorkspaceMCPStdioShutdownReapsOnlyOwnedProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	executable, err := os.Executable()
	require.NoError(t, err)
	stores := map[string]*config.ConfigStore{}
	pids := map[string]int{}
	for _, owner := range []string{"first", "second"} {
		store := config.NewTestStore(&config.Config{MCP: config.MCPs{"same-name": {Type: config.MCPStdio, Command: executable, Args: []string{"-test.run=^TestWorkspaceMCPStdioChild$"}, Env: map[string]string{"CRUX_MCP_TEST_CHILD": "1", "CRUX_MCP_TEST_OWNER": owner}}}})
		stores[owner] = store
		t.Cleanup(func() { _ = For(store).Close(context.Background()) })
		require.NoError(t, For(store).InitializeSingle(ctx, "same-name", store))
		result, err := For(store).RunTool(ctx, store, "same-name", "identify", "{}")
		require.NoError(t, err)
		parts := strings.Split(result.Content, ":")
		require.Equal(t, owner, parts[0])
		require.Len(t, parts, 2)
		pids[owner], err = strconv.Atoi(parts[1])
		require.NoError(t, err)
	}
	require.NotEqual(t, pids["first"], pids["second"])
	require.NoError(t, For(stores["first"]).Close(ctx))
	require.Eventually(t, func() bool { return syscall.Kill(pids["first"], 0) == syscall.ESRCH }, time.Second, 5*time.Millisecond)
	require.NoError(t, syscall.Kill(pids["second"], 0))
	result, err := For(stores["second"]).RunTool(ctx, stores["second"], "same-name", "identify", "{}")
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("second:%d", pids["second"]), result.Content)
	require.NoError(t, For(stores["first"]).Close(ctx))
}
