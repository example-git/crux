package model

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type historyWorkspace struct {
	workspace.Workspace
	messages []message.Message
}

func (historyWorkspace) Config() *config.Config {
	return &config.Config{}
}

func (historyWorkspace) PermissionSkipRequests() bool {
	return false
}

func (w historyWorkspace) ListUserMessages(context.Context, string) ([]message.Message, error) {
	return w.messages, nil
}

func (w historyWorkspace) ListAllUserMessages(context.Context) ([]message.Message, error) {
	return w.messages, nil
}

func TestPromptHistoryExcludesTaskNotifications(t *testing.T) {
	t.Parallel()

	notification := `<task-notification><task-id>a12345678</task-id><task-type>agent</task-type><status>completed</status><summary>done</summary></task-notification>`
	u := newTestUI()
	u.com.Workspace = historyWorkspace{messages: []message.Message{
		{ID: "ordinary", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "keep this prompt"}}},
		{ID: "notification", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: notification}}},
		{ID: "malformed", Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "<task-notification><status>completed</status>"}}},
	}}

	loaded, ok := u.loadPromptHistory()().(promptHistoryLoadedMsg)
	require.True(t, ok)
	require.Equal(t, []string{"keep this prompt", "<task-notification><status>completed</status>"}, loaded.messages)
}

func TestHistoryBangCommandStripsPrefixWhileAlreadyInBangMode(t *testing.T) {
	t.Parallel()

	u := newTestUI()
	u.com.Workspace = historyWorkspace{}
	u.promptHistory.messages = []string{"!echo one", "!echo two"}
	u.promptHistory.index = -1

	require.True(t, u.historyPrev())
	require.True(t, u.bangMode)
	require.Equal(t, "echo one", u.textarea.Value())

	require.True(t, u.historyPrev())
	require.True(t, u.bangMode)
	require.Equal(t, "echo two", u.textarea.Value())
}
