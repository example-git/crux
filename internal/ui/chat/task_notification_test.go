package chat

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestTaskNotificationRendersAsCompactBackgroundAgentAction(t *testing.T) {
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:    "notification-message",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: `<task-notification><task-id>a12345678</task-id><task-type>agent</task-type><tool-use-id>call-1</tool-use-id><output-file>session:child</output-file><status>completed</status><summary>Background agent completed</summary><result>Review passed.</result><usage><PromptTokens>21</PromptTokens><CompletionTokens>34</CompletionTokens></usage></task-notification>`}},
	}

	items := ExtractMessageItems(&sty, msg, nil, "")
	require.Len(t, items, 1)
	require.IsType(t, &taskNotificationMessageItem{}, items[0])
	view := ansi.Strip(items[0].Render(100))
	require.Contains(t, view, "Background Agent")
	require.Contains(t, view, "a12345678")
	require.Contains(t, view, "completed")
	require.Contains(t, view, "Review passed.")
	require.NotContains(t, view, "task-notification")
	require.NotContains(t, view, "session:child")
}

func TestMalformedTaskNotificationFallsBackToUserMessage(t *testing.T) {
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:    "malformed-notification",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: `<task-notification><status>completed</status>`}},
	}

	items := ExtractMessageItems(&sty, msg, nil, "")
	require.Len(t, items, 1)
	require.IsType(t, &UserMessageItem{}, items[0])
}

func TestOrdinaryUserMessageDoesNotBecomeTaskNotification(t *testing.T) {
	sty := styles.CharmtonePantera()
	msg := &message.Message{
		ID:    "user-message",
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "Please inspect the task notification."}},
	}
	items := ExtractMessageItems(&sty, msg, nil, "")
	require.Len(t, items, 1)
	require.IsType(t, &UserMessageItem{}, items[0])
}
