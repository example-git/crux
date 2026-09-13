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
	"github.com/example-git/crux/foundation/providers/anthropic"
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
		usage = fantasy.Usage{InputTokens: 10, TotalTokens: 10}
	}
	if call.Headers["x-request-purpose"] == "summary" {
		text = "<summary>readable remote checkpoint</summary>"
		usage = fantasy.Usage{InputTokens: 10, OutputTokens: 2, TotalTokens: 12}
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
	agent.codexCompactionV2 = true
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
	require.NotNil(t, calls[0].MaxOutputTokens)
	require.EqualValues(t, 10000, *calls[0].MaxOutputTokens)
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

func TestSummarizeImagePayloadsPreserveStoredHistory(t *testing.T) {
	for _, policy := range []fantasy.InstructionPolicy{fantasy.InstructionPolicyAnthropic, fantasy.InstructionPolicyCodex} {
		t.Run(string(policy), func(t *testing.T) {
			env := testEnv(t)
			model := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "<summary>checkpoint</summary>"}}
			agent := newSummaryTestAgent(env, model)
			runtime := agent.Runtime()
			runtime.LargeModel.InstructionPolicy = policy
			runtime.LargeModel.CatalogModel.SupportsImages = true
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			var originals []message.Message
			for _, params := range []message.CreateMessageParams{
				{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "summarize this work"}, message.BinaryContent{MIMEType: "image/png", Data: []byte("synthetic-upload")}}},
				{Role: message.Assistant, Parts: []message.ContentPart{message.ToolCall{ID: "image-call", Name: "view", Input: `{}`, Finished: true}}},
				{Role: message.Tool, Parts: []message.ContentPart{message.ToolResult{ToolCallID: "image-call", Name: "view", MIMEType: "image/png", Data: "c3ludGhldGljLXRvb2w=", Content: "image description"}}},
			} {
				stored, err := env.messages.Create(t.Context(), current.ID, params)
				require.NoError(t, err)
				originals = append(originals, stored)
			}
			require.NoError(t, agent.SummarizeWithRuntime(t.Context(), current.ID, nil, nil, runtime))
			calls, _ := model.snapshot()
			require.Len(t, calls, 1)
			images := 0
			for _, msg := range calls[0].Prompt {
				for _, part := range msg.Content {
					if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && strings.HasPrefix(file.MediaType, "image/") {
						images++
					}
					if tool, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
						if _, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](tool.Output); ok {
							images++
						}
					}
				}
			}
			if policy == fantasy.InstructionPolicyCodex {
				require.Equal(t, 2, images)
			} else {
				require.Zero(t, images)
			}
			for _, original := range originals {
				stored, err := env.messages.Get(t.Context(), original.ID)
				require.NoError(t, err)
				require.Equal(t, original.Parts, stored.Parts)
			}
		})
	}
}

func TestSummarizeAnthropicOutputBudgetAndCheckpoint(t *testing.T) {
	for _, test := range []struct {
		name       string
		catalogMax int64
		configured int64
		wantMax    int64
		stopReason string
		text       string
		wantError  string
	}{
		{name: "catalog default", catalogMax: 64000, wantMax: 64000, stopReason: "end_turn", text: "<summary>checkpoint</summary>"},
		{name: "configured override", catalogMax: 64000, configured: 12000, wantMax: 12000, stopReason: "end_turn", text: "<summary>checkpoint</summary>"},
		{name: "unspecified budget", wantMax: 4096, stopReason: "end_turn", text: "<summary>checkpoint</summary>"},
		{name: "truncated", catalogMax: 64000, wantMax: 64000, stopReason: "max_tokens", text: "<summary>unfinished", wantError: "truncated by a token limit"},
		{name: "malformed", catalogMax: 64000, wantMax: 64000, stopReason: "end_turn", text: "<summary>unfinished", wantError: "non-empty <summary> block"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan map[string]any, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests <- body
				w.Header().Set("Content-Type", "text/event-stream")
				text, _ := json.Marshal(test.text)
				events := []string{
					`{"type":"message_start","message":{"id":"msg_summary","type":"message","role":"assistant","model":"claude-test","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}`,
					`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
					`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + string(text) + `}}`,
					`{"type":"content_block_stop","index":0}`,
					`{"type":"message_delta","delta":{"stop_reason":"` + test.stopReason + `","stop_sequence":null},"usage":{"output_tokens":20}}`,
					`{"type":"message_stop"}`,
				}
				for _, event := range events {
					var envelope struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal([]byte(event), &envelope); err != nil {
						t.Error(err)
						return
					}
					_, _ = w.Write([]byte("event: " + envelope.Type + "\ndata: " + event + "\n\n"))
				}
			}))
			defer server.Close()
			provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("synthetic"))
			require.NoError(t, err)
			model, err := provider.LanguageModel(t.Context(), "claude-test")
			require.NoError(t, err)
			env := testEnv(t)
			agent := newSummaryTestAgent(env, model)
			runtime := agent.Runtime()
			runtime.LargeModel.CatalogModel.SupportsImages = true
			runtime.LargeModel.CatalogModel.DefaultMaxTokens = test.catalogMax
			runtime.LargeModel.ModelCfg.MaxTokens = test.configured
			runtime.LargeModel.InstructionPolicy = fantasy.InstructionPolicyAnthropic
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			original, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "preserve the current task"}, message.BinaryContent{MIMEType: "image/png", Data: []byte("synthetic-summary-image")}},
			})
			require.NoError(t, err)
			err = agent.SummarizeWithRuntime(t.Context(), current.ID, nil, nil, runtime)
			if test.wantError == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, test.wantError)
				if test.stopReason == "end_turn" {
					require.NotContains(t, err.Error(), "truncated")
				}
			}
			select {
			case body := <-requests:
				require.EqualValues(t, test.wantMax, body["max_tokens"])
				require.Empty(t, body["tools"])
				encoded, err := json.Marshal(body)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), `"type":"image"`)
				require.NotContains(t, string(encoded), "c3ludGhldGljLXN1bW1hcnktaW1hZ2U=")
			default:
				t.Fatal("summary did not reach the Anthropic HTTP transport")
			}
			retained, err := env.messages.Get(t.Context(), original.ID)
			require.NoError(t, err)
			require.Equal(t, original.Parts, retained.Parts)
			stored, err := env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			if test.wantError == "" {
				require.NotEmpty(t, stored.SummaryMessageID)
				checkpoint, err := env.messages.Get(t.Context(), stored.SummaryMessageID)
				require.NoError(t, err)
				require.Contains(t, checkpoint.Content().Text, "Summary:\ncheckpoint")
			} else {
				require.Empty(t, stored.SummaryMessageID)
				messages, err := env.messages.List(t.Context(), current.ID)
				require.NoError(t, err)
				require.Len(t, messages, 1)
				require.Equal(t, original.ID, messages[0].ID)
				require.Equal(t, original.Content().Text, messages[0].Content().Text)
			}
		})
	}
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
	active, ok = agent.activeRequests.Get(current.ID)
	require.True(t, ok)
	require.Same(t, triggering, active)
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

func TestAnthropicRunPreservesMessageProviderMetadata(t *testing.T) {
	env := testEnv(t)
	model := &summaryCaptureModel{}
	agent := newSummaryTestAgent(env, model)
	runtime := agent.Runtime()
	runtime.LargeModel.AnthropicEfficiency = &anthropic.EfficiencyPolicy{PromptCaching: true}
	agent.SetRuntime(runtime)
	current, err := env.sessions.Create(t.Context(), "message metadata")
	require.NoError(t, err)
	_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "prior user"}}})
	require.NoError(t, err)
	previous, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "prior answer"}}})
	require.NoError(t, err)
	metadata, err := message.MetadataFromFantasyMetadata(message.ProviderMetadataScopeMessage, fantasy.ProviderMetadata{anthropic.Name: &anthropic.ProviderOptions{ExtraBody: map[string]any{"preserved": "value"}}})
	require.NoError(t, err)
	previous.SetMessageProviderMetadata(metadata)
	require.NoError(t, env.messages.Update(t.Context(), previous))
	require.NoError(t, env.messages.FlushAll(t.Context()))
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "next user"})
	require.NoError(t, err)
	calls, _ := model.snapshot()
	require.Len(t, calls, 1)
	found := false
	for _, projected := range calls[0].Prompt {
		if projected.Role == fantasy.MessageRoleAssistant {
			options, ok := projected.ProviderOptions[anthropic.Name].(*anthropic.ProviderOptions)
			require.True(t, ok)
			require.Equal(t, "value", options.ExtraBody["preserved"])
			found = true
		}
	}
	require.True(t, found)
}

func TestAnthropicAgentRollingCacheHTTP(t *testing.T) {
	var requests []map[string]any
	var requestMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requestMu.Lock()
		requests = append(requests, body)
		requestMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"answer\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()
	provider, err := anthropic.New(anthropic.WithBaseURL(server.URL), anthropic.WithAPIKey("test"))
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "claude-test")
	require.NoError(t, err)
	env := testEnv(t)
	agent := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "stable system")
	runtime := agent.Runtime()
	runtime.LargeModel.AnthropicEfficiency = &anthropic.EfficiencyPolicy{PromptCaching: true}
	runtime.LargeModel.InstructionPolicy = fantasy.InstructionPolicyAnthropic
	agent.SetRuntime(runtime)
	current, err := env.sessions.Create(t.Context(), "cache test")
	require.NoError(t, err)
	for _, prompt := range []string{"first turn", "second turn"} {
		_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: prompt})
		require.NoError(t, err)
	}
	agent = testSessionAgent(env, model, &finishStreamModel{text: "title"}, "stable system")
	agent.SetRuntime(runtime)
	_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "resumed turn"})
	require.NoError(t, err)
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, "first turn", stored[0].Content().Text)
	require.Equal(t, todoReminderText, stored[0].Content().Context.TodoReminder)
	requestMu.Lock()
	defer requestMu.Unlock()
	require.Len(t, requests, 3)
	require.Equal(t, requests[1]["system"], requests[2]["system"])
	require.Equal(t, requests[0]["system"], requests[1]["system"])
	for _, request := range requests {
		require.NoError(t, anthropic.ValidateCacheMarkers(request))
		messages := request["messages"].([]any)
		for index, raw := range messages {
			blocks := raw.(map[string]any)["content"].([]any)
			for blockIndex, rawBlock := range blocks {
				_, marked := rawBlock.(map[string]any)["cache_control"]
				require.Equal(t, index == len(messages)-1 && blockIndex == len(blocks)-1, marked)
			}
		}
	}
	first := requests[0]["messages"].([]any)[0].(map[string]any)
	second := requests[1]["messages"].([]any)[0].(map[string]any)
	for _, raw := range first["content"].([]any) {
		delete(raw.(map[string]any), "cache_control")
	}
	require.Equal(t, first, second)
	require.Equal(t, second, requests[2]["messages"].([]any)[0])
	for _, raw := range requests[2]["messages"].([]any)[len(requests[2]["messages"].([]any))-1].(map[string]any)["content"].([]any) {
		require.NotEqual(t, todoReminderText, raw.(map[string]any)["text"])
	}
}

func TestAnthropicUserTurnContextTransitions(t *testing.T) {
	first := anthropicUserTurnContext(nil, true, false)
	require.Equal(t, todoReminderText, first.TodoReminder)
	history := []message.Message{{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "first", Context: first}}}}
	require.Empty(t, anthropicUserTurnContext(history, true, false).TodoReminder)
	populated := anthropicUserTurnContext(history, false, false)
	require.Equal(t, "populated", populated.TodoState)
	require.Empty(t, populated.TodoReminder)
	history = append(history, message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "next", Context: populated}}})
	require.Equal(t, todoReminderText, anthropicUserTurnContext(history, true, false).TodoReminder)
	require.Empty(t, anthropicUserTurnContext(nil, true, true).TodoReminder)
}

func TestAnthropicRuntimePolicyIsolation(t *testing.T) {
	env := testEnv(t)
	agent := newSummaryTestAgent(env, &finishStreamModel{})
	runtime := agent.Runtime()
	runtime.LargeModel.AnthropicEfficiency = &anthropic.EfficiencyPolicy{PromptCaching: true, TTL: "1h"}
	runtime.SmallModel.AnthropicEfficiency = &anthropic.EfficiencyPolicy{PromptCaching: true, TTL: "5m"}
	agent.SetRuntime(runtime)
	runtime.LargeModel.AnthropicEfficiency.TTL = "5m"
	runtime.SmallModel.AnthropicEfficiency.PromptCaching = false
	snapshot := agent.Runtime()
	require.Equal(t, "1h", snapshot.LargeModel.AnthropicEfficiency.TTL)
	require.True(t, snapshot.SmallModel.AnthropicEfficiency.PromptCaching)
	snapshot.LargeModel.AnthropicEfficiency.TTL = "5m"
	snapshot.SmallModel.AnthropicEfficiency.PromptCaching = false
	require.Equal(t, "1h", agent.Runtime().LargeModel.AnthropicEfficiency.TTL)
	require.True(t, agent.Runtime().SmallModel.AnthropicEfficiency.PromptCaching)
}

func TestAnthropicAutomaticCompactionUsesReferenceReserve(t *testing.T) {
	for _, test := range []struct {
		name     string
		cap      int64
		disabled bool
		budget   int64
		calls    int64
	}{
		{name: "threshold", cap: 21090, calls: 2},
		{name: "below threshold", cap: 21091, calls: 1},
		{name: "disabled", cap: 21090, disabled: true, calls: 1},
		{name: "explicit budget", cap: 21090, budget: 10, calls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			model := &autoCompactionModel{}
			agent := newSummaryTestAgent(env, model)
			runtime := agent.Runtime()
			runtime.LargeModel.AnthropicEfficiency = &anthropic.EfficiencyPolicy{PromptCaching: true}
			runtime.LargeModel.CatalogModel.ContextWindow = 200000
			runtime.LargeModel.ModelCfg.MaxTokens = 8000
			if test.budget > 0 {
				runtime.LargeModel.Compaction = &manifest.CompactionPolicy{Mode: "local-summary", RetainedTokenBudget: test.budget}
			}
			runtime.SummarizationContextCap = test.cap
			runtime.DisableAutoSummarize = test.disabled
			runtime.SmallModel.Model = &finishStreamModel{text: "title"}
			agent.SetRuntime(runtime)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "check reserve"})
			require.NoError(t, err)
			require.Equal(t, test.calls, model.calls.Load())
		})
	}
}

func TestAnthropicCompactionReserve(t *testing.T) {
	model := Model{CatalogModel: catalog.Model{DefaultMaxTokens: 32000}}
	require.EqualValues(t, 33000, anthropicCompactionReserve(model, 0))
	model.ModelCfg.MaxTokens = 8000
	require.EqualValues(t, 21000, anthropicCompactionReserve(model, 0))
	require.EqualValues(t, 17000, anthropicCompactionReserve(model, 4000))
	require.EqualValues(t, 33000, anthropicCompactionReserve(model, 64000))
	triggered, reserve := shouldAutoCompact(200000, 179000, anthropicCompactionReserve(model, 0), false)
	require.True(t, triggered)
	require.EqualValues(t, 21000, reserve)
	triggered, _ = shouldAutoCompact(200000, 178999, reserve, false)
	require.False(t, triggered)
	triggered, _ = shouldAutoCompact(200000, 179000, reserve, true)
	require.False(t, triggered)
}

func TestSummarizationContextCapControlsAutomaticCompaction(t *testing.T) {
	for _, test := range []struct {
		cap      int64
		disabled bool
		calls    int64
	}{{0, false, 1}, {100, false, 2}, {100, true, 1}, {2000000, false, 1}} {
		env := testEnv(t)
		model := &autoCompactionModel{}
		agent := newSummaryTestAgent(env, model)
		runtime := agent.Runtime()
		runtime.LargeModel.CatalogModel.ContextWindow = 1000000
		runtime.SummarizationContextCap = test.cap
		runtime.SmallModel.Model = &finishStreamModel{text: "title"}
		runtime.DisableAutoSummarize = test.disabled
		agent.SetRuntime(runtime)
		current, err := env.sessions.Create(t.Context(), "session")
		require.NoError(t, err)
		_, err = agent.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "test the configured cap"})
		require.NoError(t, err)
		require.Equal(t, test.calls, model.calls.Load())
	}
	require.EqualValues(t, 258000, effectiveSummarizationWindow(1000000, 258000))
	require.EqualValues(t, 128000, effectiveSummarizationWindow(128000, 258000))
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
	agent.codexCompactionV2 = true
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
	require.True(t, storedSession.EstimatedUsage)
	storedCheckpoint, err := env.messages.Get(t.Context(), storedSession.SummaryMessageID)
	require.NoError(t, err)
	require.Equal(t, "remote checkpoint", storedCheckpoint.Content().Text)
	require.True(t, storedCheckpoint.IsSummaryMessage)
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
		v2        bool
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
		{
			name: "explicit v2 unavailable",
			configure: func(configured *Model, _ *remoteCompactionModel) {
				configured.Compaction = &manifest.CompactionPolicy{Mode: "local-summary"}
			},
			want: "experimental Codex compaction v2 is unavailable",
			v2:   true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			model := &remoteCompactionModel{}
			agent := newSummaryTestAgent(env, model)
			configured := agent.largeModel.Get()
			test.configure(&configured, model)
			agent.largeModel.Set(configured)
			agent.codexCompactionV2 = test.v2 || configured.Compaction.Mode == "remote-operation"
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

func TestRemoteCompactionFailurePreservesHistoryWithoutReadableFallback(t *testing.T) {
	for _, test := range []struct {
		name   string
		cancel bool
	}{
		{name: "provider error"},
		{name: "cancelled", cancel: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			env := testEnv(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			compactor := &remoteCompactionModel{compact: func(context.Context, fantasy.Call) (*codexresponses.CompactionResult, error) {
				if test.cancel {
					cancel()
					return nil, context.Canceled
				}
				return nil, errors.New("compaction failed")
			}}
			agent := newSummaryTestAgent(env, compactor)
			configureRemoteCompaction(agent, compactor)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			original, err := env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{
				Role:  message.User,
				Parts: []message.ContentPart{message.TextContent{Text: "preserve source history"}},
			})
			require.NoError(t, err)
			events := env.messages.Subscribe(t.Context())
			err = agent.Summarize(ctx, current.ID, nil, nil)
			require.Error(t, err)
			require.EqualValues(t, 1, compactor.compactCalls.Load())
			require.Zero(t, compactor.streamCalls.Load())
			created := nextSummaryMessageEvent(t, events)
			deleted := nextSummaryMessageEvent(t, events)
			require.Equal(t, pubsub.CreatedEvent, created.Type)
			require.Equal(t, pubsub.DeletedEvent, deleted.Type)
			require.Equal(t, created.Payload.ID, deleted.Payload.ID)
			stored, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, original.ID, stored[0].ID)
			current, err = env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			require.Empty(t, current.SummaryMessageID)
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
	require.True(t, storedSession.EstimatedUsage)
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

func TestAutomaticCodexCompactionKeepsRepeatedBoundariesWithLargeToolResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	var turns, compactions atomic.Int64
	requests := make(chan map[string]any, 16)
	serverErrors := make(chan error, 16)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		connection, err := upgrader.Upgrade(w, request, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer connection.Close()
		for {
			var frame map[string]any
			if err := connection.ReadJSON(&frame); err != nil {
				return
			}
			requests <- frame
			input, _ := frame["input"].([]any)
			compact := false
			for _, value := range input {
				item, _ := value.(map[string]any)
				compact = compact || item["type"] == "compaction_trigger"
			}
			var events []map[string]any
			usage := int64(136000)
			responseID := "summary-response"
			if compact {
				checkpoint := "checkpoint-one"
				if compactions.Add(1) > 1 {
					checkpoint = "checkpoint-two"
				}
				responseID = checkpoint
				events = append(events, map[string]any{
					"type": "response.output_item.done",
					"item": map[string]any{"type": "compaction", "encrypted_content": checkpoint},
				})
			} else if tools, _ := frame["tools"].([]any); len(tools) == 0 {
				events = append(events, map[string]any{"type": "response.output_text.delta", "item_id": "summary", "delta": "<summary>readable checkpoint</summary>"})
			} else {
				switch turns.Add(1) {
				case 1:
					responseID = "first-boundary"
					events = append(events, map[string]any{
						"type": "response.output_item.done",
						"item": map[string]any{"type": "function_call", "id": "first-boundary-item", "call_id": "first-boundary-call", "name": "checkpoint_boundary", "arguments": "{}"},
					})
				case 2:
					responseID = "large-tool-response"
					usage = 100000
					events = append(events, map[string]any{
						"type": "response.output_item.done",
						"item": map[string]any{"type": "function_call", "id": "tool-item", "call_id": "large-result", "name": "large_result", "arguments": "{}"},
					})
				case 3:
					responseID = "after-large-result"
					usage = 115000
				case 4:
					responseID = "second-boundary"
					events = append(events, map[string]any{
						"type": "response.output_item.done",
						"item": map[string]any{"type": "function_call", "id": "second-boundary-item", "call_id": "second-boundary-call", "name": "checkpoint_boundary", "arguments": "{}"},
					})
				default:
					responseID = "after-second-checkpoint"
					usage = 100000
				}
			}
			events = append(events, map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":    responseID,
					"usage": map[string]any{"input_tokens": usage, "output_tokens": 0, "input_tokens_details": map[string]any{"cached_tokens": usage / 2}},
				},
			})
			for _, event := range events {
				if err := connection.WriteJSON(event); err != nil {
					serverErrors <- err
					return
				}
			}
		}
	}))
	defer server.Close()
	store := codexresponses.NewSessionStore()
	defer store.Close()
	retry := *remoteCompactionRetry()
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
	languageModel, err := provider.LanguageModel(ctx, "gpt-6-astra")
	require.NoError(t, err)
	env := testEnv(t)
	agent := newSummaryTestAgent(env, languageModel)
	rawOutput := strings.Repeat("large tool output\n", 30000)
	runtime := agent.Runtime()
	runtime.SmallModel.Model = &finishStreamModel{text: "title"}
	runtime.LargeModel.ModelCfg.Model = "gpt-6-astra"
	runtime.LargeModel.CatalogModel.ContextWindow = 1000000
	runtime.LargeModel.Compaction = remoteCompactionPolicy()
	runtime.LargeModel.Compaction.RetainedTokenBudget = 64000
	runtime.LargeModel.Compactor = languageModel.(RemoteCompactor)
	runtime.LargeModel.CompactionRetry = remoteCompactionRetry()
	runtime.LargeModel.Metadata = remoteCompactionContracts(nil)
	runtime.SummarizationContextCap = 200000
	runtime.SummarizationFastMode = true
	runtime.CodexCompactionV2 = true
	runtime.LargeModel.InstructionPolicy = fantasy.InstructionPolicyCodex
	runtime.Tools = []fantasy.AgentTool{
		fantasy.NewAgentTool("large_result", "Return a large result", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse(rawOutput), nil
		}),
		fantasy.NewAgentTool("checkpoint_boundary", "Reach a checkpoint boundary", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return fantasy.NewTextResponse("done"), nil
		}),
	}
	agent.SetRuntime(runtime)
	current, err := env.sessions.Create(ctx, "session")
	require.NoError(t, err)
	sessionEvents := env.sessions.Subscribe(ctx)
	_, err = agent.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "before first checkpoint"})
	require.NoError(t, err)
	require.EqualValues(t, 1, compactions.Load())
	first, err := env.sessions.Get(ctx, current.ID)
	require.NoError(t, err)
	require.NotEmpty(t, first.SummaryMessageID)
	require.EqualValues(t, 3, turns.Load(), "automatic continuation must execute the tool round after the checkpoint")
	require.EqualValues(t, 115000, first.PromptTokens)
	require.Zero(t, first.CompletionTokens)

	_, err = agent.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "reach the next boundary"})
	require.NoError(t, err)
	require.EqualValues(t, 2, compactions.Load())
	second, err := env.sessions.Get(ctx, current.ID)
	require.NoError(t, err)
	require.NotEqual(t, first.SummaryMessageID, second.SummaryMessageID)
	require.EqualValues(t, 5, turns.Load(), "the second checkpoint must also continue automatically")
	require.EqualValues(t, 100000, second.PromptTokens)
	_, err = agent.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "continue after second checkpoint"})
	require.NoError(t, err)
	require.EqualValues(t, 2, compactions.Load())

	var sawToolOccupancy, sawProviderReplacement, sawCheckpointEstimate bool
	boundaryUpdates := 0
	for len(sessionEvents) > 0 {
		updated := (<-sessionEvents).Payload
		if updated.ID != current.ID {
			continue
		}
		if updated.PromptTokens == 100000 && updated.UnseenLocalTokens > 0 {
			sawToolOccupancy = true
			require.Greater(t, updated.ContextTokens(), int64(100000))
			require.Less(t, updated.ContextTokens(), int64(136000))
			require.True(t, updated.ContextEstimated())
		}
		if updated.PromptTokens == 115000 {
			sawProviderReplacement = true
			require.Zero(t, updated.UnseenLocalTokens)
			require.EqualValues(t, 115000, updated.ContextTokens())
			require.False(t, updated.ContextEstimated())
		}
		if updated.PromptTokens == 136000 && updated.UnseenLocalTokens > 0 {
			boundaryUpdates++
			triggered, _ := shouldAutoCompact(runtime.SummarizationContextCap, updated.ContextTokens(), runtime.LargeModel.Compaction.RetainedTokenBudget, false)
			require.True(t, triggered)
		}
		if updated.SummaryMessageID != "" && updated.PromptTokens < 100 {
			sawCheckpointEstimate = true
			require.Zero(t, updated.UnseenLocalTokens)
			require.True(t, updated.ContextEstimated())
		}
	}
	require.True(t, sawToolOccupancy)
	require.True(t, sawProviderReplacement)
	require.True(t, sawCheckpointEstimate)
	require.Equal(t, 2, boundaryUpdates)

	var sawBoundedOutput, sawSecondCheckpoint bool
	for len(requests) > 0 {
		frame := <-requests
		input, _ := frame["input"].([]any)
		compact := false
		for _, value := range input {
			item, _ := value.(map[string]any)
			compact = compact || item["type"] == "compaction_trigger"
		}
		tools, _ := frame["tools"].([]any)
		if compact || len(tools) == 0 {
			require.Equal(t, "priority", frame["service_tier"])
		} else {
			require.NotContains(t, frame, "service_tier")
		}
		for _, value := range input {
			item, _ := value.(map[string]any)
			if item["type"] == "function_call_output" && item["call_id"] == "large-result" {
				require.Equal(t, codexresponses.TruncateToolOutput("gpt-6-astra", rawOutput), item["output"])
				sawBoundedOutput = true
			}
			if item["encrypted_content"] == "checkpoint-two" {
				sawSecondCheckpoint = true
				require.Empty(t, frame["previous_response_id"])
				encoded, err := json.Marshal(input)
				require.NoError(t, err)
				require.NotContains(t, string(encoded), "checkpoint-one")
				require.NotContains(t, string(encoded), "before first checkpoint")
			}
		}
	}
	require.True(t, sawBoundedOutput)
	require.True(t, sawSecondCheckpoint)
	select {
	case err := <-serverErrors:
		require.NoError(t, err)
	default:
	}
}

func TestCodexSummarizationUsesOneRequest(t *testing.T) {
	for _, test := range []struct {
		name     string
		v2       bool
		fastMode bool
	}{
		{name: "default readable"},
		{name: "priority readable", fastMode: true},
		{name: "experimental v2", v2: true},
		{name: "priority experimental v2", v2: true, fastMode: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var requestCount atomic.Int64
			requests := make(chan map[string]any, 4)
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				connection, err := upgrader.Upgrade(w, request, nil)
				if err != nil {
					return
				}
				defer connection.Close()
				for {
					var frame map[string]any
					if err := connection.ReadJSON(&frame); err != nil {
						return
					}
					requestCount.Add(1)
					requests <- frame
					input, _ := json.Marshal(frame["input"])
					event := map[string]any{"type": "response.output_text.delta", "item_id": "summary", "delta": "<summary>Preserve the newest unfinished request</summary>"}
					if strings.Contains(string(input), "compaction_trigger") {
						event = map[string]any{"type": "response.output_item.done", "item": map[string]any{"type": "compaction", "encrypted_content": "opaque-checkpoint"}}
					}
					if err := connection.WriteJSON(event); err != nil {
						return
					}
					if err := connection.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "summary-response", "usage": map[string]any{"input_tokens": 12, "output_tokens": 5}}}); err != nil {
						return
					}
				}
			}))
			defer server.Close()
			store := codexresponses.NewSessionStore()
			defer store.Close()
			retry := *remoteCompactionRetry()
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
			model, err := provider.LanguageModel(ctx, "synthetic-model")
			require.NoError(t, err)
			env := testEnv(t)
			agent := newSummaryTestAgent(env, model)
			runtime := agent.Runtime()
			runtime.LargeModel.InstructionPolicy = fantasy.InstructionPolicyCodex
			runtime.LargeModel.Compaction = remoteCompactionPolicy()
			runtime.LargeModel.Compactor = model.(RemoteCompactor)
			runtime.LargeModel.CompactionRetry = remoteCompactionRetry()
			runtime.LargeModel.Metadata = remoteCompactionContracts(nil)
			runtime.CodexCompactionV2 = test.v2
			runtime.SummarizationFastMode = test.fastMode
			agent.SetRuntime(runtime)
			current, err := env.sessions.Create(ctx, "session")
			require.NoError(t, err)
			_, err = env.messages.Create(ctx, current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "newest unfinished request"}}})
			require.NoError(t, err)
			require.NoError(t, agent.Summarize(ctx, current.ID, nil, nil))
			require.EqualValues(t, 1, requestCount.Load())
			frame := <-requests
			input, err := json.Marshal(frame["input"])
			require.NoError(t, err)
			require.Contains(t, string(input), "newest unfinished request")
			require.Equal(t, test.v2, strings.Contains(string(input), "compaction_trigger"))
			require.Empty(t, frame["tools"])
			if test.fastMode {
				require.Equal(t, "priority", frame["service_tier"])
			} else {
				require.NotContains(t, frame, "service_tier")
			}
			storedSession, err := env.sessions.Get(ctx, current.ID)
			require.NoError(t, err)
			checkpoint, err := env.messages.Get(ctx, storedSession.SummaryMessageID)
			require.NoError(t, err)
			require.True(t, checkpoint.IsFinished())
			if test.v2 {
				require.Equal(t, "Conversation compacted by Codex", checkpoint.Content().Text)
				require.Len(t, checkpoint.Content().ProviderMetadata, 1)
			} else {
				require.Contains(t, checkpoint.Content().Text, "Summary:\nPreserve the newest unfinished request")
				require.Empty(t, checkpoint.Content().ProviderMetadata)
			}
		})
	}
}

func TestRemoteCompactionFinalizesTransportStateOnlyAfterCheckpointCommit(t *testing.T) {
	for _, test := range []struct {
		name         string
		rejectCommit bool
		fastMode     bool
		wantPrevious string
	}{
		{name: "commit failure retains chain", rejectCommit: true, wantPrevious: "compaction-response"},
		{name: "successful commit clears chain"},
		{name: "priority commit clears chain", fastMode: true},
		{name: "priority failure retains chain", fastMode: true, rejectCommit: true, wantPrevious: "compaction-response"},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan map[string]any, 2)
			summaryRequests := make(chan map[string]any, 1)
			serverErrors := make(chan error, 3)
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
					compactionRequest := false
					if input, ok := frame["input"].([]any); ok {
						for _, value := range input {
							if item, ok := value.(map[string]any); ok && item["type"] == "compaction_trigger" {
								compactionRequest = true
							}
						}
					}
					if !compactionRequest {
						summaryRequests <- frame
						for _, event := range []map[string]any{
							{"type": "response.output_text.delta", "item_id": "summary-text", "delta": "<summary>Readable checkpoint from transport</summary>"},
							{"type": "response.completed", "response": map[string]any{"id": "display-summary-response", "usage": map[string]any{"input_tokens": 20, "output_tokens": 5}}},
						} {
							if err := connection.WriteJSON(event); err != nil {
								serverErrors <- err
								return
							}
						}
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
			configured.InstructionPolicy = fantasy.InstructionPolicyCodex
			configured.CompactionRetry = remoteCompactionRetry()
			configured.Metadata = remoteCompactionContracts(nil)
			agent.largeModel.Set(configured)
			runtime := agent.Runtime()
			runtime.SummarizationFastMode = test.fastMode
			runtime.CodexCompactionV2 = true
			agent.SetRuntime(runtime)
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
			require.Empty(t, summaryRequests)
			require.Len(t, requests, 1)
			if !test.rejectCommit {
				storedSession, err := env.sessions.Get(t.Context(), current.ID)
				require.NoError(t, err)
				checkpoint, err := underlyingMessages.Get(t.Context(), storedSession.SummaryMessageID)
				require.NoError(t, err)
				require.True(t, checkpoint.IsSummaryMessage)
				require.Equal(t, "Conversation compacted by Codex", checkpoint.Content().Text)
				require.Len(t, checkpoint.Content().ProviderMetadata, 1)
			}
			probeResult, err := compactor.Compact(t.Context(), capturedCompactor.snapshot())
			require.NoError(t, err)
			probeResult.Finalize()
			firstRequest := <-requests
			secondRequest := <-requests
			for _, frame := range []map[string]any{firstRequest, secondRequest} {
				if test.fastMode {
					require.Equal(t, "priority", frame["service_tier"])
				} else {
					require.NotContains(t, frame, "service_tier")
				}
			}
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
