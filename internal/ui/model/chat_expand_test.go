package model

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

// TestChatToggleExpandedSelectedItem_AssistantMessage is the regression test
// for §4.8.1: before the fix, AssistantMessageItem.ToggleExpanded returned no
// value so the type did not satisfy chat.Expandable, and the keyboard-driven
// ToggleExpandedSelectedItem path silently skipped thinking blocks. This
// wires a real Chat with an AssistantMessageItem, selects it, invokes
// ToggleExpandedSelectedItem, and asserts the thinking block actually
// flipped.
func TestThinkingOriginFollowsConversationOrder(t *testing.T) {
	for _, appendLive := range []bool{false, true} {
		u := newTestUI()
		user := chat.ExtractMessageItems(u.com.Styles, &message.Message{ID: "user", Role: message.User}, nil, "")[0]
		msg := &message.Message{
			ID: "thoughts", Role: message.Assistant,
			Parts: []message.ContentPart{
				message.ReasoningContent{Thinking: "First thought\nSecond thought\nThird thought"},
			},
		}
		item := chat.NewAssistantMessageItem(u.com.Styles, msg).(*chat.AssistantMessageItem)
		if appendLive {
			u.chat.SetMessages(user)
			u.chat.AppendMessages(item)
		} else {
			u.chat.SetMessages(user, item)
		}
		for _, width := range []int{40, 80} {
			require.Contains(t, item.Render(width), "╭── Expand Thoughts")
			require.NotContains(t, item.Render(width), "╰── Expand Thoughts")
			require.True(t, item.ToggleExpanded())
			rendered := ansi.Strip(item.Render(width))
			require.Contains(t, rendered, "╭── First thought")
			require.Contains(t, rendered, "├── Second thought")
			require.Contains(t, rendered, "╰── Third thought")
			require.False(t, item.ToggleExpanded())
		}
		tool := chat.NewShellItem(u.com.Styles, "echo result", "result", 0)
		if appendLive {
			u.chat.SetMessages(user, tool)
			u.chat.AppendMessages(item)
		} else {
			u.chat.SetMessages(user, tool, item)
		}
		require.Contains(t, item.Render(80), "╰── Expand Thoughts")
		require.NotContains(t, item.Render(80), "╭── Expand Thoughts")
	}
}

func TestChatToggleExpandedSelectedItem_AssistantMessage(t *testing.T) {
	t.Parallel()

	u := newTestUI()

	msg := &message.Message{
		ID:   "m-assist",
		Role: message.Assistant,
		Parts: []message.ContentPart{
			message.ReasoningContent{Thinking: "thinking about it"},
		},
	}
	item := chat.NewAssistantMessageItem(u.com.Styles, msg)

	// The keyboard expand path uses the generic Expandable interface;
	// verifying satisfaction at runtime guards the contract.
	exp, ok := item.(chat.Expandable)
	require.True(t, ok, "AssistantMessageItem must satisfy chat.Expandable")

	u.chat.SetMessages(item)
	u.chat.SetSelected(0)

	// First keyboard toggle should expand. Immediately follow with a
	// direct ToggleExpanded: it flips the now-expanded item back to
	// collapsed and returns false. If the keyboard path had silently
	// skipped the item (the bug), the item would still be collapsed and
	// the direct toggle would return true.
	u.chat.ToggleExpandedSelectedItem()
	require.False(t, exp.ToggleExpanded(),
		"keyboard toggle did not expand the assistant thinking block")

	// Second keyboard toggle against the re-collapsed item should expand
	// it again. Direct ToggleExpanded then returns false because it
	// re-collapses.
	u.chat.ToggleExpandedSelectedItem()
	require.False(t, exp.ToggleExpanded(),
		"second keyboard toggle did not re-expand the assistant thinking block")
}
