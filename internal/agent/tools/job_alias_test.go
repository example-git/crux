package tools

import (
	"encoding/json"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestJobListDelegatesToUnifiedTaskListAndFiltersAgents(t *testing.T) {
	manager := shell.NewBackgroundShellManager(t.TempDir())
	service := &taskServiceStub{tasks: []managedtask.View{
		{ID: "b12345678", Type: managedtask.TypeShell, Description: "persisted shell", State: managedtask.State{Status: managedtask.StatusCompleted}},
		{ID: "a12345678", Type: managedtask.TypeAgent, Description: "agent", State: managedtask.State{Status: managedtask.StatusRunning}},
	}}
	tool := NewJobListTool(service, manager)

	response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "job-list", Name: JobListToolName, Input: `{}`})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Contains(t, response.Content, "b12345678")
	require.Contains(t, response.Content, "persisted shell")
	require.Contains(t, response.Content, "completed")
	require.NotContains(t, response.Content, "a12345678")
}

func TestJobKillDelegatesToUnifiedTaskStop(t *testing.T) {
	manager := shell.NewBackgroundShellManager(t.TempDir())
	service := &taskServiceStub{stopped: managedtask.View{
		ID:          "b12345678",
		Type:        managedtask.TypeShell,
		Description: "shell",
		State:       managedtask.State{Status: managedtask.StatusKilled},
	}}
	tool := NewJobKillTool(service, manager)
	input, err := json.Marshal(JobKillParams{ShellID: "b12345678"})
	require.NoError(t, err)

	response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "job-kill", Name: JobKillToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Equal(t, "b12345678", service.stopID)
	require.Equal(t, "Background shell b12345678 is killed", response.Content)

	var metadata JobKillResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.Equal(t, "b12345678", metadata.ShellID)
	require.Equal(t, "shell", metadata.Description)
}

func TestJobAliasesRejectMissingShellID(t *testing.T) {
	manager := shell.NewBackgroundShellManager(t.TempDir())
	service := &taskServiceStub{}

	for _, tool := range []fantasy.AgentTool{
		NewJobOutputTool(service, manager),
		NewJobKillTool(service, manager),
	} {
		response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "missing", Input: `{}`})
		require.NoError(t, err)
		require.True(t, response.IsError)
		require.Equal(t, "missing shell_id", response.Content)
	}
}
