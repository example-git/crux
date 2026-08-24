package chat

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestCompactActivityHistoryKeepsLatestActivitiesAndOrdinaryMessages(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	ordinary := NewUserMessageItem(&sty, &message.Message{ID: "ordinary", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "keep me"}}}, nil)
	items := []MessageItem{ordinary}
	for i := range ActivityHistoryLimit + 3 {
		items = append(items, taskNotificationItem(t, &sty, i))
	}

	compacted := CompactActivityHistory(&sty, items, ActivityHistoryLimit)
	require.Len(t, compacted, ActivityHistoryLimit+2)
	require.Same(t, ordinary, compacted[0])
	stub, ok := compacted[1].(*activityHistoryStubItem)
	require.True(t, ok)
	require.Equal(t, 3, stub.count)
	require.Contains(t, ansi.Strip(stub.Render(80)), "3 older agent/task activities hidden")
	require.Equal(t, "notification-3:task-notification", compacted[2].ID())
	require.Equal(t, fmt.Sprintf("notification-%d:task-notification", ActivityHistoryLimit+2), compacted[len(compacted)-1].ID())
}

func TestCompactActivityHistoryAccumulatesHiddenCount(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	var items []MessageItem
	for i := range ActivityHistoryLimit + 1 {
		items = append(items, taskNotificationItem(t, &sty, i))
	}
	items = CompactActivityHistory(&sty, items, ActivityHistoryLimit)
	items = append(items, taskNotificationItem(t, &sty, ActivityHistoryLimit+1))
	items = CompactActivityHistory(&sty, items, ActivityHistoryLimit)

	stub, ok := items[0].(*activityHistoryStubItem)
	require.True(t, ok)
	require.Equal(t, 2, stub.count)
	activityCount := 0
	for _, item := range items {
		if IsAgentTaskActivity(item) {
			activityCount++
		}
	}
	require.Equal(t, ActivityHistoryLimit, activityCount)
}

func TestCompactActivityHistoryLeavesShortHistoryUnchanged(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	items := []MessageItem{taskNotificationItem(t, &sty, 0), taskNotificationItem(t, &sty, 1)}
	require.Equal(t, items, CompactActivityHistory(&sty, items, ActivityHistoryLimit))
}

func TestCompactActivityHistoryNeverPrunesUnfinishedActivity(t *testing.T) {
	t.Parallel()

	sty := styles.CharmtonePantera()
	unfinished := NewAgentToolMessageItem(&sty, message.ToolCall{ID: "unfinished", Name: "agent"}, nil, false)
	items := []MessageItem{unfinished}
	for i := range ActivityHistoryLimit + 2 {
		items = append(items, taskNotificationItem(t, &sty, i))
	}

	compacted := CompactActivityHistory(&sty, items, ActivityHistoryLimit)
	require.Contains(t, compacted, MessageItem(unfinished))
	require.False(t, unfinished.Finished())
	require.Len(t, compacted, ActivityHistoryLimit+2)
}

func taskNotificationItem(t *testing.T, sty *styles.Styles, index int) MessageItem {
	t.Helper()
	msg := &message.Message{
		ID:   fmt.Sprintf("notification-%d", index),
		Role: message.User,
		Parts: []message.ContentPart{message.TextContent{Text: fmt.Sprintf(
			"<task-notification><task-id>a%08d</task-id><task-type>agent</task-type><status>completed</status><summary>done</summary></task-notification>", index,
		)}},
	}
	item, ok := newTaskNotificationMessageItem(sty, msg)
	require.True(t, ok)
	return item
}
