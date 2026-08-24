package agent

import (
	"context"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/session"
)

type planExecutionTool struct {
	inner fantasy.AgentTool
}

func toolsForSessionMode(mode session.Mode, normalTools, planTools []fantasy.AgentTool) []fantasy.AgentTool {
	if mode.IsReadOnlyPlan() {
		return planTools
	}
	if mode == session.ModePlanExecution {
		return wrapToolsForPlanExecution(normalTools)
	}
	return normalTools
}

func wrapToolsForPlanExecution(toolset []fantasy.AgentTool) []fantasy.AgentTool {
	wrapped := make([]fantasy.AgentTool, len(toolset))
	for index, tool := range toolset {
		wrapped[index] = &planExecutionTool{inner: tool}
	}
	return wrapped
}

func (tool *planExecutionTool) Info() fantasy.ToolInfo {
	return tool.inner.Info()
}

func (tool *planExecutionTool) ProviderOptions() fantasy.ProviderOptions {
	return tool.inner.ProviderOptions()
}

func (tool *planExecutionTool) SetProviderOptions(options fantasy.ProviderOptions) {
	tool.inner.SetProviderOptions(options)
}

func (tool *planExecutionTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	return tool.inner.Run(permission.WithPlanExecutionApproval(ctx), call)
}
