package agent

import (
	"context"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type permissionRequestTool struct {
	permissions permission.Service
}

func (tool *permissionRequestTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "permission_test"}
}

func (tool *permissionRequestTool) ProviderOptions() fantasy.ProviderOptions {
	return nil
}

func (tool *permissionRequestTool) SetProviderOptions(fantasy.ProviderOptions) {}

func (tool *permissionRequestTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	granted, err := tool.permissions.Request(ctx, permission.CreatePermissionRequest{
		SessionID:  tools.GetSessionFromContext(ctx),
		ToolCallID: call.ID,
		ToolName:   call.Name,
		Action:     "execute",
		Path:       "/tmp",
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if !granted {
		return fantasy.NewTextErrorResponse("permission denied"), nil
	}
	return fantasy.NewTextResponse("permission granted"), nil
}

func TestToolsForSessionMode(t *testing.T) {
	normalTool := &fakeTool{name: "normal"}
	planTool := &fakeTool{name: "plan"}
	normalTools := []fantasy.AgentTool{normalTool}
	planTools := []fantasy.AgentTool{planTool}

	for _, mode := range []session.Mode{session.ModePlan, session.ModePlanRevision} {
		selected := toolsForSessionMode(mode, normalTools, planTools)
		require.Len(t, selected, 1)
		require.Same(t, planTool, selected[0])
	}

	selected := toolsForSessionMode(session.ModeDefault, normalTools, planTools)
	require.Len(t, selected, 1)
	require.Same(t, normalTool, selected[0])

	selected = toolsForSessionMode(session.ModePlanExecution, normalTools, planTools)
	require.Len(t, selected, 1)
	wrapped, ok := selected[0].(*planExecutionTool)
	require.True(t, ok)
	require.Same(t, normalTool, wrapped.inner)
}

func TestPlanExecutionToolAutoApprovesWithoutEnablingYolo(t *testing.T) {
	permissions := permission.NewPermissionService(t.TempDir(), false, nil)
	wrapped := wrapToolsForPlanExecution([]fantasy.AgentTool{&permissionRequestTool{permissions: permissions}})

	response, err := wrapped[0].Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "permission_test"})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Equal(t, "permission granted", response.Content)
	require.False(t, permissions.SkipRequests())
}

func TestPlanExecutionToolDoesNotBypassHookDenial(t *testing.T) {
	inner := &fakeTool{name: "bash"}
	hooked := newHookedTool(inner, newRunner(t, `echo "blocked" >&2; exit 2`))
	wrapped := wrapToolsForPlanExecution([]fantasy.AgentTool{hooked})

	response, err := wrapped[0].Run(t.Context(), fantasy.ToolCall{ID: "call-1", Name: "bash"})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "blocked")
	require.False(t, inner.called)
}
