package app

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type rollbackHistoryRecorder struct {
	history.Service
	sessionID  string
	messageIDs []string
	restore    bool
}

func (r *rollbackHistoryRecorder) RewindCheckpoints(_ context.Context, sessionID string, messageIDs []string, restore bool) error {
	r.sessionID = sessionID
	r.messageIDs = append([]string(nil), messageIDs...)
	r.restore = restore
	return nil
}

func TestRollbackSessionCommitsHistoryAndRemovesCheckpointsWithoutRestore(t *testing.T) {
	directory := t.TempDir()
	connection, err := db.Connect(t.Context(), directory)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Release(directory)) })
	queries := db.New(connection)
	sessions := session.NewService(queries, connection)
	messages := message.NewService(queries, message.WithCompactionStore(connection, sessions))
	current, err := sessions.Create(t.Context(), "rollback")
	require.NoError(t, err)
	firstUser, err := messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first"}}})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "first response"}}})
	require.NoError(t, err)
	secondUser, err := messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "second"}}})
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "second response"}}})
	require.NoError(t, err)
	historyRecorder := &rollbackHistoryRecorder{}
	application := &App{Messages: messages, History: historyRecorder}

	result, err := application.RollbackSession(t.Context(), current.ID, secondUser.ID)
	require.NoError(t, err)
	require.Len(t, result.Messages, 2)
	require.Equal(t, firstUser.ID, result.Messages[0].ID)
	require.Equal(t, int64(2), result.Session.MessageCount)
	require.Equal(t, current.ID, historyRecorder.sessionID)
	require.Equal(t, []string{secondUser.ID}, historyRecorder.messageIDs)
	require.False(t, historyRecorder.restore)

	stored, err := messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, []string{result.Messages[0].ID, result.Messages[1].ID}, []string{stored[0].ID, stored[1].ID})
}
