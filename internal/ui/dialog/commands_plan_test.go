package dialog

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

func TestCommandsIncludesAgentDefinitionCreator(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	var found bool
	for _, item := range commands.defaultCommands() {
		if item.id != "create_agent" {
			continue
		}
		action, ok := item.action.(ActionOpenDialog)
		found = ok && action.DialogID == AgentDefinitionsID
		break
	}
	require.True(t, found)
}

func TestCommandsIncludesTmuxSessionsPicker(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	for _, item := range commands.defaultCommands() {
		if item.id != "tmux_sessions" {
			continue
		}
		action, ok := item.action.(ActionOpenDialog)
		require.True(t, ok)
		require.Equal(t, TmuxSessionsID, action.DialogID)
		return
	}
	t.Fatal("tmux sessions command not found")
}

func TestCommandsIncludesProjectsPicker(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	for _, item := range commands.defaultCommands() {
		if item.id != "projects" {
			continue
		}
		action, ok := item.action.(ActionOpenDialog)
		require.True(t, ok)
		require.Equal(t, ProjectsID, action.DialogID)
		return
	}
	t.Fatal("projects command not found")
}

func TestCommandsIncludesSidebarToggleAtWideWidthWithActiveSession(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}

	withSession, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "s1", true, false, false, nil, nil)
	require.NoError(t, err)
	withoutSession, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	withSession.SetWindowWidth(sidebarCompactModeBreakpoint - 1)
	require.False(t, hasCommandAction[ActionToggleCompactMode](withSession.defaultCommands(), "toggle_sidebar"))

	withSession.SetWindowWidth(sidebarCompactModeBreakpoint)
	require.True(t, hasCommandAction[ActionToggleCompactMode](withSession.defaultCommands(), "toggle_sidebar"))

	withoutSession.SetWindowWidth(sidebarCompactModeBreakpoint)
	require.False(t, hasCommandAction[ActionToggleCompactMode](withoutSession.defaultCommands(), "toggle_sidebar"))
}

func hasCommandAction[T Action](items []*CommandItem, id string) bool {
	for _, item := range items {
		if item.id == id {
			_, ok := item.action.(T)
			return ok
		}
	}
	return false
}

func TestCommandsIncludesPlanToggleForActiveSession(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}

	withSession, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "s1", true, false, false, nil, nil)
	require.NoError(t, err)
	withoutSession, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	var found bool
	for _, item := range withSession.defaultCommands() {
		if item.id == "toggle_plan" {
			_, found = item.action.(ActionTogglePlanMode)
			break
		}
	}
	require.True(t, found)

	for _, item := range withoutSession.defaultCommands() {
		require.NotEqual(t, "toggle_plan", item.id)
	}
}
