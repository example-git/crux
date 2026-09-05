package dialog

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type rewindWorkspace struct {
	workspace.Workspace
	messages []message.Message
}

func (w rewindWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return w.messages, nil
}

func TestRewindKeepsTaskNotificationWithReadableTitle(t *testing.T) {
	t.Parallel()

	notification := `<task-notification><task-id>a12345678</task-id><task-type>agent</task-type><status>completed</status><summary>done</summary></task-notification>`
	sty := styles.CharmtonePantera()
	com := &common.Common{
		Workspace: rewindWorkspace{messages: []message.Message{
			{ID: "ordinary", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "ordinary prompt"}}},
			{ID: "notification", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: notification}}},
		}},
		Styles: &sty,
	}
	rewind, err := NewRewind(com, "session")
	require.NoError(t, err)

	items := rewind.list.FilteredItems()
	require.Len(t, items, 2)
	taskItem, ok := items[0].(*RewindItem)
	require.True(t, ok)
	require.Equal(t, "notification", taskItem.ID())
	require.Equal(t, "Background Agent · completed", taskItem.title)
	ordinaryItem, ok := items[1].(*RewindItem)
	require.True(t, ok)
	require.Equal(t, "ordinary prompt", ordinaryItem.title)

	require.Nil(t, rewind.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	action, ok := rewind.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionRewind)
	require.True(t, ok)
	require.Equal(t, "notification", action.MessageID)
	require.Equal(t, notification, action.Text)
}
