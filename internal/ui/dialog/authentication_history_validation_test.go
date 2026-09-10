package dialog

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func historyValidationScreen(d Dialog, w, h int) string {
	screen := uv.NewScreenBuffer(w, h)
	d.Draw(screen, uv.Rect(0, 0, w, h))
	return ansi.Strip(screen.String())
}
func TestAuthenticationHistoryValidationActionsFitAndDispatch(t *testing.T) {
	theme := styles.ThemeForProvider("")
	for _, size := range [][2]int{{40, 12}, {100, 32}} {
		t.Run(fmt.Sprintf("%dx%d", size[0], size[1]), func(t *testing.T) {
			d := NewAuthenticationHistory(&common.Common{Styles: &theme})
			d.SetRows([]AuthenticationHistoryRow{{Key: "exact-row", Label: "Original provider operation", Details: strings.Repeat("original unknown progress\n", 30) + "FINAL ORIGINAL DETAIL", Review: true, Recover: true, RetryRecovery: true, Repair: true, ApplyRepair: true, AbandonLocal: true, RetryAbandonLocal: true, AbandonPublication: true, RetryAbandonPublication: true, AbandonReview: true, RetryAbandonReview: true}})
			d.SetState("Select an explicit action", false)
			action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionAuthenticationHistory)
			require.Equal(t, "open", action.Kind)
			require.Equal(t, "exact-row", action.Key)
			d.ShowDetails()
			require.Contains(t, historyValidationScreen(d, size[0], size[1]), "actions")
			d.HandleMsg(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
			choices := d.actionChoices()
			require.Len(t, choices, 13)
			for i, choice := range choices {
				d.actionIndex = i
				frame := historyValidationScreen(d, size[0], size[1])
				require.Contains(t, frame, choice.key, "selected action key must be visible at the requested terminal size")
				require.Contains(t, frame, "choose")
				for _, line := range strings.Split(frame, "\n") {
					require.LessOrEqual(t, ansi.StringWidth(line), size[0])
				}
				selected := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionAuthenticationHistory)
				require.Equal(t, choice.kind, selected.Kind)
				require.Equal(t, "exact-row", selected.Key)
				require.Equal(t, d.Generation(), selected.Generation)
				require.False(t, d.actions, "dispatch must expose the operation status, not leave an action list over it")
				d.HandleMsg(tea.KeyPressMsg{Code: 'a', Mod: tea.ModCtrl})
			}
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEscape})
			for range 100 {
				d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
			}
			require.Contains(t, historyValidationScreen(d, size[0], size[1]), "FINAL ORIGINAL DETAIL")
			d.SetState("Waiting for the exact original receipt", true)
			require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'x', Mod: tea.ModAlt}))
			require.Equal(t, "cancel", d.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}).(ActionAuthenticationHistory).Kind)
		})
	}
}
func TestAuthenticationHistoryValidationRetiredReviewIsNotSuccess(t *testing.T) {
	theme := styles.ThemeForProvider("")
	d := NewAuthenticationReconciliation(&common.Common{Styles: &theme}, "original unknown acknowledgement", nil)
	d.SetState("Recovery abandoned; original outcome unchanged", "", false, true, true, true)
	d.SetRetired(true)
	for _, key := range []tea.KeyPressMsg{{Code: tea.KeyEnter}, {Code: 'y', Mod: tea.ModCtrl}, {Code: 't', Mod: tea.ModCtrl}, {Code: 'r', Mod: tea.ModCtrl}} {
		require.Nil(t, d.HandleMsg(key))
	}
	for _, size := range [][2]int{{40, 12}, {100, 32}} {
		frame := historyValidationScreen(d, size[0], size[1])
		require.NotContains(t, frame, "publication completed")
		require.Contains(t, frame, "retained review")
		require.Len(t, d.ShortHelp(), 1)
	}
}

func TestAuthenticationHistoryValidationSavedControlsAndSourceChoice(t *testing.T) {
	theme := styles.ThemeForProvider("")
	d := NewSavedAuthentication(&common.Common{Styles: &theme})
	d.SetRows([]SavedAuthenticationChoice{{Label: "Exact saved owner and active account"}})
	d.SetState(strings.Repeat("Original operation remains unchanged. ", 20)+"LAST SAVED NOTICE", false, true)
	for _, size := range [][2]int{{40, 12}, {100, 32}} {
		frame := historyValidationScreen(d, size[0], size[1])
		for _, key := range []string{"enter", "ctrl+l", "ctrl+r", "ctrl+t", "esc"} {
			require.Contains(t, frame, key)
		}
		for range 100 {
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyPgDown})
		}
		require.Contains(t, historyValidationScreen(d, size[0], size[1]), "LAST SAVED NOTICE")
	}
	for _, item := range []struct {
		key  tea.KeyPressMsg
		kind string
	}{{tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl}, "reload"}, {tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}, "retry-reload"}, {tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl}, "status"}, {tea.KeyPressMsg{Code: tea.KeyEnter}, "review"}} {
		action := d.HandleMsg(item.key).(ActionSavedAuthentication)
		require.Equal(t, item.kind, action.Kind)
	}
	d.SetState("Reload pending", true, true)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Equal(t, "cancel", d.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}).(ActionSavedAuthentication).Kind)
}
