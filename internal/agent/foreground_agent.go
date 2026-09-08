package agent

import (
	"context"
	"errors"
	"fmt"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	managedtask "github.com/example-git/crux/internal/task"
)

func (c *coordinator) runDetachableSubAgent(ctx context.Context, params AgentParams, selected presetSubagent, runParams subAgentParams) (fantasy.ToolResponse, error) {
	if c.backgroundShells == nil || c.backgroundAgents == nil {
		return c.runSubAgent(ctx, runParams)
	}
	runCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	runCtx, markDetached := permission.WithDetachableAgent(runCtx)
	promoted := false
	defer func() {
		if !promoted {
			cancel()
		}
	}()
	type agentResult struct {
		response fantasy.ToolResponse
		err      error
	}
	result := make(chan agentResult, 1)
	go func() {
		response, err := c.runSubAgent(runCtx, runParams)
		result <- agentResult{response, err}
	}()
	foregroundWait := c.backgroundShells.ForegroundWaits.Register(runParams.SessionID)
	defer c.backgroundShells.ForegroundWaits.Remove(foregroundWait)
	select {
	case output := <-result:
		return output.response, output.err
	case <-ctx.Done():
		if !c.backgroundShells.ForegroundWaits.Remove(foregroundWait) {
			cancel()
			<-result
			return fantasy.ToolResponse{}, ctx.Err()
		}
	case <-foregroundWait.Detached:
	}
	agentType := params.SubagentType
	if agentType == "" {
		agentType = config.AgentTask
	}
	task, err := c.backgroundAgents.ReserveContext(runCtx, params.Prompt, agentType, selected.title, managedtask.Ownership{
		ParentSessionID:  runParams.SessionID,
		OriginToolCallID: runParams.ToolCallID,
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Could not send agent to background: %v", err)), nil
	}
	childSessionID := c.sessions.CreateAgentToolSessionID(runParams.AgentMessageID, runParams.ToolCallID)
	markDetached()
	err = c.backgroundAgents.Start(task, childSessionID, func(taskCtx context.Context) backgroundAgentResult {
		defer cancel()
		var output agentResult
		select {
		case output = <-result:
		case <-taskCtx.Done():
			cancel()
			output = <-result
		}
		finished := backgroundAgentResult{Output: output.response.Content, Err: output.err, Usage: c.backgroundAgentUsage(childSessionID)}
		if output.response.IsError && finished.Err == nil {
			finished.Err = errors.New(output.response.Content)
		}
		return finished
	})
	if err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("Could not send agent to background: %v", err)), nil
	}
	promoted = true
	metadata := AgentResponseMetadata{Background: true, TaskID: task.ID, ChildSessionID: childSessionID}
	return fantasy.WithResponseMetadata(fantasy.NewTextResponse(fmt.Sprintf("The user sent this agent to the background.\n\nBackground task ID: %s\n\nUse task_output to read output or task_stop to terminate.", task.ID)), metadata), nil
}
