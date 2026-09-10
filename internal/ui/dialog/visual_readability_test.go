package dialog

import (
	"fmt"
	"image"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/question"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestYesNoShortViewportScrollsAndKeepsButtonsClickable(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, width := range []int{9, 40, 80} {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			dialog := NewYesNo(&sty, question.Question{ID: "confirm", Text: "Continue?", Description: strings.Repeat("Long description with context.\n\n", 12) + "FINALCONTEXT"})
			area := image.Rect(2, 1, width+2, 7)
			draw := func() string {
				screen := uv.NewScreenBuffer(width+4, 9)
				dialog.Draw(screen, area)
				return ansi.Strip(screen.Render())
			}
			require.Contains(t, draw(), "Cont")
			require.Greater(t, dialog.contentHeight, dialog.viewportHeight)
			dialog.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
			view := draw()
			require.Contains(t, view, "Yes")
			require.Contains(t, view, "No")
			clicked := false
			for y := area.Min.Y; y < area.Max.Y && !clicked; y++ {
				for x := area.Min.X; x < area.Max.X; x++ {
					if common.HitButtonIndex(dialog.compositor, x, y) == 0 {
						handled, done := dialog.HandleMouseClick(x, y)
						require.True(t, handled)
						require.True(t, done)
						require.True(t, *dialog.Response().Yes)
						clicked = true
						break
					}
				}
			}
			require.True(t, clicked)
			dialog.HandleWheel(0, -float64(dialog.contentHeight))
			require.Contains(t, draw(), "Cont")
			require.Zero(t, dialog.scrollOffset)
			dialog.HandleKey(tea.KeyPressMsg{Code: tea.KeyRight})
			require.Contains(t, draw(), "No")
			_, done := dialog.HandleMouseClick(0, 0)
			require.False(t, done)
		})
	}
}

func TestYesNoNoteRemainsEditableAfterScrollingAndResize(t *testing.T) {
	sty := styles.CharmtonePantera()
	dialog := NewYesNo(&sty, question.Question{ID: "confirm", Text: "Continue?", Description: strings.Repeat("Context\n\n", 25)})
	dialog.SetFocused(true)
	area := image.Rect(0, 0, 40, 6)
	screen := uv.NewScreenBuffer(40, 6)
	dialog.Draw(screen, area)
	dialog.HandleKey(tea.KeyPressMsg{Code: 'n', Mod: tea.ModAlt})
	dialog.HandlePaste(tea.PasteMsg{Content: "Remember this note"})
	cursor := dialog.Draw(screen, area)
	require.NotNil(t, cursor)
	require.Less(t, cursor.Y, area.Dy())
	require.Contains(t, ansi.Strip(screen.Render()), "Remember this note")
	dialog.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Equal(t, "Remember this note", dialog.Response().Notes["_question"])
	dialog.HandleKey(tea.KeyPressMsg{Code: tea.KeyEnd})
	screen = uv.NewScreenBuffer(20, 5)
	dialog.Draw(screen, image.Rect(0, 0, 20, 5))
	require.Contains(t, ansi.Strip(screen.Render()), "Remember this")
	require.LessOrEqual(t, dialog.scrollOffset, dialog.contentHeight-dialog.viewportHeight)
}

func TestShellDetailPansLongLinesAndConsumesCoalescedWheel(t *testing.T) {
	task := managedtask.View{ID: "b12345678", Type: managedtask.TypeShell, Command: "print output", State: managedtask.State{Status: managedtask.StatusCompleted}}
	lines := make([]string, 40)
	for index := range lines {
		lines[index] = fmt.Sprintf("line%02d ", index) + strings.Repeat("x", 100) + "ENDMARK"
	}
	dialog := newTasksTestDialog(&tasksTestWorkspace{tasks: []managedtask.View{task}, output: managedtask.OutputResult{Task: task, Output: strings.Join(lines, "\n")}})
	runTaskDialogAction(t, dialog, dialog.HandleMsg(dialog.InitialCmd()()))
	draw := func() string {
		screen := uv.NewScreenBuffer(80, 30)
		dialog.Draw(screen, screen.Bounds())
		return ansi.Strip(screen.Render())
	}
	require.NotContains(t, draw(), "ENDMARK")
	require.False(t, dialog.terminalRect.Empty())
	require.Equal(t, dialog.terminalContentWidth+dialog.com.Styles.Dialog.TerminalPanel.GetHorizontalFrameSize(), dialog.terminalRect.Dx())
	require.Equal(t, dialog.terminalViewportHeight+2, dialog.terminalRect.Dy())
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab})
	for range 20 {
		dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyRight})
	}
	require.Greater(t, dialog.terminalXOffset, 0)
	require.Contains(t, draw(), "ENDMARK")
	mouse := tea.Mouse{X: dialog.terminalRect.Min.X + 1, Y: dialog.terminalRect.Min.Y + 1}
	dialog.HandleMsg(common.CoalescedWheelMsg{Mouse: mouse, DeltaY: -3})
	require.Equal(t, 3, dialog.terminalScroll)
	previous := dialog.terminalXOffset
	dialog.HandleMsg(common.CoalescedWheelMsg{Mouse: mouse, DeltaX: -4})
	require.Equal(t, previous-4, dialog.terminalXOffset)
	dialog.HandleMsg(common.CoalescedWheelMsg{Mouse: tea.Mouse{X: -1, Y: -1}, DeltaY: -3})
	require.Equal(t, 3, dialog.terminalScroll)
	dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnd})
	require.Zero(t, dialog.terminalScroll)
}
