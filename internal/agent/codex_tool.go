package agent

import (
	"context"
	"fmt"
	"runtime/debug"

	fantasy "github.com/example-git/crux/foundation"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
)

type codexBoundedTool struct {
	tool    fantasy.AgentTool
	modelID string
}

func (t *codexBoundedTool) Info() fantasy.ToolInfo {
	return t.tool.Info()
}

func (t *codexBoundedTool) Run(ctx context.Context, call fantasy.ToolCall) (response fantasy.ToolResponse, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			response = fantasy.NewTextErrorResponse(codexresponses.TruncateToolOutput(t.modelID, fmt.Sprintf(
				"tool %q panicked: %v\n\n%s", call.Name, recovered, debug.Stack(),
			)))
			err = nil
		}
	}()
	response, err = t.tool.Run(ctx, call)
	response.Content = codexresponses.TruncateToolOutput(t.modelID, response.Content)
	if err != nil {
		err = boundedToolError{err: err, modelID: t.modelID}
	}
	return response, err
}

func (t *codexBoundedTool) ProviderOptions() fantasy.ProviderOptions {
	return t.tool.ProviderOptions()
}

func (t *codexBoundedTool) SetProviderOptions(options fantasy.ProviderOptions) {
	t.tool.SetProviderOptions(options)
}

type boundedToolError struct {
	err     error
	modelID string
}

func (e boundedToolError) Error() string {
	return codexresponses.TruncateToolOutput(e.modelID, e.err.Error())
}

func (e boundedToolError) Unwrap() error {
	return e.err
}

func codexBoundedTools(tools []fantasy.AgentTool, model Model) []fantasy.AgentTool {
	if model.InstructionPolicy != fantasy.InstructionPolicyCodex {
		return tools
	}
	bounded := make([]fantasy.AgentTool, len(tools))
	for i, tool := range tools {
		if existing, ok := tool.(*codexBoundedTool); ok {
			existing.modelID = model.ModelCfg.Model
			bounded[i] = existing
			continue
		}
		bounded[i] = &codexBoundedTool{tool: tool, modelID: model.ModelCfg.Model}
	}
	return bounded
}
