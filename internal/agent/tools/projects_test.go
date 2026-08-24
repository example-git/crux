package tools

import (
	"testing"

	"github.com/example-git/crux/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestProjectToolsManageDurableProjectLifecycle(t *testing.T) {
	workingDir := t.TempDir()
	service := projects.NewServiceAt(t.TempDir())

	response := runServiceTool(t, NewProjectStatusTool(service, workingDir), ProjectStatusToolName, struct{}{})
	require.Equal(t, "No project is active for this workspace.", response.Content)

	response = runServiceTool(t, NewProjectCreateTool(service, workingDir), ProjectCreateToolName, ProjectCreateParams{
		Name:            "Tool Project",
		Slug:            "tool-project",
		Goal:            "Exercise project tools",
		SuccessCriteria: []string{"Lifecycle completes"},
		Tasks: []ProjectDefinitionTask{
			{ID: "T1", Content: "Implement lifecycle"},
			{ID: "T1.1", Content: "Persist progress", ParentID: "T1"},
		},
	})
	require.Contains(t, response.Content, "Created and activated project")
	require.Contains(t, response.Content, "tool-project.notes.md")

	response = runServiceTool(t, NewProjectStatusTool(service, workingDir), ProjectStatusToolName, struct{}{})
	require.Contains(t, response.Content, `"slug": "tool-project"`)
	require.Contains(t, response.Content, `"id": "T1.1"`)
	require.Contains(t, response.Content, `"current_goal"`)

	response = runServiceTool(t, NewProjectUpdateTool(service, workingDir), ProjectUpdateToolName, ProjectUpdateParams{ID: "T1", Completed: true})
	require.Contains(t, response.Content, "cannot be completed before subtask")
	response = runServiceTool(t, NewProjectUpdateTool(service, workingDir), ProjectUpdateToolName, ProjectUpdateParams{ID: "T1.1", Completed: true, Note: "Persisted"})
	require.Contains(t, response.Content, "Updated T1.1")
	response = runServiceTool(t, NewProjectNotesTool(service, workingDir), ProjectNotesToolName, ProjectNotesParams{Content: "Durable observation"})
	require.Contains(t, response.Content, "Appended notes")

	response = runServiceTool(t, NewProjectCompleteTool(service, workingDir), ProjectCompleteToolName, struct{}{})
	require.Contains(t, response.Content, "incomplete items")
	for _, id := range []string{"T1", "C1"} {
		response = runServiceTool(t, NewProjectUpdateTool(service, workingDir), ProjectUpdateToolName, ProjectUpdateParams{ID: id, Completed: true})
		require.Contains(t, response.Content, "Updated "+id)
	}
	response = runServiceTool(t, NewProjectCompleteTool(service, workingDir), ProjectCompleteToolName, struct{}{})
	require.Contains(t, response.Content, "Completed project")

	document, err := service.Get("tool-project")
	require.NoError(t, err)
	require.Equal(t, projects.StatusCompleted, document.Metadata.Status)
	require.Contains(t, document.Notes, "`T1.1`: Persisted")
	require.Contains(t, document.Notes, "Durable observation")
}

func TestProjectCreateToolRejectsInvalidExplicitSlug(t *testing.T) {
	service := projects.NewServiceAt(t.TempDir())
	response := runServiceTool(t, NewProjectCreateTool(service, t.TempDir()), ProjectCreateToolName, ProjectCreateParams{
		Name:            "Invalid",
		Slug:            "Invalid Slug",
		Goal:            "Must fail",
		SuccessCriteria: []string{"Rejected"},
		Tasks:           []ProjectDefinitionTask{{ID: "T1", Content: "Reject invalid slug"}},
	})
	require.Contains(t, response.Content, "invalid project slug")
}
