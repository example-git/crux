package history

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestLatestCheckpointFilesFollowTurnBoundaries(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	queries := db.New(conn)
	workspace := t.TempDir()
	files, err := NewServiceWithSnapshots(queries, conn, t.TempDir(), workspace)
	require.NoError(t, err)
	sessions := session.NewService(queries, conn)
	messages := message.NewService(queries)

	createdSession, err := sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = messages.Create(t.Context(), createdSession.ID, message.CreateMessageParams{Role: message.User})
	require.NoError(t, err)
	firstAssistant, err := messages.Create(t.Context(), createdSession.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)

	firstPath := filepath.Join(workspace, "first.txt")
	require.NoError(t, os.WriteFile(firstPath, []byte("before"), 0o644))
	require.NoError(t, files.Checkpoint(t.Context(), createdSession.ID, firstAssistant.ID, firstPath, "before", true, 0o644))
	_, err = files.Create(t.Context(), createdSession.ID, firstPath, "before")
	require.NoError(t, err)
	_, err = files.CreateVersion(t.Context(), createdSession.ID, firstPath, "after")
	require.NoError(t, err)

	latest, err := files.ListLatestCheckpointFiles(t.Context(), createdSession.ID)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, "before", latest[0].Content)
	require.True(t, latest[0].Exists)
	require.Equal(t, "after", latest[1].Content)
	require.True(t, latest[1].Exists)

	secondUser, err := messages.Create(t.Context(), createdSession.ID, message.CreateMessageParams{Role: message.User})
	require.NoError(t, err)
	secondAssistant, err := messages.Create(t.Context(), createdSession.ID, message.CreateMessageParams{Role: message.Assistant})
	require.NoError(t, err)
	latest, err = files.ListLatestCheckpointFiles(t.Context(), createdSession.ID)
	require.NoError(t, err)
	require.Empty(t, latest)

	secondPath := filepath.Join(workspace, "second.txt")
	require.NoError(t, files.Checkpoint(t.Context(), createdSession.ID, secondAssistant.ID, secondPath, "", false, 0))
	_, err = files.Create(t.Context(), createdSession.ID, secondPath, "")
	require.NoError(t, err)
	_, err = files.CreateVersion(t.Context(), createdSession.ID, secondPath, "created")
	require.NoError(t, err)
	require.NoError(t, files.Checkpoint(t.Context(), createdSession.ID, secondAssistant.ID, secondPath, "created", true, 0o644))
	_, err = files.CreateVersion(t.Context(), createdSession.ID, secondPath, "created again")
	require.NoError(t, err)
	latest, err = files.ListLatestCheckpointFiles(t.Context(), createdSession.ID)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, secondPath, latest[0].Path)
	require.Equal(t, "", latest[0].Content)
	require.False(t, latest[0].Exists)
	require.Equal(t, "created again", latest[1].Content)
	require.True(t, latest[1].Exists)

	require.NoError(t, files.RewindCheckpoints(t.Context(), createdSession.ID, []string{secondUser.ID}, false))
	require.NoError(t, messages.Delete(t.Context(), secondAssistant.ID))
	require.NoError(t, messages.Delete(t.Context(), secondUser.ID))
	latest, err = files.ListLatestCheckpointFiles(t.Context(), createdSession.ID)
	require.NoError(t, err)
	require.Len(t, latest, 2)
	require.Equal(t, firstPath, latest[0].Path)
	require.Equal(t, "before", latest[0].Content)
	require.Equal(t, "after", latest[1].Content)
}
