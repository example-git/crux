package model

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// TestThinkingBlocksAreSelfContainedAndSeparated verifies identical framed
// thoughts and normal list spacing in both initial and appended message paths.
func TestThinkingBlocksAreSelfContainedAndSeparated(t *testing.T) {
	for _, appendLive := range []bool{false, true} {
		u := newTestUI()
		user := chat.ExtractMessageItems(u.com.Styles, &message.Message{ID: "user", Role: message.User}, nil, "")[0]
		msg := &message.Message{
			ID: "thoughts", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "First thought\nSecond thought\nThird thought"},
				message.TextContent{Text: "Done"},
			},
		}
		item := chat.NewAssistantMessageItem(u.com.Styles, msg).(*chat.AssistantMessageItem)
		setItems := func(previous chat.MessageItem) int {
			if appendLive {
				u.chat.SetMessages(user, previous)
				u.chat.AppendMessages(item)
				return 1
			}
			u.chat.SetMessages(user, previous, item)
			return 1
		}
		if appendLive {
			u.chat.SetMessages(user)
			u.chat.AppendMessages(item)
		} else {
			u.chat.SetMessages(user, item)
		}
		require.Equal(t, 1, u.chat.list.GapAfter(0))
		for _, width := range []int{40, 80} {
			collapsed := ansi.Strip(item.Render(width))
			collapsedLines := strings.Split(collapsed, "\n")
			require.Contains(t, collapsed, "THOUGHTS ▾")
			require.NotContains(t, collapsed, "• First thought")
			require.True(t, item.ToggleExpanded())
			expanded := ansi.Strip(item.Render(width))
			expandedLines := strings.Split(expanded, "\n")
			require.Contains(t, expanded, "• First thought")
			require.Contains(t, expanded, "• Second thought")
			require.Contains(t, expanded, "• Third thought")
			require.NotContains(t, expanded, "THOUGHTS ▾")
			require.Equal(t, ansi.StringWidth(strings.TrimSpace(collapsedLines[0])), ansi.StringWidth(strings.TrimSpace(expandedLines[0])))
			require.False(t, item.ToggleExpanded())
		}
		tool := chat.NewShellItem(u.com.Styles, "echo result", "result", 0)
		previousIndex := setItems(tool)
		require.Equal(t, 1, u.chat.list.GapAfter(previousIndex))
		collapsed := ansi.Strip(item.Render(80))
		require.Contains(t, collapsed, "THOUGHTS ▾")
		require.NotContains(t, collapsed, "• First thought")
		require.True(t, item.ToggleExpanded())
		expanded := ansi.Strip(item.Render(80))
		require.Contains(t, expanded, "• First thought")
		require.Contains(t, expanded, "• Third thought")
	}
}

func TestChatToggleExpandedSelectedItem_AssistantMessage(t *testing.T) {
	t.Parallel()

	u := newTestUI()

	msg := &message.Message{
		ID:   "m-assist",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking about it\nchecking the result"},
		},
	}
	item := chat.NewAssistantMessageItem(u.com.Styles, msg)

	exp, ok := item.(chat.Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy chat.Expandable")

	u.chat.SetMessages(item)
	u.chat.SetSelected(0)

	u.chat.ToggleExpandedSelectedItem()
	require.False(t, exp.ToggleExpanded(),
		"keyboard toggle did not expand the initially collapsed thinking block")

	u.chat.ToggleExpandedSelectedItem()
	require.False(t, exp.ToggleExpanded(),
		"second keyboard toggle did not expand the re-collapsed thinking block")
}

func TestFocusedThinkingRowsKeepOuterAlignment(t *testing.T) {
	u := newTestUI()
	msg := &message.Message{
		ID:   "focused-thoughts",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "First thought\nSecond thought\nThird thought"},
			message.TextContent{Text: "Done"},
		},
	}
	item := chat.NewAssistantMessageItem(u.com.Styles, msg).(*chat.AssistantMessageItem)
	u.chat.SetMessages(item)
	u.chat.SetSize(80, 20)
	u.chat.SetSelected(0)
	u.chat.Focus()

	assertAligned := func() {
		screen := uv.NewScreenBuffer(80, 20)
		u.chat.Draw(screen, screen.Bounds())
		positions := make([]int, 0, 4)
		for _, line := range strings.Split(ansi.Strip(screen.Render()), "\n") {
			if index := strings.IndexAny(line, "╭│•╰"); index >= 0 {
				positions = append(positions, ansi.StringWidth(line[:index]))
			}
		}
		require.NotEmpty(t, positions)
		for _, position := range positions[1:] {
			require.Equal(t, positions[0], position, positions)
		}
	}

	assertAligned()
	handled, cmd := u.chat.HandleMouseDown(20, 1)
	require.True(t, handled)
	require.True(t, u.chat.HandleMouseUp(20, 1))
	require.NotNil(t, cmd)
	delayed, ok := cmd().(DelayedClickMsg)
	require.True(t, ok)
	require.True(t, u.chat.HandleDelayedClick(delayed))
	assertAligned()
}
