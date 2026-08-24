package chat

import (
	"encoding/json"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestActionToolRenderersUseActionSpecificHeaders(t *testing.T) {
	sty := styles.CharmtonePantera()
	tests := []struct {
		name   string
		input  string
		result string
		want   string
		param  string
	}{
		{name: tools.MemoryListToolName, input: `{"scope":"project"}`, result: `[]`, want: "List Memories", param: "project"},
		{name: tools.MemoryUpsertToolName, input: `{"scope":"user","name":"Rendering preference","topic":"rendering"}`, result: `Saved user memory rendering.md.`, want: "Save Memory", param: "Rendering preference"},
		{name: tools.MemoryRemoveToolName, input: `{"scope":"project","topic":"old-topic"}`, result: `Removed project memory old-topic.`, want: "Remove Memory", param: "old-topic"},
		{name: tools.ProjectCreateToolName, input: `{"name":"Parity","slug":"parity"}`, result: `Created and activated project.`, want: "Create Project", param: "Parity"},
		{name: tools.ProjectStatusToolName, input: `{}`, result: `No project is active for this workspace.`, want: "Project Status"},
		{name: tools.ProjectUpdateToolName, input: `{"id":"T1","completed":true}`, result: `Updated T1.`, want: "Update Project", param: "T1"},
		{name: tools.ProjectNotesToolName, input: `{"content":"evidence"}`, result: `Appended notes.`, want: "Add Project Note"},
		{name: tools.ProjectCompleteToolName, input: `{}`, result: `Completed project.`, want: "Complete Project"},
		{name: tools.TaskListToolName, input: `{}`, result: `[]`, want: "List Tasks"},
		{name: tools.TaskOutputToolName, input: `{"task_id":"b12345678"}`, result: `{"task":{"id":"b12345678","state":{"status":"running"}},"retrieval_status":"not_ready"}`, want: "Task Output", param: "b12345678"},
		{name: tools.TaskStopToolName, input: `{"task_id":"b12345678"}`, result: `{"id":"b12345678","state":{"status":"killed"}}`, want: "Stop Task", param: "b12345678"},
		{name: tools.TaskContinueToolName, input: `{"task_id":"a12345678","prompt":"continue"}`, result: `{"id":"a87654321","state":{"status":"running"},"child_session_id":"child"}`, want: "Continue Task", param: "a12345678"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call := message.ToolCall{ID: test.name, Name: test.name, Input: test.input, Finished: true}
			result := &message.ToolResult{ToolCallID: call.ID, Content: test.result}
			view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(100))
			require.Contains(t, view, test.want)
			if test.param != "" {
				require.Contains(t, view, test.param)
			}
			require.NotContains(t, view, test.input)
		})
	}
}

func TestActionToolRenderersSummarizeStructuredResults(t *testing.T) {
	sty := styles.CharmtonePantera()
	tasks, err := json.Marshal([]managedtask.View{
		{ID: "b12345678", Type: managedtask.TypeShell, Description: "compile package", State: managedtask.State{Status: managedtask.StatusRunning}},
		{ID: "a12345678", Type: managedtask.TypeAgent, Description: "review changes", State: managedtask.State{Status: managedtask.StatusCompleted}},
	})
	require.NoError(t, err)
	call := message.ToolCall{ID: "tasks", Name: tools.TaskListToolName, Input: `{}`, Finished: true}
	result := &message.ToolResult{ToolCallID: call.ID, Content: string(tasks)}

	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(100))
	require.Contains(t, view, "2 tasks")
	require.Contains(t, view, "b12345678 · running · compile package")
	require.Contains(t, view, "a12345678 · completed · review changes")
	require.NotContains(t, view, `"ownership"`)
	require.NotContains(t, view, `"usage"`)
}

func TestBackgroundAgentLaunchUsesConciseResult(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "agent", Name: agent.AgentToolName, Input: `{"prompt":"review changes","subagent_type":"reviewer","run_in_background":true}`, Finished: true}
	metadata, err := json.Marshal(agent.AgentResponseMetadata{Background: true, TaskID: "a12345678", ChildSessionID: "child-session"})
	require.NoError(t, err)
	result := &message.ToolResult{
		ToolCallID: call.ID,
		Content:    "Background agent started with ID: a12345678\n\nChild session ID: child-session",
		Metadata:   string(metadata),
	}

	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(100))
	require.Contains(t, view, "Background Agent")
	require.Contains(t, view, "Started a12345678 · child session ready")
	require.NotContains(t, view, "Child session ID:")
}
