package model

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

func TestBackgroundNotificationExpandsWithSpaceAndLeftClick(t *testing.T) {
	ui := newTestUI()
	ui.keyMap = DefaultKeyMap()
	ui.dialog = dialog.NewOverlay()
	ui.focus = uiFocusMain
	msg := &message.Message{ID: "notification", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "<task-notification><task-id>a12345678</task-id><task-type>agent</task-type><status>failed</status><error-message>provider failure</error-message><lost-reason>worker unavailable</lost-reason><exit-code>7</exit-code><result>" + strings.Repeat("result row\n", 20) + "FINALRESULT</result></task-notification>"}}}
	items := chat.ExtractMessageItems(ui.com.Styles, msg, nil, "")
	require.Len(t, items, 1)
	item := items[0]
	ui.chat.SetMessages(item)
	require.Same(t, item, ui.chat.MessageItem(msg.ID+":task-notification"))
	require.Nil(t, ui.appendSessionMessage(*msg))
	require.Equal(t, 1, ui.chat.Len())
	ui.updateLayoutAndSize()
	ui.chat.SetSelected(0)
	ui.chat.Focus()
	require.NotContains(t, ansi.Strip(item.Render(80)), "FINALRESULT")
	ui.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	view := ansi.Strip(item.Render(80))
	for _, want := range []string{"FINALRESULT", "provider failure", "worker unavailable", "Exit code: 7"} {
		require.Contains(t, view, want)
	}
	ui.handleKeyPressMsg(tea.KeyPressMsg{Code: tea.KeySpace})
	require.NotContains(t, ansi.Strip(item.Render(80)), "FINALRESULT")
	ui.chat.ScrollToTop()
	handled, cmd := ui.chat.HandleMouseDown(3, 0)
	require.True(t, handled)
	require.NotNil(t, cmd)
	require.True(t, ui.chat.HandleMouseUp(3, 0))
	delayed, ok := cmd().(DelayedClickMsg)
	require.True(t, ok)
	require.True(t, ui.chat.HandleDelayedClick(delayed))
	require.Contains(t, ansi.Strip(item.Render(80)), "FINALRESULT")
}

func TestLongYesNoQuestionGetsBoundedEditorLayout(t *testing.T) {
	for _, state := range []uiState{uiLanding, uiChat} {
		for _, compact := range []bool{false, true} {
			ui := newTestUI()
			ui.state = state
			ui.isCompact = compact
			ui.width, ui.height = 60, 12
			ui.activeInline = dialog.NewYesNo(ui.com.Styles, question.Question{ID: "confirm", Text: "Continue?", Description: strings.Repeat("Long context\n\n", 40)})
			layout := ui.generateLayout(ui.width, ui.height)
			require.Greater(t, layout.editor.Dy(), 0)
			require.GreaterOrEqual(t, layout.editor.Min.Y, 0)
			require.LessOrEqual(t, layout.editor.Max.Y, ui.height)
			require.LessOrEqual(t, layout.editor.Dy(), ui.height)
		}
	}
}
