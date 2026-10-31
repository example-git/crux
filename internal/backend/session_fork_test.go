package backend

import (
	"context"
	"testing"
	"time"

	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/pubsub"
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
	source.Model = "source-model"
	source.Provider = "source-provider"
	source.PromptTokens = 120
	source.CompletionTokens = 30
	source.UnseenLocalTokens = 25
	source.EstimatedUsage = true
	source, err = workspace.Sessions.Save(t.Context(), source)
	require.NoError(t, err)
	original, err := workspace.Messages.Create(t.Context(), source.ID, message.CreateMessageParams{
		ID: "source-user", Role: message.User, SubmissionID: "desktop-submission", RunID: "source-run", ItemID: "source-user",
		Parts: []message.ContentPart{message.TextContent{Text: "hello"}},
	})
	require.NoError(t, err)
	firstAssistant, err := workspace.Messages.Create(t.Context(), source.ID, message.CreateMessageParams{
		ID: "source-assistant-1", Role: message.Assistant, RunID: "source-run", ItemID: "source-assistant-item",
		Parts: []message.ContentPart{message.TextContent{Text: "first"}}, PreserveParts: true,
	})
	require.NoError(t, err)
	secondAssistant, err := workspace.Messages.Create(t.Context(), source.ID, message.CreateMessageParams{
		ID: "source-assistant-2", Role: message.Assistant, RunID: "source-run", ItemID: "source-assistant-item",
		Parts: []message.ContentPart{message.TextContent{Text: "second"}}, PreserveParts: true,
	})
	require.NoError(t, err)

	events := workspace.Sessions.Subscribe(t.Context())
	forked, err := backend.ForkSession(t.Context(), workspace.ID, source.ID)
	require.NoError(t, err)
	event := <-events
	require.Equal(t, pubsub.CreatedEvent, event.Type)
	require.Equal(t, forked, event.Payload)
	visibleMessages, err := workspace.Messages.List(t.Context(), event.Payload.ID)
	require.NoError(t, err)
	require.Len(t, visibleMessages, 3)
	require.NotEqual(t, source.ID, forked.ID)
	require.Empty(t, forked.ParentSessionID)
	require.Equal(t, source.Model, forked.Model)
	require.Equal(t, source.Provider, forked.Provider)
	require.Equal(t, source.PromptTokens, forked.PromptTokens)
	require.Equal(t, source.CompletionTokens, forked.CompletionTokens)
	require.True(t, forked.EstimatedUsage)
	persistedFork, err := workspace.Sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.True(t, persistedFork.EstimatedUsage)
	require.Equal(t, source.Model, persistedFork.Model)
	require.Equal(t, source.Provider, persistedFork.Provider)
	require.Equal(t, source.UnseenLocalTokens, persistedFork.UnseenLocalTokens)
	require.Equal(t, source.ContextTokens(), persistedFork.ContextTokens())
	cloned, err := workspace.Messages.List(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Len(t, cloned, 3)
	require.NotEqual(t, original.ID, cloned[0].ID)
	require.Equal(t, cloned[0].ID, cloned[0].ItemID)
	require.Equal(t, original.SubmissionID, cloned[0].SubmissionID)
	require.Equal(t, original.Content().String(), cloned[0].Content().String())
	require.Equal(t, original.Parts, cloned[0].Parts)
	require.NotEqual(t, firstAssistant.ID, cloned[1].ID)
	require.NotEqual(t, secondAssistant.ID, cloned[2].ID)
	require.NotEmpty(t, cloned[1].ItemID)
	require.NotEqual(t, firstAssistant.ItemID, cloned[1].ItemID)
	require.Equal(t, cloned[1].ItemID, cloned[2].ItemID)
	for _, message := range cloned {
		require.Empty(t, message.RunID)
	}

	require.NoError(t, workspace.Messages.Delete(t.Context(), cloned[0].ID))
	sourceMessages, err := workspace.Messages.List(t.Context(), source.ID)
	require.NoError(t, err)
	require.Len(t, sourceMessages, 3)

	lineageFork, err := backend.ForkSessionWithOrigin(t.Context(), workspace.ID, source.ID)
	require.NoError(t, err)
	require.Empty(t, lineageFork.ParentSessionID)
	require.Equal(t, source.ID, lineageFork.ForkedFromSessionID)
	persistedLineageFork, err := workspace.Sessions.Get(t.Context(), lineageFork.ID)
	require.NoError(t, err)
	require.Empty(t, persistedLineageFork.ParentSessionID)
	require.Equal(t, source.ID, persistedLineageFork.ForkedFromSessionID)
	require.Equal(t, source.Model, persistedLineageFork.Model)
	require.Equal(t, source.Provider, persistedLineageFork.Provider)

	nestedFork, err := backend.ForkSessionWithOrigin(t.Context(), workspace.ID, lineageFork.ID)
	require.NoError(t, err)
	require.Empty(t, nestedFork.ParentSessionID)
	require.Equal(t, lineageFork.ID, nestedFork.ForkedFromSessionID)
	require.Equal(t, source.Model, nestedFork.Model)
	require.Equal(t, source.Provider, nestedFork.Provider)
}
