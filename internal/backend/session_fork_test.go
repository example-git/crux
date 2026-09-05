package backend

import (
	"context"
	"testing"
	"time"

	"github.com/example-git/crux/internal/message"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestForkSessionClonesPersistedHistoryIndependently(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	backend := New(context.Background(), nil, func() {})
	backend.SetCreateGrace(2 * time.Second)
	t.Cleanup(func() { drainBackend(t, backend) })
	workspace, _, err := backend.CreateWorkspace(protoWS(t.TempDir(), t.TempDir(), uuid.NewString()))
	require.NoError(t, err)
	source, err := workspace.Sessions.Create(t.Context(), "source")
	require.NoError(t, err)
	source.PromptTokens = 120
	source.CompletionTokens = 30
	source.EstimatedUsage = true
	source, err = workspace.Sessions.Save(t.Context(), source)
	require.NoError(t, err)
	original, err := workspace.Messages.Create(t.Context(), source.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)

	forked, err := backend.ForkSession(t.Context(), workspace.ID, source.ID)
	require.NoError(t, err)
	require.NotEqual(t, source.ID, forked.ID)
	require.Empty(t, forked.ParentSessionID)
	require.Equal(t, source.PromptTokens, forked.PromptTokens)
	require.Equal(t, source.CompletionTokens, forked.CompletionTokens)
	require.True(t, forked.EstimatedUsage)
	persistedFork, err := workspace.Sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.True(t, persistedFork.EstimatedUsage)
	cloned, err := workspace.Messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, cloned, 1)
	require.NotEqual(t, original.ID, cloned[0].ID)
	require.Equal(t, original.Content().String(), cloned[0].Content().String())
	require.Equal(t, original.Parts, cloned[0].Parts)

	require.NoError(t, workspace.Messages.Delete(t.Context(), cloned[0].ID))
	sourceMessages, err := workspace.Messages.List(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, sourceMessages, 1)
}
