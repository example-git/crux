package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

type taskServiceStub struct {
	tasks             []managedtask.View
	output            managedtask.OutputResult
	stopped           managedtask.View
	continued         managedtask.View
	err               error
	outputID          string
	outputWait        bool
	outputTimeout     time.Duration
	stopID            string
	continueID        string
	continueSessionID string
	continuePrompt    string
	continueCallID    string
}

func (s *taskServiceStub) ListTasks() []managedtask.View {
	return s.tasks
}

func (s *taskServiceStub) TaskOutput(_ context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error) {
	s.outputID = id
	s.outputWait = wait
	s.outputTimeout = timeout
	return s.output, s.err
}

func (s *taskServiceStub) StopTask(_ context.Context, id string) (managedtask.View, error) {
	s.stopID = id
	return s.stopped, s.err
}

func (s *taskServiceStub) ContinueTask(_ context.Context, id, parentSessionID, prompt, originToolCallID string) (managedtask.View, error) {
	s.continueID = id
	s.continueSessionID = parentSessionID
	s.continuePrompt = prompt
	s.continueCallID = originToolCallID
	return s.continued, s.err
}

func runTaskTool(t *testing.T, tool fantasy.AgentTool, ctx context.Context, name string, input any) fantasy.ToolResponse {
	t.Helper()
	data, err := json.Marshal(input)
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "tool-call", Name: name, Input: string(data)})
	require.NoError(t, err)
	return response
}

func TestTaskToolsDispatchUnifiedOperations(t *testing.T) {
	service := &taskServiceStub{
		tasks:     []managedtask.View{{ID: "b12345678", Type: managedtask.TypeShell}},
		output:    managedtask.OutputResult{Task: managedtask.View{ID: "a12345678"}, Output: "result"},
		stopped:   managedtask.View{ID: "b12345678", State: managedtask.State{Status: managedtask.StatusKilled}},
		continued: managedtask.View{ID: "a87654321", ContinuationOf: "a12345678"},
	}

	response := runTaskTool(t, NewTaskListTool(service), t.Context(), TaskListToolName, struct{}{})
	require.Contains(t, response.Content, "b12345678")

	response = runTaskTool(t, NewTaskOutputTool(service), t.Context(), TaskOutputToolName, TaskOutputParams{TaskID: "a12345678"})
	require.Contains(t, response.Content, "result")
	require.Equal(t, "a12345678", service.outputID)
	require.True(t, service.outputWait)
	require.Equal(t, 30*time.Second, service.outputTimeout)

	response = runTaskTool(t, NewTaskStopTool(service), t.Context(), TaskStopToolName, TaskStopParams{TaskID: "b12345678"})
	require.Contains(t, response.Content, "killed")
	require.Equal(t, "b12345678", service.stopID)

	ctx := context.WithValue(t.Context(), SessionIDContextKey, "parent-session")
	response = runTaskTool(t, NewTaskContinueTool(service), ctx, TaskContinueToolName, TaskContinueParams{TaskID: "a12345678", Prompt: "continue"})
	require.Contains(t, response.Content, "a87654321")
	require.Equal(t, "a12345678", service.continueID)
	require.Equal(t, "parent-session", service.continueSessionID)
	require.Equal(t, "continue", service.continuePrompt)
	require.Equal(t, "tool-call", service.continueCallID)
}

func TestTaskToolsRejectInvalidExplicitInputs(t *testing.T) {
	service := &taskServiceStub{}
	timeout := 600001
	response := runTaskTool(t, NewTaskOutputTool(service), t.Context(), TaskOutputToolName, TaskOutputParams{TaskID: "b12345678", TimeoutMS: &timeout})
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "between 0 and 600000")

	input, err := json.Marshal(TaskContinueParams{TaskID: "a12345678", Prompt: "continue"})
	require.NoError(t, err)
	_, err = NewTaskContinueTool(service).Run(t.Context(), fantasy.ToolCall{ID: "tool-call", Name: TaskContinueToolName, Input: string(input)})
	require.ErrorContains(t, err, "session id missing")

	service.err = errors.New("task not found")
	response = runTaskTool(t, NewTaskStopTool(service), t.Context(), TaskStopToolName, TaskStopParams{TaskID: "b12345678"})
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "task not found")
}
