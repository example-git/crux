package dialog

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
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

func TestCommandsGoalInsertsGoalCommand(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "", false, false, false, nil, nil)
	require.NoError(t, err)

	for _, item := range commands.defaultCommands() {
		if item.ID() != "goal" {
			continue
		}
		action, ok := item.Action().(ActionInsertCommand)
		require.True(t, ok)
		require.Equal(t, "/goal ", action.Value)
		return
	}
	t.Fatal("goal command not found")
}

func TestCommandsGoalControlsShowUsage(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "s1", true, false, false, nil, nil)
	require.NoError(t, err)
	items := make(map[string]*CommandItem)
	for _, item := range commands.defaultCommands() {
		items[item.ID()] = item
	}
	for _, test := range []struct {
		id, command, usage, meaning string
	}{
		{"goal", "/goal ", "/goal <objective> or /goal set <objective>", "starts a goal"},
		{"goal-status", "/goal status", "/goal or /goal status", "objective and state"},
		{"goal-stop", "/goal stop", "/goal stop", "cancels now; /goal resume continues later"},
		{"goal-pause", "/goal pause", "/goal pause", "lets this turn finish, then pauses"},
		{"goal-resume", "/goal resume", "/goal resume", "continues the saved goal"},
		{"goal-complete", "/goal complete", "/goal complete", "marks the goal as finished"},
		{"goal-clear", "/goal clear", "/goal clear", "removes the saved goal"},
	} {
		t.Run(test.id, func(t *testing.T) {
			require.Contains(t, items, test.id)
			item := items[test.id]
			action, ok := item.Action().(ActionInsertCommand)
			require.True(t, ok)
			require.Equal(t, test.command, action.Value)
			require.Contains(t, item.Filter(), test.usage)
			for _, width := range []int{60, 80} {
				rendered := ansi.Strip(item.Render(width))
				require.Contains(t, rendered, test.usage)
				require.Contains(t, rendered, test.meaning)
			}
			if test.id == "goal-stop" {
				require.Equal(t, "esc", item.Shortcut())
			}
		})
	}
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

func TestCommandsStopGoalInsertsStopCommand(t *testing.T) {
	theme := styles.ThemeForProvider("")
	ws := &mcpServersTestWorkspace{cfg: &config.Config{}}
	commands, err := NewCommands(&common.Common{Workspace: ws, Styles: &theme}, "s1", true, false, false, nil, nil)
	require.NoError(t, err)
	for _, item := range commands.defaultCommands() {
		if item.ID() == "goal-stop" {
			action, ok := item.Action().(ActionInsertCommand)
			require.True(t, ok)
			require.Equal(t, "/goal stop", action.Value)
			return
		}
	}
	t.Fatal("stop goal command not found")
}
