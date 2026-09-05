package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

type summaryCaptureModel struct {
	finishStreamModel
	mu     sync.Mutex
	calls  []fantasy.Call
	resets []string
}

func (m *summaryCaptureModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.mu.Unlock()
	return m.finishStreamModel.Stream(ctx, call)
}

func (m *summaryCaptureModel) ResetConversationChain(conversationID string) {
	m.mu.Lock()
	m.resets = append(m.resets, conversationID)
	m.mu.Unlock()
}

func (m *summaryCaptureModel) snapshot() ([]fantasy.Call, []string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Call(nil), m.calls...), append([]string(nil), m.resets...)
}

type blockingSummaryModel struct {
	finishStreamModel
	calls   atomic.Int64
	entered chan struct{}
	release chan struct{}
}

func (m *blockingSummaryModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Add(1) == 1 {
		close(m.entered)
		select {
		case <-m.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return (&finishStreamModel{text: "<analysis>draft</analysis><summary>newest context</summary>"}).Stream(ctx, call)
	}
	return (&finishStreamModel{text: "continued"}).Stream(ctx, call)
}

type failingSummaryModel struct {
	finishStreamModel
}

func (m *failingSummaryModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	return nil, errors.New("summary failed")
}

type autoCompactionModel struct {
	finishStreamModel
	calls atomic.Int64
}

func (m *autoCompactionModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	call := m.calls.Add(1)
	text := "regular response"
	usage := fantasy.Usage{InputTokens: 90, TotalTokens: 90}
	if call > 1 {
		text = "<analysis>draft</analysis><summary>automatic checkpoint</summary>"
		usage = fantasy.Usage{}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: usage})
	}, nil
}

type remoteCompactionModel struct {
	finishStreamModel
	streamCalls   atomic.Int64
	compactCalls  atomic.Int64
	mu            sync.Mutex
	streamPrompts []fantasy.Prompt
	compactCall   fantasy.Call
	compact       func(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error)
}

func (m *remoteCompactionModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	invocation := m.streamCalls.Add(1)
	m.mu.Lock()
	m.streamPrompts = append(m.streamPrompts, call.Prompt)
	m.mu.Unlock()
	text := "regular response"
	usage := fantasy.Usage{InputTokens: 90, TotalTokens: 90}
	if invocation > 1 {
		text = "<summary>local summary must not run</summary>"
		usage = fantasy.Usage{InputTokens: 10, TotalTokens: 10}
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "1"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "1", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "1"}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop, Usage: usage})
	}, nil
}

func (m *remoteCompactionModel) Compact(ctx context.Context, call fantasy.Call) (*codexresponses.CompactionResult, error) {
	m.compactCalls.Add(1)
	m.mu.Lock()
	m.compactCall = call
	m.mu.Unlock()
	if m.compact != nil {
		return m.compact(ctx, call)
	}
	return remoteCompactionResult("remote checkpoint")
}

func (m *remoteCompactionModel) snapshot() ([]fantasy.Prompt, fantasy.Call) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]fantasy.Prompt(nil), m.streamPrompts...), m.compactCall
}

func remoteCompactionResult(summary string) (*codexresponses.CompactionResult, error) {
	var history codexresponses.CompactedHistory
	if err := json.Unmarshal([]byte(`{"items":[{"type":"compaction","encrypted_content":"opaque-checkpoint"}]}`), &history); err != nil {
		return nil, err
	}
	return &codexresponses.CompactionResult{
		History:           &history,
		Summary:           summary,
		Usage:             fantasy.Usage{InputTokens: 70, OutputTokens: 5, TotalTokens: 75},
		UsageAvailable:    true,
		Implementation:    codexresponses.CompactionRemoteV2,
		ActiveInputTokens: 8,
	}, nil
}

type capturingRemoteCompactor struct {
	RemoteCompactor
	mu   sync.Mutex
	call fantasy.Call
}

func (c *capturingRemoteCompactor) Compact(ctx context.Context, call fantasy.Call) (*codexresponses.CompactionResult, error) {
	c.mu.Lock()
	c.call = call
	c.mu.Unlock()
	return c.RemoteCompactor.Compact(ctx, call)
}

func (c *capturingRemoteCompactor) snapshot() fantasy.Call {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.call
}

type rejectingCompactionMessageService struct {
	message.Service
	calls atomic.Int64
}

func (s *rejectingCompactionMessageService) CommitCompaction(context.Context, message.CommitCompactionParams) (message.Message, error) {
	s.calls.Add(1)
	return message.Message{}, errors.New("compaction commit rejected")
}

func remoteCompactionPolicy() *manifest.CompactionPolicy {
	return &manifest.CompactionPolicy{
		Mode:                "remote-operation",
		Operation:           "remote-compact",
		RetainedTokenBudget: 20,
		PreserveToolPairs:   true,
		MetadataNamespace:   codexresponses.Name,
	}
}

func remoteCompactionRetry() *manifest.RetryPolicy {
	return &manifest.RetryPolicy{
		MaxAttempts:       1,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}
}

func remoteCompactionContracts(schema map[string]any) []manifest.MetadataContract {
	if schema == nil {
		schema = map[string]any{
			"$schema":              "https://json-schema.org/draft/2020-12/schema",
			"type":                 "object",
			"additionalProperties": true,
		}
	}
	return []manifest.MetadataContract{{
		Namespace:         codexresponses.Name,
		Version:           1,
		Scope:             string(message.ProviderMetadataScopeCompaction),
		Schema:            schema,
		RequiredForReplay: true,
	}}
}

func configureRemoteCompaction(agent *sessionAgent, model *remoteCompactionModel) Model {
	configured := agent.largeModel.Get()
	configured.Compaction = remoteCompactionPolicy()
	configured.Compactor = model
	configured.CompactionRetry = remoteCompactionRetry()
	configured.Metadata = remoteCompactionContracts(nil)
	agent.largeModel.Set(configured)
	return configured
}

func nextSummaryMessageEvent(t *testing.T, events <-chan pubsub.Event[message.Message]) pubsub.Event[message.Message] {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if event.Payload.IsSummaryMessage {
				return event
			}
		case <-timer.C:
			t.Fatal("timed out waiting for summary message event")
		}
	}
}

func newSummaryTestAgent(env fakeEnv, model fantasy.LanguageModel) *sessionAgent {
	configured := Model{
		Model: model,
		CatalogModel: catalog.Model{
			ContextWindow:    200000,
			DefaultMaxTokens: 10000,
		},
		ModelCfg: config.SelectedModel{
			Model:    "gpt-test",
			Provider: codexresponses.Name,
		},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	}
	return NewSessionAgent(SessionAgentOptions{
		LargeModel:   configured,
		SmallModel:   configured,
		SystemPrompt: "system prompt",
		IsYolo:       true,
		Sessions:     env.sessions,
		Messages:     env.messages,
	}).(*sessionAgent)
}

func TestBuildSummaryPromptPrioritizesLatestUserDirection(t *testing.T) {
	prompt := buildSummaryPrompt([]session.Todo{{Status: session.TodoStatusInProgress, Content: "current task"}})

	require.Contains(t, prompt, "Treat the newest explicit user direction as authoritative")
	require.Contains(t, prompt, "All User Messages")
	require.Contains(t, prompt, "direct quotes from the newest messages")
	require.Contains(t, prompt, "- [in_progress] current task")
	require.NotContains(t, prompt, "## Current State")
}

func TestFormatCompactSummaryPersistsOnlySummaryBlock(t *testing.T) {
	formatted, err := formatCompactSummary("outside<analysis>discard this</analysis><summary>keep this</summary>outside")
	require.NoError(t, err)
	require.Contains(t, formatted, "Summary:\nkeep this")
	require.NotContains(t, formatted, "discard this")
	require.NotContains(t, formatted, "outside")

	_, err = formatCompactSummary("missing summary block")
	require.ErrorContains(t, err, "non-empty <summary> block")
}

func TestFormatCompactSummaryAcceptsSummarySplitAcrossTextParts(t *testing.T) {
	response := fantasy.ResponseContent{
		fantasy.TextContent{Text: "> <analysis>draft</analysis><summary>"},
		fantasy.TextContent{Text: "latest request wins</summary>"},
	}

	formatted, err := formatCompactSummary(allResponseText(response))
	require.NoError(t, err)
	require.Contains(t, formatted, "Summary:\nlatest request wins")
	require.NotContains(t, formatted, "draft")
}

func TestCompactionContinuationDoesNotReplayOriginalPrompt(t *testing.T) {
	runtime := InstalledRuntime{LargeModel: Model{ModelCfg: config.SelectedModel{Model: "captured-model"}}}
	continuation := compactionContinuationCall(SessionAgentCall{
		SessionID: "session",
		Prompt:    "old watchdog request",
		runtime:   &runtime,
	})

	require.Equal(t, "session", continuation.SessionID)
	require.NotContains(t, continuation.Prompt, "old watchdog request")
	require.Contains(t, continuation.Prompt, "newest user request")
	require.Same(t, &runtime, continuation.runtime)
}

func TestSummarizeUsesPromptedTextCheckpointAndClearsCodexChain(t *testing.T) {
	env := testEnv(t)
	model := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "<analysis>draft</analysis><summary>latest request wins</summary>"}}
	agent := newSummaryTestAgent(env, model)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "work on the latest request"}},
	})
	require.NoError(t, err)
	options := fantasy.ProviderOptions{
		codexresponses.Name: &codexresponses.ProviderOptions{ReasoningEffort: "high"},
	}

	require.NoError(t, agent.Summarize(t.Context(), current.ID, options, nil))
	calls, resets := model.snapshot()
	require.Len(t, calls, 1)
	require.Empty(t, calls[0].Tools)
	require.Equal(t, options, calls[0].ProviderOptions)
	require.Equal(t, sessionHeaders(current.ID, "summary"), calls[0].Headers)
	require.Contains(t, fantasySystemText(calls[0].Prompt), "system prompt")
	require.Equal(t, buildSummaryPrompt(nil), fantasyUserText(calls[0].Prompt[len(calls[0].Prompt)-1:]))
	require.Equal(t, []string{session.HashID(current.ID)}, resets)

	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.NotEmpty(t, storedSession.SummaryMessageID)
	require.Positive(t, storedSession.PromptTokens)
	require.Zero(t, storedSession.CompletionTokens)
	require.True(t, storedSession.EstimatedUsage)
	storedSummary, err := env.messages.Get(t.Context(), storedSession.SummaryMessageID)
	require.NoError(t, err)
	require.Contains(t, storedSummary.Content().Text, "Summary:\nlatest request wins")
	require.NotContains(t, storedSummary.Content().Text, "draft")
	require.Empty(t, storedSummary.Content().ProviderMetadata)
}

func TestSummarizeAtomicallyReplacesTriggeringRun(t *testing.T) {
	env := testEnv(t)
	model := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "<analysis>draft</analysis><summary>checkpoint</summary>"}}
	agent := newSummaryTestAgent(env, model)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "current task"}},
	})
	require.NoError(t, err)
	triggering := &activeCancel{cancel: func() {}}
	agent.activeRequests.Set(current.ID, triggering)

	err = agent.summarize(t.Context(), current.ID, nil, nil, fantasy.Instructions{}, &activeCancel{cancel: func() {}})
	require.ErrorIs(t, err, ErrSessionBusy)
	active, ok := agent.activeRequests.Get(current.ID)
	require.True(t, ok)
	require.Same(t, triggering, active)

	require.NoError(t, agent.summarize(t.Context(), current.ID, nil, nil, fantasy.Instructions{}, triggering))
	require.False(t, agent.IsSessionBusy(current.ID))
}

func TestPromptArrivingDuringSummaryRunsAfterCheckpoint(t *testing.T) {
	env := testEnv(t)
	model := &blockingSummaryModel{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	agent := newSummaryTestAgent(env, model)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "older task"}},
	})
	require.NoError(t, err)
	events := env.messages.Subscribe(t.Context())

	done := make(chan error, 1)
	go func() {
		done <- agent.Summarize(context.Background(), current.ID, nil, nil)
	}()
	select {
	case <-model.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("summary did not start")
	}

	createdEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.CreatedEvent, createdEvent.Type)
	require.False(t, createdEvent.Payload.IsFinished())
	require.Empty(t, createdEvent.Payload.Content().Text)
	placeholderID := createdEvent.Payload.ID
	storedWhileSummarizing, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, storedWhileSummarizing, 2)
	require.Equal(t, placeholderID, storedWhileSummarizing[1].ID)
	require.True(t, storedWhileSummarizing[1].IsSummaryMessage)
	require.False(t, storedWhileSummarizing[1].IsFinished())

	result, err := agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "newest user direction"})
	require.NoError(t, err)
	require.Nil(t, result)
	require.Equal(t, 1, agent.QueuedPrompts(current.ID))
	close(model.release)
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("summary did not finish")
	}

	updatedEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.UpdatedEvent, updatedEvent.Type)
	require.Equal(t, placeholderID, updatedEvent.Payload.ID)
	require.True(t, updatedEvent.Payload.IsFinished())
	require.Contains(t, updatedEvent.Payload.Content().Text, "Summary:\nnewest context")
	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, placeholderID, storedSession.SummaryMessageID)
	messages, err := agent.getSessionMessages(t.Context(), storedSession)
	require.NoError(t, err)
	require.Len(t, messages, 3)
	require.Equal(t, message.User, messages[0].Role)
	require.Contains(t, messages[0].Content().Text, "Summary:\nnewest context")
	require.Equal(t, message.User, messages[1].Role)
	require.Equal(t, "newest user direction", messages[1].Content().Text)
	require.Equal(t, message.Assistant, messages[2].Role)
	require.Equal(t, "continued", messages[2].Content().Text)
	require.Zero(t, agent.QueuedPrompts(current.ID))
}

func TestSummarizeRemovesPlaceholderAfterFailure(t *testing.T) {
	env := testEnv(t)
	model := &failingSummaryModel{}
	agent := newSummaryTestAgent(env, model)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	userMessage, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "keep this message"}},
	})
	require.NoError(t, err)
	events := env.messages.Subscribe(t.Context())

	err = agent.Summarize(t.Context(), current.ID, nil, nil)
	require.ErrorContains(t, err, "summary failed")
	createdEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.CreatedEvent, createdEvent.Type)
	require.False(t, createdEvent.Payload.IsFinished())
	deletedEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.DeletedEvent, deletedEvent.Type)
	require.Equal(t, createdEvent.Payload.ID, deletedEvent.Payload.ID)

	storedMessages, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, storedMessages, 1)
	require.Equal(t, userMessage.ID, storedMessages[0].ID)
	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Empty(t, storedSession.SummaryMessageID)
}

func TestAutomaticCompactionPublishesSummaryPlaceholderAndCompletesIt(t *testing.T) {
	env := testEnv(t)
	model := &autoCompactionModel{}
	agent := newSummaryTestAgent(env, model)
	configured := agent.largeModel.Get()
	configured.CatalogModel.ContextWindow = 100
	agent.largeModel.Set(configured)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "existing context"}},
	})
	require.NoError(t, err)
	events := env.messages.Subscribe(t.Context())

	_, err = agent.Run(t.Context(), SessionAgentCall{
		SessionID: current.ID,
		Prompt:    "trigger automatic compaction",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), model.calls.Load())

	createdEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.CreatedEvent, createdEvent.Type)
	require.False(t, createdEvent.Payload.IsFinished())
	require.Empty(t, createdEvent.Payload.Content().Text)
	updatedEvent := nextSummaryMessageEvent(t, events)
	require.Equal(t, pubsub.UpdatedEvent, updatedEvent.Type)
	require.Equal(t, createdEvent.Payload.ID, updatedEvent.Payload.ID)
	require.True(t, updatedEvent.Payload.IsFinished())
	require.Contains(t, updatedEvent.Payload.Content().Text, "Summary:\nautomatic checkpoint")

	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, createdEvent.Payload.ID, storedSession.SummaryMessageID)
}

func TestAutomaticRemoteCompactionCommitsAndReplaysProviderCheckpoint(t *testing.T) {
	env := testEnv(t)
	model := &remoteCompactionModel{}
	agent := newSummaryTestAgent(env, model)
	configured := agent.largeModel.Get()
	configured.CatalogModel.ContextWindow = 100
	configured.Compaction = &manifest.CompactionPolicy{
		Mode:                "remote-operation",
		Operation:           "remote-compact",
		RetainedTokenBudget: 20,
		PreserveToolPairs:   true,
		MetadataNamespace:   codexresponses.Name,
	}
	configured.Compactor = model
	configured.CompactionRetry = &manifest.RetryPolicy{
		MaxAttempts:       1,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}
	configured.Metadata = []manifest.MetadataContract{{
		Namespace: codexresponses.Name,
		Version:   1,
		Scope:     string(message.ProviderMetadataScopeCompaction),
		Schema: map[string]any{
			"$schema":              "https://json-schema.org/draft/2020-12/schema",
			"type":                 "object",
			"additionalProperties": true,
		},
		RequiredForReplay: true,
	}}
	agent.largeModel.Set(configured)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "existing context"}},
	})
	require.NoError(t, err)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		SessionID: current.ID,
		Prompt:    "trigger remote compaction",
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), model.streamCalls.Load())
	require.Equal(t, int64(1), model.compactCalls.Load())

	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, int64(8), storedSession.PromptTokens)
	require.Zero(t, storedSession.CompletionTokens)
	require.False(t, storedSession.EstimatedUsage)
	storedCheckpoint, err := env.messages.Get(t.Context(), storedSession.SummaryMessageID)
	require.NoError(t, err)
	require.Equal(t, "remote checkpoint", storedCheckpoint.Content().Text)
	require.Len(t, storedCheckpoint.Content().ProviderMetadata, 1)
	require.Equal(t, codexresponses.Name, storedCheckpoint.Content().ProviderMetadata[0].Namespace)
	require.Equal(t, 1, storedCheckpoint.Content().ProviderMetadata[0].Version)
	require.Equal(t, message.ProviderMetadataScopeCompaction, storedCheckpoint.Content().ProviderMetadata[0].Scope)

	_, err = agent.Run(t.Context(), SessionAgentCall{
		SessionID: current.ID,
		Prompt:    "continue after checkpoint",
	})
	require.NoError(t, err)
	require.Equal(t, int64(2), model.streamCalls.Load())
	require.Equal(t, int64(1), model.compactCalls.Load())
	prompts, compactCall := model.snapshot()
	require.Len(t, prompts, 2)
	require.Equal(t, sessionHeaders(current.ID, "compaction"), compactCall.Headers)
	var history *codexresponses.CompactedHistory
	for _, promptMessage := range prompts[1] {
		for _, part := range promptMessage.Content {
			textPart, ok := part.(fantasy.TextPart)
			if !ok || textPart.ProviderOptions == nil {
				continue
			}
			candidate, ok := textPart.ProviderOptions[codexresponses.Name].(*codexresponses.CompactedHistory)
			if ok {
				history = candidate
			}
		}
	}
	require.NotNil(t, history)
	historyJSON, err := json.Marshal(history)
	require.NoError(t, err)
	require.Contains(t, string(historyJSON), "opaque-checkpoint")
	userText := fantasyUserText(prompts[1])
	require.Contains(t, userText, "continue after checkpoint")
	require.NotContains(t, userText, "existing context")
}

func TestRemoteCompactionRejectsUnavailableDeclarationsWithoutLocalFallback(t *testing.T) {
	for _, test := range []struct {
		name      string
		configure func(*Model, *remoteCompactionModel)
		want      string
	}{
		{
			name: "disabled",
			configure: func(configured *Model, _ *remoteCompactionModel) {
				configured.Compaction = &manifest.CompactionPolicy{Mode: "none"}
			},
			want: "disabled",
		},
		{
			name: "missing executor",
			configure: func(configured *Model, _ *remoteCompactionModel) {
				configured.Compaction = remoteCompactionPolicy()
				configured.CompactionRetry = remoteCompactionRetry()
				configured.Metadata = remoteCompactionContracts(nil)
			},
			want: "executor is unavailable",
		},
		{
			name: "missing retry",
			configure: func(configured *Model, model *remoteCompactionModel) {
				configured.Compaction = remoteCompactionPolicy()
				configured.Compactor = model
				configured.Metadata = remoteCompactionContracts(nil)
			},
			want: "retry policy is unavailable",
		},
		{
			name: "missing metadata",
			configure: func(configured *Model, model *remoteCompactionModel) {
				configured.Compaction = remoteCompactionPolicy()
				configured.Compactor = model
				configured.CompactionRetry = remoteCompactionRetry()
			},
			want: "has no compaction contract",
		},
		{
			name: "unsupported mode",
			configure: func(configured *Model, _ *remoteCompactionModel) {
				configured.Compaction = &manifest.CompactionPolicy{Mode: "unsupported"}
			},
			want: "unsupported",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			model := &remoteCompactionModel{}
			agent := newSummaryTestAgent(env, model)
			configured := agent.largeModel.Get()
			test.configure(&configured, model)
			agent.largeModel.Set(configured)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			userMessage, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "preserve this history"}},
			})
			require.NoError(t, err)

			err = agent.Summarize(t.Context(), current.ID, nil, nil)
			require.ErrorContains(t, err, test.want)
			require.Zero(t, model.compactCalls.Load())
			require.Zero(t, model.streamCalls.Load())
			storedMessages, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			require.Len(t, storedMessages, 1)
			require.Equal(t, userMessage.ID, storedMessages[0].ID)
			storedSession, err := env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			require.Empty(t, storedSession.SummaryMessageID)
		})
	}
}

func TestRemoteCompactionInvalidCheckpointPreservesSessionAndHistory(t *testing.T) {
	env := testEnv(t)
	model := &remoteCompactionModel{}
	agent := newSummaryTestAgent(env, model)
	configured := configureRemoteCompaction(agent, model)
	configured.Metadata = remoteCompactionContracts(map[string]any{
		"$schema": "https://json-schema.org/draft/2020-12/schema",
		"type":    "object",
		"properties": map[string]any{
			"items": map[string]any{"type": "array", "maxItems": 0},
		},
		"required": []any{"items"},
	})
	agent.largeModel.Set(configured)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	current.PromptTokens = 41
	current.CompletionTokens = 7
	current.Cost = 1.25
	current.EstimatedUsage = true
	current, err = env.sessions.Save(t.Context(), current)
	require.NoError(t, err)
	userMessage, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "preserve invalid checkpoint history"}},
	})
	require.NoError(t, err)

	err = agent.Summarize(t.Context(), current.ID, nil, nil)
	require.ErrorContains(t, err, "schema validation failed")
	require.Equal(t, int64(1), model.compactCalls.Load())
	require.Zero(t, model.streamCalls.Load())
	storedMessages, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, storedMessages, 1)
	require.Equal(t, userMessage.ID, storedMessages[0].ID)
	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Empty(t, storedSession.SummaryMessageID)
	require.Equal(t, current.PromptTokens, storedSession.PromptTokens)
	require.Equal(t, current.CompletionTokens, storedSession.CompletionTokens)
	require.Equal(t, current.Cost, storedSession.Cost)
	require.Equal(t, current.EstimatedUsage, storedSession.EstimatedUsage)
}

func TestRemoteCompactionCommitFailurePreservesPriorCheckpointAndHistory(t *testing.T) {
	env := testEnv(t)
	underlyingMessages := env.messages
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	priorPlaceholder, err := underlyingMessages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:             message.Assistant,
		Model:            "gpt-test",
		Provider:         codexresponses.Name,
		IsSummaryMessage: true,
	})
	require.NoError(t, err)
	_, err = underlyingMessages.CommitCompaction(t.Context(), message.CommitCompactionParams{
		MessageID: priorPlaceholder.ID,
		SessionID: current.ID,
		Parts: []message.ContentPart{
			message.TextContent{Text: "prior checkpoint"},
			message.Finish{Reason: message.FinishReasonEndTurn, Time: time.Now().Unix()},
		},
		PromptTokens:     33,
		CompletionTokens: 4,
		Cost:             2.5,
		EstimatedUsage:   true,
	})
	require.NoError(t, err)
	newUserMessage, err := underlyingMessages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "history after prior checkpoint"}},
	})
	require.NoError(t, err)

	rejectingMessages := &rejectingCompactionMessageService{Service: underlyingMessages}
	env.messages = rejectingMessages
	model := &remoteCompactionModel{}
	agent := newSummaryTestAgent(env, model)
	configureRemoteCompaction(agent, model)

	err = agent.Summarize(t.Context(), current.ID, nil, nil)
	require.ErrorContains(t, err, "compaction commit rejected")
	require.Equal(t, int64(1), rejectingMessages.calls.Load())
	require.Equal(t, int64(1), model.compactCalls.Load())
	require.Zero(t, model.streamCalls.Load())
	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, priorPlaceholder.ID, storedSession.SummaryMessageID)
	require.Equal(t, int64(33), storedSession.PromptTokens)
	require.Equal(t, int64(4), storedSession.CompletionTokens)
	require.Equal(t, 2.5, storedSession.Cost)
	require.True(t, storedSession.EstimatedUsage)
	storedMessages, err := underlyingMessages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, storedMessages, 2)
	require.Equal(t, priorPlaceholder.ID, storedMessages[0].ID)
	require.Equal(t, "prior checkpoint", storedMessages[0].Content().Text)
	require.Equal(t, newUserMessage.ID, storedMessages[1].ID)
	require.Equal(t, "history after prior checkpoint", storedMessages[1].Content().Text)
}

func TestRemoteCompactionAuthenticationRefreshUsesExactRefreshedCompactor(t *testing.T) {
	env := testEnv(t)
	initial := &remoteCompactionModel{compact: func(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error) {
		return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "expired"}
	}}
	refreshed := &remoteCompactionModel{compact: func(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error) {
		return remoteCompactionResult("refreshed checkpoint")
	}}
	agent := newSummaryTestAgent(env, initial)
	configured := configureRemoteCompaction(agent, initial)
	configured.CompactionRetry.Authentication = "refresh-once"
	configured.CatalogModel.CostPer1MIn = 1_000_000
	configured.CatalogModel.CostPer1MOut = 2_000_000
	agent.largeModel.Set(configured)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
		Role:  message.User,
		Parts: []message.ContentPart{message.TextContent{Text: "refresh remote compaction"}},
	})
	require.NoError(t, err)
	refreshCalls := 0

	err = agent.Summarize(t.Context(), current.ID, nil, func(context.Context, *fantasy.ProviderError) error {
		refreshCalls++
		refreshedConfiguration := configured
		refreshedConfiguration.Model = refreshed
		refreshedConfiguration.Compactor = refreshed
		agent.largeModel.Set(refreshedConfiguration)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, refreshCalls)
	require.Equal(t, int64(1), initial.compactCalls.Load())
	require.Equal(t, int64(1), refreshed.compactCalls.Load())
	require.Zero(t, initial.streamCalls.Load())
	require.Zero(t, refreshed.streamCalls.Load())
	storedSession, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, int64(8), storedSession.PromptTokens)
	require.Zero(t, storedSession.CompletionTokens)
	require.Equal(t, 80.0, storedSession.Cost)
	require.False(t, storedSession.EstimatedUsage)
	storedCheckpoint, err := env.messages.Get(t.Context(), storedSession.SummaryMessageID)
	require.NoError(t, err)
	require.Equal(t, "refreshed checkpoint", storedCheckpoint.Content().Text)
	require.Len(t, storedCheckpoint.Content().ProviderMetadata, 1)
}

func TestRemoteCompactionAuthenticationRefreshRejectsDriftBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*Model)
	}{
		{name: "provider", edit: func(model *Model) { model.ModelCfg.Provider = "replacement-provider" }},
		{name: "model", edit: func(model *Model) { model.ModelCfg.Model = "replacement-model" }},
		{name: "operation", edit: func(model *Model) {
			policy := *model.Compaction
			policy.Operation = "replacement-operation"
			model.Compaction = &policy
		}},
		{name: "retry", edit: func(model *Model) {
			policy := cloneRetryPolicy(*model.CompactionRetry)
			policy.MaxAttempts++
			model.CompactionRetry = &policy
		}},
		{name: "metadata version", edit: func(model *Model) {
			model.Metadata = append([]manifest.MetadataContract(nil), model.Metadata...)
			model.Metadata[0].Version++
		}},
		{name: "metadata schema", edit: func(model *Model) {
			model.Metadata[0].Schema["required"] = []any{"replacement"}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			initial := &remoteCompactionModel{compact: func(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error) {
				return nil, &fantasy.ProviderError{StatusCode: http.StatusUnauthorized, Message: "expired"}
			}}
			refreshed := &remoteCompactionModel{}
			agent := newSummaryTestAgent(env, initial)
			configured := configureRemoteCompaction(agent, initial)
			configured.CompactionRetry.Authentication = "refresh-once"
			agent.largeModel.Set(configured)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			userMessage, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "reject refreshed drift"}},
			})
			require.NoError(t, err)

			err = agent.Summarize(t.Context(), current.ID, nil, func(context.Context, *fantasy.ProviderError) error {
				refreshedConfiguration := agent.largeModel.Get()
				refreshedConfiguration.Model = refreshed
				refreshedConfiguration.Compactor = refreshed
				test.edit(&refreshedConfiguration)
				agent.largeModel.Set(refreshedConfiguration)
				return nil
			})
			require.Error(t, err)
			require.Equal(t, int64(1), initial.compactCalls.Load())
			require.Zero(t, refreshed.compactCalls.Load())
			require.Zero(t, initial.streamCalls.Load())
			require.Zero(t, refreshed.streamCalls.Load())
			storedMessages, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			require.Len(t, storedMessages, 1)
			require.Equal(t, userMessage.ID, storedMessages[0].ID)
			storedSession, err := env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			require.Empty(t, storedSession.SummaryMessageID)
		})
	}
}

func TestRemoteCompactionFinalizesTransportStateOnlyAfterCheckpointCommit(t *testing.T) {
	for _, test := range []struct {
		name         string
		rejectCommit bool
		wantPrevious string
	}{
		{name: "commit failure retains chain", rejectCommit: true, wantPrevious: "compaction-response"},
		{name: "successful commit clears chain"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan map[string]any, 2)
			serverErrors := make(chan error, 1)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				connection, err := upgrader.Upgrade(w, request, nil)
				if err != nil {
					serverErrors <- err
					return
				}
				defer connection.Close()
				for invocation := 0; invocation < 2; invocation++ {
					var frame map[string]any
					if err := connection.ReadJSON(&frame); err != nil {
						serverErrors <- err
						return
					}
					requests <- frame
					if err := connection.WriteJSON(map[string]any{
						"type": "response.output_item.done",
						"item": map[string]any{"type": "compaction", "encrypted_content": "opaque-checkpoint"},
					}); err != nil {
						serverErrors <- err
						return
					}
					responseID := "probe-response"
					if invocation == 0 {
						responseID = "compaction-response"
					}
					if err := connection.WriteJSON(map[string]any{
						"type": "response.completed",
						"response": map[string]any{
							"id":    responseID,
							"usage": map[string]any{"input_tokens": 12, "output_tokens": 1},
						},
					}); err != nil {
						serverErrors <- err
						return
					}
				}
			}))
			defer server.Close()

			store := codexresponses.NewSessionStore()
			defer store.Close()
			retry := manifest.RetryPolicy{
				MaxAttempts:       1,
				Authentication:    "never",
				ReplayRequirement: "before-first-event",
			}
			provider, err := codexresponses.New(
				codexresponses.WithURL(strings.Replace(server.URL, "http://", "ws://", 1)),
				codexresponses.WithName(codexresponses.Name),
				codexresponses.WithTokenSource(func() string { return "synthetic-token" }),
				codexresponses.WithAccountIDSource(func() string { return "synthetic-account" }),
				codexresponses.WithSessionStore(store),
				codexresponses.WithOwnerValidator(func() error { return nil }),
				codexresponses.WithRetryPolicy(retry),
				codexresponses.WithCompactionPolicy(0, 0, 0, 1<<20, retry),
			)
			require.NoError(t, err)
			languageModel, err := provider.LanguageModel(t.Context(), "synthetic-model")
			require.NoError(t, err)
			compactor, ok := languageModel.(RemoteCompactor)
			require.True(t, ok)
			capturedCompactor := &capturingRemoteCompactor{RemoteCompactor: compactor}

			env := testEnv(t)
			underlyingMessages := env.messages
			if test.rejectCommit {
				env.messages = &rejectingCompactionMessageService{Service: underlyingMessages}
			}
			agent := newSummaryTestAgent(env, languageModel)
			configured := agent.largeModel.Get()
			configured.Model = languageModel
			configured.Compaction = remoteCompactionPolicy()
			configured.Compactor = capturedCompactor
			configured.CompactionRetry = remoteCompactionRetry()
			configured.Metadata = remoteCompactionContracts(nil)
			agent.largeModel.Set(configured)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			_, err = underlyingMessages.Create(t.Context(), current.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "compact real transport state"}},
			})
			require.NoError(t, err)

			err = agent.Summarize(t.Context(), current.ID, nil, nil)
			if test.rejectCommit {
				require.ErrorContains(t, err, "compaction commit rejected")
			} else {
				require.NoError(t, err)
			}
			probeResult, err := compactor.Compact(t.Context(), capturedCompactor.snapshot())
			require.NoError(t, err)
			probeResult.Finalize()
			firstRequest := <-requests
			secondRequest := <-requests
			firstPrevious, _ := firstRequest["previous_response_id"].(string)
			secondPrevious, _ := secondRequest["previous_response_id"].(string)
			require.Empty(t, firstPrevious)
			require.Equal(t, test.wantPrevious, secondPrevious)
			if test.rejectCommit {
				secondInput, ok := secondRequest["input"].([]any)
				require.True(t, ok)
				require.Len(t, secondInput, 1)
				trigger, ok := secondInput[0].(map[string]any)
				require.True(t, ok)
				require.Equal(t, "compaction_trigger", trigger["type"])
			}
			select {
			case err := <-serverErrors:
				require.NoError(t, err)
			default:
			}
		})
	}
}
