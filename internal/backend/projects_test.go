package backend

import (
	"testing"

	"github.com/example-git/crux/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestBackendListsSelectsAndDisablesProjects(t *testing.T) {
	backend, _ := newTestBackend(t)
	backend.projectService = projects.NewServiceAt(t.TempDir())
	workspace, _ := insertTestWorkspace(t, backend, t.TempDir())
	otherRoot := t.TempDir()
	_, err := backend.projectService.Create(projects.Definition{
		Name:            "Backend Project",
		Slug:            "backend-project",
		Goal:            "Exercise backend transport",
		SuccessCriteria: []string{"Selection is transported"},
		Tasks:           []projects.DefinitionTask{{ID: "T1", Content: "Select the project"}},
	}, otherRoot)
	require.NoError(t, err)

	entries, err := backend.ListProjects(workspace.ID)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.False(t, entries[0].Selected)
	require.Equal(t, 0, entries[0].Completed)
	require.Equal(t, 2, entries[0].Total)

	require.NoError(t, backend.SelectProject(workspace.ID, "backend-project"))
	entries, err = backend.ListProjects(workspace.ID)
	require.NoError(t, err)
	require.True(t, entries[0].Selected)

	require.NoError(t, backend.SelectProject(workspace.ID, ""))
	entries, err = backend.ListProjects(workspace.ID)
	require.NoError(t, err)
	require.False(t, entries[0].Selected)
}

func TestBackendProjectSelectionRejectsInvalidInputs(t *testing.T) {
	backend, _ := newTestBackend(t)
	backend.projectService = projects.NewServiceAt(t.TempDir())
	workspace, _ := insertTestWorkspace(t, backend, t.TempDir())

	require.Error(t, backend.SelectProject(workspace.ID, "missing"))
	_, err := backend.ListProjects("missing-workspace")
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}
