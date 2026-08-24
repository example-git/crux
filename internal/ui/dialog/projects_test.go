package dialog

import (
	"context"
	"errors"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type projectsTestWorkspace struct {
	workspace.Workspace
	projects []proto.ProjectInfo
	err      error
	calls    int
}

func (w *projectsTestWorkspace) ListProjects(context.Context) ([]proto.ProjectInfo, error) {
	w.calls++
	return w.projects, w.err
}

func newProjectsTestDialog(workspace *projectsTestWorkspace) *Projects {
	theme := styles.ThemeForProvider("")
	return NewProjects(&common.Common{Workspace: workspace, Styles: &theme})
}

func TestProjectsDialogLoadsAndSelectsExistingProjects(t *testing.T) {
	workspace := &projectsTestWorkspace{projects: []proto.ProjectInfo{
		{Slug: "active", Name: "Active Project", Status: "active", Selected: true, Completed: 2, Total: 4},
		{Slug: "finished", Name: "Finished Project", Status: "completed", Completed: 3, Total: 3},
	}}
	dialog := newProjectsTestDialog(workspace)
	cmd := dialog.InitialCmd()
	require.Zero(t, workspace.calls)

	dialog.HandleMsg(cmd())
	require.Equal(t, 1, workspace.calls)
	selected, ok := dialog.list.SelectedItem().(*ProjectItem)
	require.True(t, ok)
	require.Equal(t, "active", selected.ID())

	action, ok := dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionSelectProject)
	require.True(t, ok)
	require.Equal(t, "active", action.Slug)

	dialog.list.SetSelected(2)
	require.Nil(t, dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))

	dialog.list.SetSelected(0)
	action, ok = dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionSelectProject)
	require.True(t, ok)
	require.Empty(t, action.Slug)
}

func TestProjectsDialogDefaultsToDisabledAndReportsLoadErrors(t *testing.T) {
	workspace := &projectsTestWorkspace{}
	dialog := newProjectsTestDialog(workspace)
	dialog.HandleMsg(dialog.InitialCmd()())
	selected, ok := dialog.list.SelectedItem().(*ProjectItem)
	require.True(t, ok)
	require.Equal(t, "disabled", selected.ID())

	workspace.err = errors.New("projects unavailable")
	dialog.HandleMsg(dialog.InitialCmd()())
	require.EqualError(t, dialog.loadErr, "projects unavailable")
}
