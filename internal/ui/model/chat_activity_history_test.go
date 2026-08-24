package model

import (
	"fmt"
	"testing"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/chat"
	"github.com/stretchr/testify/require"
)

func TestChatKeepsLatestAgentTaskActivitiesOnLoadAndAppend(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	items := make([]chat.MessageItem, 0, chat.ActivityHistoryLimit+1)
	for i := range chat.ActivityHistoryLimit + 1 {
		items = append(items, modelTaskNotificationItem(t, ui, i))
	}

	ui.chat.SetMessages(items...)
	require.Equal(t, chat.ActivityHistoryLimit+1, ui.chat.Len())
	require.Nil(t, ui.chat.MessageItem("notification-0:task-notification"))
	require.NotNil(t, ui.chat.MessageItem("notification-1:task-notification"))

	ui.chat.AppendMessages(modelTaskNotificationItem(t, ui, chat.ActivityHistoryLimit+1))
	require.Equal(t, chat.ActivityHistoryLimit+1, ui.chat.Len())
	require.Nil(t, ui.chat.MessageItem("notification-1:task-notification"))
	require.NotNil(t, ui.chat.MessageItem(fmt.Sprintf("notification-%d:task-notification", chat.ActivityHistoryLimit+1)))
}

func modelTaskNotificationItem(t *testing.T, ui *UI, index int) chat.MessageItem {
	t.Helper()
	msg := &message.Message{
		ID:   fmt.Sprintf("notification-%d", index),
		Role: message.User,
		Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf(
			"<task-notification><task-id>a%08d</task-id><task-type>agent</task-type><status>completed</status><summary>done</summary></task-notification>", index,
		)}},
	}
	items := chat.ExtractMessageItems(ui.com.Styles, msg, nil, "")
	require.Len(t, items, 1)
	return items[0]
}
