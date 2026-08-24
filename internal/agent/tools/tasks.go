package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	managedtask "github.com/example-git/crux/internal/task"
)

const (
	TaskListToolName     = "task_list"
	TaskOutputToolName   = "task_output"
	TaskStopToolName     = "task_stop"
	TaskContinueToolName = "task_continue"
)

const taskListDescription = "List managed background shell and agent tasks with their status, ownership, output location, and usage."

const taskOutputDescription = "Read output from a managed background shell or agent task. Set wait to block for at most timeout_ms while the task is still active."

const taskStopDescription = "Stop a managed background shell or agent task. Stopping an already terminal task is idempotent."

const taskContinueDescription = "Continue the persisted transcript of a terminal background agent task with a new prompt. The continued turn starts as a new background task."

type TaskService interface {
	ListTasks() []managedtask.View
	TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error)
	StopTask(ctx context.Context, id string) (managedtask.View, error)
	ContinueTask(ctx context.Context, id, parentSessionID, prompt, originToolCallID string) (managedtask.View, error)
}

type TaskOutputParams struct {
	TaskID    string `json:"task_id" description:"The typed ID of the background task"`
	Wait      *bool  `json:"wait,omitempty" description:"Wait for output or task completion. Defaults to true."`
	TimeoutMS *int   `json:"timeout_ms,omitempty" description:"Maximum milliseconds to wait. Defaults to 30000 and must be between 0 and 600000."`
}

type TaskStopParams struct {
	TaskID string `json:"task_id" description:"The typed ID of the background task to stop"`
}

type TaskContinueParams struct {
	TaskID string `json:"task_id" description:"The typed ID of the terminal background agent task"`
	Prompt string `json:"prompt" description:"The next prompt for the persisted agent transcript"`
}

func NewTaskListTool(service TaskService) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskListToolName,
		taskListDescription,
		func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			data, err := json.MarshalIndent(service.ListTasks(), "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding managed tasks: %w", err)
			}
			if strings.TrimSpace(string(data)) == "[]" || strings.TrimSpace(string(data)) == "null" {
				return fantasy.NewTextResponse("No background tasks are currently tracked."), nil
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

func NewTaskOutputTool(service TaskService) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskOutputToolName,
		taskOutputDescription,
		func(ctx context.Context, params TaskOutputParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			wait := true
			if params.Wait != nil {
				wait = *params.Wait
			}
			timeoutMS := 30000
			if params.TimeoutMS != nil {
				timeoutMS = *params.TimeoutMS
			}
			if timeoutMS < 0 || timeoutMS > 600000 {
				return fantasy.NewTextErrorResponse("timeout_ms must be between 0 and 600000"), nil
			}
			result, err := service.TaskOutput(ctx, params.TaskID, wait, time.Duration(timeoutMS)*time.Millisecond)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding managed task output: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

func NewTaskContinueTool(service TaskService) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskContinueToolName,
		taskContinueDescription,
		func(ctx context.Context, params TaskContinueParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			if params.Prompt == "" {
				return fantasy.NewTextErrorResponse("prompt is required"), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, fmt.Errorf("session id missing from context")
			}
			result, err := service.ContinueTask(ctx, params.TaskID, sessionID, params.Prompt, call.ID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding continued task: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}

func NewTaskStopTool(service TaskService) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		TaskStopToolName,
		taskStopDescription,
		func(ctx context.Context, params TaskStopParams, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			if params.TaskID == "" {
				return fantasy.NewTextErrorResponse("missing task_id"), nil
			}
			result, err := service.StopTask(ctx, params.TaskID)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			data, err := json.MarshalIndent(result, "", "  ")
			if err != nil {
				return fantasy.ToolResponse{}, fmt.Errorf("encoding stopped task: %w", err)
			}
			return fantasy.NewTextResponse(string(data)), nil
		},
	)
}
