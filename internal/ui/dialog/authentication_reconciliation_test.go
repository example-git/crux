package dialog

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationReconciliationDialogChoicesAndGates(t *testing.T) {
	theme := styles.ThemeForProvider("")
	com := &common.Common{Styles: &theme, Workspace: &struct{ workspace.Workspace }{}}
	d := NewAuthenticationReconciliation(com, "original partial result", []AuthenticationRow{{AccountID: "first", Label: "First"}, {AccountID: "second", Label: "Second"}})
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}))
	action := d.HandleMsg(tea.KeyPressMsg{Code: '2', Mod: tea.ModAlt}).(ActionAuthenticationReconciliation)
	require.Equal(t, workspace.ProviderAuthenticationReviewChoice{Kind: "saved-account", AccountID: "first"}, action.Choice)
	action = d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}).(ActionAuthenticationReconciliation)
	require.Equal(t, "second", action.Choice.AccountID)
	d.SetState("ready", "exact preview", false, true, true, false)
	require.Equal(t, "apply", d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}).(ActionAuthenticationReconciliation).Kind)
	d.HandleMsg(tea.KeyPressMsg{Code: '3', Mod: tea.ModAlt})
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}), "different choice invalidates displayed apply authority")
	d.HandleMsg(tea.KeyPressMsg{Code: '2', Mod: tea.ModAlt})
	d.SetState("ready", "exact preview", false, true, true, true)
	d.HandleMsg(tea.PasteMsg{Content: "changed"})
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}), "editing account input invalidates preview")
	d.SetState("waiting", "", true, true, true, true)
	for _, kp := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'r', Mod: tea.ModCtrl}, {Code: 'y', Mod: tea.ModCtrl}, {Code: 't', Mod: tea.ModCtrl}, {Code: '1', Mod: tea.ModAlt}} {
		require.Nil(t, d.HandleMsg(kp))
	}
	require.IsType(t, ActionClose{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape}))
	require.Equal(t, "cancel", d.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}).(ActionAuthenticationReconciliation).Kind)
	d.SetState("complete", "exact preview", false, false, false, false)
	d.SetFinished(true)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Len(t, d.ShortHelp(), 1, "completed records advertise only close, not inert review actions")
}

func TestAuthenticationReconciliationDialogFitsAndScrolls(t *testing.T) {
	theme := styles.ThemeForProvider("")
	d := NewAuthenticationReconciliation(&common.Common{Styles: &theme}, strings.Repeat("retained history ", 80), nil)
	d.SetState("Review ready", "Preview: exact saved account\nChanged: authentication\nReceiver revision: 3", false, true, true, false)
	for _, size := range [][2]int{{80, 24}, {40, 12}, {20, 8}, {5, 3}} {
		screen := uv.NewScreenBuffer(size[0], size[1])
		d.Draw(screen, uv.Rect(0, 0, size[0], size[1]))
		for _, line := range strings.Split(screen.String(), "\n") {
			require.LessOrEqual(t, ansi.StringWidth(line), size[0])
		}
	}
	screen := uv.NewScreenBuffer(80, 24)
	d.Draw(screen, uv.Rect(0, 0, 80, 24))
	output := ansi.Strip(screen.String())
	require.Contains(t, output, "Alt+1 original intent")
	require.Contains(t, output, "ctrl+y")
	require.Contains(t, output, "apply preview")
	for range 100 {
		d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
	}
	screen = uv.NewScreenBuffer(80, 24)
	d.Draw(screen, uv.Rect(0, 0, 80, 24))
	require.Contains(t, ansi.Strip(screen.String()), "No files are repaired or reloaded")
}
