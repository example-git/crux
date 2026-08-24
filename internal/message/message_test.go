package message

import (
	"bytes"
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestAllProviderMetadataScopesSurviveDatabaseRestart(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "all metadata scopes")
	require.NoError(t, err)

	payload := []byte(`{ "precise" : -0.00e-0 }`)
	envelope := func(scope ProviderMetadataScope) ProviderMetadataEnvelope {
		value, envelopeErr := NewProviderMetadataEnvelope("missing.plugin", 23, scope, payload)
		require.NoError(t, envelopeErr)
		return value
	}
	parts := []ContentPart{
		TextContent{Text: "before", ProviderMetadata: ProviderMetadata{envelope(ProviderMetadataScopeText), envelope(ProviderMetadataScopeCompaction)}},
		ReasoningContent{Thinking: "reasoning", ProviderMetadata: ProviderMetadata{envelope(ProviderMetadataScopeReasoning)}},
		ToolCall{ID: "call", Name: "tool", Input: "{}", Finished: true, ProviderExecuted: true, ProviderMetadata: ProviderMetadata{envelope(ProviderMetadataScopeToolCall)}},
		ToolResult{ToolCallID: "call", Name: "tool", Content: "result", ProviderExecuted: true, ProviderMetadata: ProviderMetadata{envelope(ProviderMetadataScopeToolResult)}},
		ProviderMetadataContent{ProviderMetadata: ProviderMetadata{envelope(ProviderMetadataScopeMessage), envelope(ProviderMetadataScopeContinuation)}},
	}

	svc := NewService(q)
	created, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant, Parts: parts, Model: "future-model", Provider: "missing.plugin"})
	require.NoError(t, err)
	created.AppendContent(" after")
	require.NoError(t, svc.Update(t.Context(), created))
	require.NoError(t, db.Release(dir))

	conn, err = db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dir) })
	loaded, err := NewService(db.New(conn)).Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "missing.plugin", loaded.Provider)
	require.Equal(t, "future-model", loaded.Model)

	var metadata ProviderMetadata
	for _, part := range loaded.Parts {
		switch value := part.(type) {
		case TextContent:
			require.Equal(t, "before after", value.Text)
			metadata = append(metadata, value.ProviderMetadata...)
		case ReasoningContent:
			metadata = append(metadata, value.ProviderMetadata...)
		case ToolCall:
			require.True(t, value.ProviderExecuted)
			metadata = append(metadata, value.ProviderMetadata...)
		case ToolResult:
			require.True(t, value.ProviderExecuted)
			metadata = append(metadata, value.ProviderMetadata...)
		case ProviderMetadataContent:
			metadata = append(metadata, value.ProviderMetadata...)
		}
	}
	require.Len(t, metadata, 7)
	for i, scope := range []ProviderMetadataScope{ProviderMetadataScopeText, ProviderMetadataScopeCompaction, ProviderMetadataScopeReasoning, ProviderMetadataScopeToolCall, ProviderMetadataScopeToolResult, ProviderMetadataScopeMessage, ProviderMetadataScopeContinuation} {
		require.Equal(t, scope, metadata[i].Scope)
		require.True(t, bytes.Equal(payload, metadata[i].Payload), "scope %s changed payload: %q", scope, metadata[i].Payload)
	}
}

func TestLegacyProviderMetadataMigratesFromDatabaseAndSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	conn, err := db.Connect(t.Context(), dir)
	require.NoError(t, err)
	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "legacy metadata")
	require.NoError(t, err)

	legacyPayload := `{ "type" : "future.private", "data" : { "number" : 1.00e+2 } }`
	parts := `[
		{"type":"text","data":{"text":"legacy","provider_options":{"missing.plugin":` + legacyPayload + `}}},
		{"type":"reasoning","data":{"thinking":"legacy","thought_signature":"signature","tool_id":"call","codex_reasoning_metadata":{"type":"codex.reasoning_metadata","data":{"encrypted_content":"opaque"}}}},
		{"type":"tool_call","data":{"id":"call","name":"tool","input":"{}","finished":true,"codex_item_id":"item"}}
	]`
	_, err = q.CreateMessage(t.Context(), db.CreateMessageParams{
		ID: "legacy-message", SessionID: sess.ID, Role: string(Assistant), Parts: parts,
		Model: sql.NullString{String: "legacy-model", Valid: true}, Provider: sql.NullString{String: "missing.plugin", Valid: true},
	})
	require.NoError(t, err)

	svc := NewService(q)
	loaded, err := svc.Get(t.Context(), "legacy-message")
	require.NoError(t, err)
	require.Equal(t, []byte(legacyPayload), loaded.Parts[0].(TextContent).ProviderMetadata[0].Payload)
	require.Len(t, loaded.Parts[1].(ReasoningContent).ProviderMetadata, 3)
	require.Len(t, loaded.Parts[2].(ToolCall).ProviderMetadata, 1)
	loaded.AppendContent(" migrated")
	require.NoError(t, svc.Update(t.Context(), loaded))
	require.NoError(t, db.Release(dir))

	conn, err = db.Connect(t.Context(), dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Release(dir) })
	reloaded, err := NewService(db.New(conn)).Get(t.Context(), "legacy-message")
	require.NoError(t, err)
	require.Equal(t, "legacy migrated", reloaded.Parts[0].(TextContent).Text)
	require.Equal(t, []byte(legacyPayload), reloaded.Parts[0].(TextContent).ProviderMetadata[0].Payload)
	require.Len(t, reloaded.Parts[1].(ReasoningContent).ProviderMetadata, 3)
	require.Len(t, reloaded.Parts[2].(ToolCall).ProviderMetadata, 1)
}

func TestUnknownProviderMetadataSurvivesDatabaseUpdate(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "metadata round trip")
	require.NoError(t, err)

	payload := []byte("{ \"precise\": 1.00e+7, \"ordered\": [\"b\", \"a\"] }")
	envelope, err := NewProviderMetadataEnvelope("missing.plugin", 9, ProviderMetadataScopeText, payload)
	require.NoError(t, err)

	svc := NewService(q)
	created, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{
		Role: User,
		Parts: []ContentPart{TextContent{
			Text:             "original",
			ProviderMetadata: ProviderMetadata{envelope},
		}},
	})
	require.NoError(t, err)

	created.AppendContent(" updated")
	require.NoError(t, svc.Update(t.Context(), created))

	loaded, err := svc.Get(t.Context(), created.ID)
	require.NoError(t, err)
	metadata := loaded.Content().ProviderMetadata
	require.Len(t, metadata, 1)
	require.True(t, bytes.Equal(payload, metadata[0].Payload), "opaque payload changed: %q", metadata[0].Payload)
}

// slowUpdateQuerier wraps a [db.Querier] and forces UpdateMessage to
// hang on a release channel. Used to simulate an in-flight SQL write.
type slowUpdateQuerier struct {
	db.Querier
	release   chan struct{}
	started   chan struct{}
	startOnce sync.Once
}

func (s *slowUpdateQuerier) UpdateMessage(ctx context.Context, arg db.UpdateMessageParams) error {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return s.Querier.UpdateMessage(ctx, arg)
}

// newTestService spins up a fresh in-memory message.Service backed by a
// temporary on-disk SQLite database. Returns the service plus a session
// ID to attach messages to.
func TestCommitCompactionAtomicallyInstallsCheckpoint(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "original title")
	require.NoError(t, err)
	sess.Todos = []session.Todo{{Content: "keep", Status: session.TodoStatusPending, ActiveForm: "Keeping"}}
	_, err = sessions.Save(t.Context(), sess)
	require.NoError(t, err)

	svc := NewService(q, WithCompactionStore(conn, sessions))
	messageEvents := svc.Subscribe(t.Context())
	sessionEvents := sessions.Subscribe(t.Context())
	committed, err := svc.CommitCompaction(t.Context(), CommitCompactionParams{
		SessionID:        sess.ID,
		Parts:            []ContentPart{TextContent{Text: "checkpoint"}, Finish{Reason: FinishReasonEndTurn, Time: time.Now().Unix()}},
		Model:            "gpt-test",
		Provider:         "codex",
		PromptTokens:     321,
		CompletionTokens: 0,
		Cost:             1.25,
		EstimatedUsage:   true,
	})
	require.NoError(t, err)
	require.True(t, committed.IsSummaryMessage)
	require.Equal(t, "checkpoint", committed.Content().Text)

	storedSession, err := sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Equal(t, committed.ID, storedSession.SummaryMessageID)
	require.Equal(t, int64(321), storedSession.PromptTokens)
	require.Zero(t, storedSession.CompletionTokens)
	require.Equal(t, 1.25, storedSession.Cost)
	require.True(t, storedSession.EstimatedUsage)
	require.Equal(t, "original title", storedSession.Title)
	require.Equal(t, sess.Todos, storedSession.Todos)

	dbMessage, err := q.GetMessage(t.Context(), committed.ID)
	require.NoError(t, err)
	require.True(t, dbMessage.FinishedAt.Valid)
	require.Equal(t, pubsub.CreatedEvent, (<-messageEvents).Type)
	require.Equal(t, pubsub.UpdatedEvent, (<-sessionEvents).Type)
}

func TestCommitCompactionRollsBackWhenCheckpointUpdateFails(t *testing.T) {
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	svc := NewService(q, WithCompactionStore(conn, sessions))
	messageEvents := svc.Subscribe(t.Context())
	sessionEvents := sessions.Subscribe(t.Context())

	_, err = conn.ExecContext(t.Context(), `
		CREATE TRIGGER reject_compaction
		BEFORE UPDATE OF summary_message_id ON sessions
		WHEN new.summary_message_id IS NOT NULL
		BEGIN
			SELECT RAISE(ABORT, 'rejected compaction');
		END;
	`)
	require.NoError(t, err)

	_, err = svc.CommitCompaction(t.Context(), CommitCompactionParams{
		SessionID:      sess.ID,
		Parts:          []ContentPart{TextContent{Text: "must roll back"}, Finish{Reason: FinishReasonEndTurn, Time: time.Now().Unix()}},
		Model:          "gpt-test",
		Provider:       "codex",
		PromptTokens:   99,
		Cost:           2,
		EstimatedUsage: true,
	})
	require.ErrorContains(t, err, "installing compacted checkpoint")

	messages, err := svc.List(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, messages)
	storedSession, err := sessions.Get(t.Context(), sess.ID)
	require.NoError(t, err)
	require.Empty(t, storedSession.SummaryMessageID)
	require.Zero(t, storedSession.PromptTokens)
	require.Zero(t, storedSession.Cost)

	select {
	case event := <-messageEvents:
		t.Fatalf("unexpected message event: %#v", event)
	default:
	}
	select {
	case event := <-sessionEvents:
		t.Fatalf("unexpected session event: %#v", event)
	default:
	}
}

func newTestService(t *testing.T, opts ...ServiceOption) (Service, string) {
	t.Helper()
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	svc := NewService(q, opts...)
	return svc, sess.ID
}

// eventCollector consumes broker events into a slice in a goroutine
// and exposes thread-safe Snapshot / Reset helpers for assertions.
type eventCollector struct {
	mu     sync.Mutex
	events []pubsub.Event[Message]
}

func collect(ctx context.Context, sub <-chan pubsub.Event[Message]) *eventCollector {
	c := &eventCollector{}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				c.mu.Lock()
				c.events = append(c.events, ev)
				c.mu.Unlock()
			}
		}
	}()
	return c
}

func (c *eventCollector) snapshot() []pubsub.Event[Message] {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]pubsub.Event[Message], len(c.events))
	copy(out, c.events)
	return out
}

func (c *eventCollector) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = nil
}

func TestListFromUsesStableCheckpointOrder(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t)
	first, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: User, Parts: []ContentPart{TextContent{Text: "first"}}})
	require.NoError(t, err)
	checkpoint, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant, Parts: []ContentPart{TextContent{Text: "summary"}}})
	require.NoError(t, err)
	last, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: User, Parts: []ContentPart{TextContent{Text: "last"}}})
	require.NoError(t, err)

	all, err := svc.List(t.Context(), sessionID)
	require.NoError(t, err)
	require.Equal(t, []string{first.ID, checkpoint.ID, last.ID}, []string{all[0].ID, all[1].ID, all[2].ID})

	fromCheckpoint, err := svc.ListFrom(t.Context(), sessionID, checkpoint.ID)
	require.NoError(t, err)
	require.Equal(t, []string{checkpoint.ID, last.ID}, []string{fromCheckpoint[0].ID, fromCheckpoint[1].ID})

	stale, err := svc.ListFrom(t.Context(), sessionID, "missing")
	require.NoError(t, err)
	require.Empty(t, stale)
}

func TestUpdate_DebouncesTextDeltas(t *testing.T) {
	t.Parallel()

	// Long-enough debounce that we can verify nothing flushes prematurely.
	svc, sessionID := newTestService(t, WithDebounce(50*time.Millisecond))

	subCtx, cancelSub := context.WithCancel(t.Context())
	defer cancelSub()
	sub := svc.Subscribe(subCtx)
	collector := collect(subCtx, sub)

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{
		Role: Assistant,
	})
	require.NoError(t, err)
	// Drop the CreatedEvent emitted by Create.
	time.Sleep(5 * time.Millisecond)
	collector.reset()

	// Push 5 deltas inside a single debounce window.
	for i := 0; i < 5; i++ {
		msg.AppendContent("a")
		require.NoError(t, svc.Update(t.Context(), msg))
	}

	// Before the debounce expires no UpdatedEvent should have landed.
	time.Sleep(10 * time.Millisecond)
	require.Empty(t, collector.snapshot(), "no events should land before debounce window expires")

	// Wait for the debounce timer to fire.
	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond)
	events := collector.snapshot()
	require.Len(t, events, 1, "5 deltas should coalesce into 1 UpdatedEvent")
	require.Equal(t, pubsub.UpdatedEvent, events[0].Type)
	require.Equal(t, "aaaaa", events[0].Payload.Content().Text)

	// Final state must be persisted.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "aaaaa", got.Content().Text)
}

func TestUpdate_TerminalUpdatesFlushSynchronously(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	subCtx, cancelSub := context.WithCancel(t.Context())
	defer cancelSub()
	sub := svc.Subscribe(subCtx)
	collector := collect(subCtx, sub)

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	collector.reset()

	// AddFinish makes the message terminal; Update must flush
	// synchronously even with a 1-hour debounce.
	msg.AppendContent("done")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.Eventually(t, func() bool {
		return len(collector.snapshot()) >= 1
	}, time.Second, 5*time.Millisecond,
		"terminal update must publish without waiting for debounce")
	events := collector.snapshot()
	require.Len(t, events, 1)
	require.True(t, events[0].Payload.IsFinished())

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.True(t, got.IsFinished())
}

func TestUpdate_ToolCallStructuralChangeFlushes(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	// Adding a new tool call is a structural change → sync flush.
	msg.AddToolCall(ToolCall{ID: "tc1", Name: "view", Finished: false})
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Len(t, got.ToolCalls(), 1)
	require.Equal(t, "tc1", got.ToolCalls()[0].ID)

	// Marking the tool call finished is also a structural change.
	msg.AddToolCall(ToolCall{ID: "tc1", Name: "view", Input: "{}", Finished: true})
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.True(t, got.ToolCalls()[0].Finished)
}

func TestUpdate_ReasoningEndFlushes(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	// Reasoning deltas alone debounce.
	msg.AppendReasoningContent("hmm")
	require.NoError(t, svc.Update(t.Context(), msg))
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Empty(t, got.ReasoningContent().Thinking, "reasoning delta should still be in the debounce buffer")

	// FinishThinking sets FinishedAt → terminal flush.
	msg.FinishThinking()
	require.NoError(t, svc.Update(t.Context(), msg))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "hmm", got.ReasoningContent().Thinking)
	require.NotZero(t, got.ReasoningContent().FinishedAt)
}

func TestFlush_DrainsPendingDebouncedUpdates(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("buffered")
	require.NoError(t, svc.Update(t.Context(), msg))

	// Without a flush the SQL row is unchanged from Create.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Empty(t, got.Content().Text)

	require.NoError(t, svc.Flush(t.Context(), msg.ID))

	got, err = svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "buffered", got.Content().Text)

	// Subsequent Flush is a no-op.
	require.NoError(t, svc.Flush(t.Context(), msg.ID))
}

func TestFlushAll_DrainsAllPending(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	const n = 5
	msgs := make([]Message, n)
	for i := range msgs {
		m, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
		require.NoError(t, err)
		m.AppendContent("hi")
		require.NoError(t, svc.Update(t.Context(), m))
		msgs[i] = m
	}

	require.NoError(t, svc.FlushAll(t.Context()))

	for _, m := range msgs {
		got, err := svc.Get(t.Context(), m.ID)
		require.NoError(t, err)
		require.Equal(t, "hi", got.Content().Text, "FlushAll should drain every pending message")
	}
}

func TestUpdate_OrderingMatchesNonCoalesced(t *testing.T) {
	t.Parallel()

	// Compare the final state after coalesced vs zero-debounce updates.
	// A sequence of interleaved text/reasoning/tool-call updates must
	// converge to the same final DB row either way.
	build := func(svc Service, sessionID string) Message {
		msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
		require.NoError(t, err)
		msg.AppendReasoningContent("thinking 1 ")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendReasoningContent("thinking 2")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.FinishThinking()
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendContent("hello ")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AppendContent("world")
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddToolCall(ToolCall{ID: "tc", Name: "x", Finished: false})
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddToolCall(ToolCall{ID: "tc", Name: "x", Input: "{}", Finished: true})
		require.NoError(t, svc.Update(t.Context(), msg))
		msg.AddFinish(FinishReasonEndTurn, "", "")
		require.NoError(t, svc.Update(t.Context(), msg))
		return msg
	}

	coalesced, sid1 := newTestService(t, WithDebounce(20*time.Millisecond))
	a := build(coalesced, sid1)
	require.NoError(t, coalesced.FlushAll(t.Context()))
	gotA, err := coalesced.Get(t.Context(), a.ID)
	require.NoError(t, err)

	immediate, sid2 := newTestService(t, WithDebounce(0))
	b := build(immediate, sid2)
	gotB, err := immediate.Get(t.Context(), b.ID)
	require.NoError(t, err)

	require.Equal(t, gotA.Content().Text, gotB.Content().Text)
	require.Equal(t, gotA.ReasoningContent().Thinking, gotB.ReasoningContent().Thinking)
	require.Equal(t, len(gotA.ToolCalls()), len(gotB.ToolCalls()))
	require.Equal(t, gotA.IsFinished(), gotB.IsFinished())
}

func TestDelete_DropsPendingState(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))
	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("dropped")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.NoError(t, svc.Delete(t.Context(), msg.ID))

	// FlushAll after Delete must not write to the deleted row.
	require.NoError(t, svc.FlushAll(t.Context()))

	_, err = svc.Get(t.Context(), msg.ID)
	require.Error(t, err, "deleted message must remain deleted")
}

func TestBroker_PublishLossyDropCounter(t *testing.T) {
	t.Parallel()

	// Tiny channel buffer so we can saturate from a single sender.
	b := pubsub.NewBrokerWithOptions[int](1)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)
	require.NotNil(t, sub)

	// Don't read from sub. Saturate the buffer.
	for range 100 {
		b.Publish(pubsub.UpdatedEvent, 1)
	}

	require.GreaterOrEqual(t, b.DropCount(), uint64(1),
		"lossy Publish must increment the drop counter under contention")
}

func TestBroker_PublishMustDeliverHonorsTimeout(t *testing.T) {
	t.Parallel()

	b := pubsub.NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(20 * time.Millisecond)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)
	require.NotNil(t, sub)

	// Saturate: one event sits in the buffer, the second must wait.
	b.Publish(pubsub.UpdatedEvent, 1)

	// PublishMustDeliver should block up to 20ms then drop.
	start := time.Now()
	b.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, 2)
	elapsed := time.Since(start)

	require.GreaterOrEqual(t, elapsed, 20*time.Millisecond,
		"PublishMustDeliver should block at least the timeout under contention")
	require.Less(t, elapsed, 200*time.Millisecond,
		"PublishMustDeliver must not block indefinitely")
	require.GreaterOrEqual(t, b.MustDeliverDropCount(), uint64(1),
		"timeout must increment the must-deliver drop counter")
}

func TestBroker_PublishMustDeliverWithReader(t *testing.T) {
	t.Parallel()

	b := pubsub.NewBrokerWithOptions[int](1)
	b.SetMustDeliverTimeout(50 * time.Millisecond)
	defer b.Shutdown()

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := b.Subscribe(subCtx)

	var received atomic.Uint64
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-subCtx.Done():
				return
			case _, ok := <-sub:
				if !ok {
					return
				}
				received.Add(1)
			}
		}
	}()

	for i := range 10 {
		b.PublishMustDeliver(t.Context(), pubsub.UpdatedEvent, i)
	}

	// All 10 should land within the must-deliver timeout window.
	require.Eventually(t, func() bool { return received.Load() == 10 },
		time.Second, 5*time.Millisecond,
		"all must-deliver events should reach an active subscriber")
	require.Zero(t, b.MustDeliverDropCount(),
		"no drops expected when subscriber drains promptly")
}

func TestUpdate_TerminalEventUsesMustDeliver(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(time.Hour))

	subCtx, cancel := context.WithCancel(t.Context())
	defer cancel()
	sub := svc.Subscribe(subCtx)

	var seenFinish atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-subCtx.Done():
				return
			case ev, ok := <-sub:
				if !ok {
					return
				}
				if ev.Type == pubsub.UpdatedEvent && ev.Payload.IsFinished() {
					seenFinish.Store(true)
				}
			}
		}
	}()

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("final")
	msg.AddFinish(FinishReasonEndTurn, "", "")
	require.NoError(t, svc.Update(t.Context(), msg))

	require.Eventually(t, func() bool { return seenFinish.Load() },
		time.Second, 10*time.Millisecond,
		"terminal update must reach subscribers via the must-deliver path")
}

func TestUpdate_ZeroDebounceFlushesEveryUpdate(t *testing.T) {
	t.Parallel()

	svc, sessionID := newTestService(t, WithDebounce(0))

	msg, err := svc.Create(t.Context(), sessionID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)

	for i := 0; i < 3; i++ {
		msg.AppendContent("x")
		require.NoError(t, svc.Update(t.Context(), msg))
		got, err := svc.Get(t.Context(), msg.ID)
		require.NoError(t, err)
		require.Len(t, got.Content().Text, i+1, "every update must land synchronously when debounce is 0")
	}
}

// TestFlush_WaitsForInFlightWrite reproduces the failure where Flush
// or FlushAll could return before a concurrent in-flight SQL write
// completed. We block UpdateMessage on a release channel, fire the
// debounce timer, then call Flush and assert it does not return until
// the in-flight write actually lands.
func TestFlush_WaitsForInFlightWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	// Short debounce so the timer fires quickly.
	svc := NewService(slow, WithDebounce(10*time.Millisecond))

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("payload")
	require.NoError(t, svc.Update(t.Context(), msg))

	// Wait for the timer-fired flush to enter UpdateMessage.
	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("timer-fired flush never reached UpdateMessage")
	}

	// At this point the buffer is dirty=false but flushing=true. A
	// naive Flush would early-return on !dirty. Spawn Flush in a
	// goroutine and assert it has not returned while the write is
	// still blocked.
	flushDone := make(chan error, 1)
	go func() { flushDone <- svc.Flush(t.Context(), msg.ID) }()

	select {
	case err := <-flushDone:
		t.Fatalf("Flush returned %v while in-flight write was still blocked", err)
	case <-time.After(50 * time.Millisecond):
		// Expected: Flush is correctly waiting.
	}

	// Release the slow write; Flush must now return cleanly.
	close(slow.release)
	select {
	case err := <-flushDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("Flush did not return after in-flight write completed")
	}

	// The SQL row should now reflect the buffered payload.
	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "payload", got.Content().Text)
}

// TestFlushAll_WaitsForInFlightWrite asserts FlushAll picks up IDs
// whose buffer is currently flushing (dirty=false) so shutdown and
// session-switch callers don't return while an SQL write is mid-flight.
func TestFlushAll_WaitsForInFlightWrite(t *testing.T) {
	t.Parallel()

	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	q := db.New(conn)
	sessions := session.NewService(q, conn)
	sess, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)

	slow := &slowUpdateQuerier{
		Querier: q,
		release: make(chan struct{}),
		started: make(chan struct{}),
	}
	svc := NewService(slow, WithDebounce(10*time.Millisecond))

	msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
	require.NoError(t, err)
	msg.AppendContent("payload")
	require.NoError(t, svc.Update(t.Context(), msg))

	select {
	case <-slow.started:
	case <-time.After(time.Second):
		t.Fatal("timer-fired flush never reached UpdateMessage")
	}

	flushDone := make(chan error, 1)
	go func() { flushDone <- svc.FlushAll(t.Context()) }()

	select {
	case err := <-flushDone:
		t.Fatalf("FlushAll returned %v while in-flight write was still blocked", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(slow.release)
	select {
	case err := <-flushDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("FlushAll did not return after in-flight write completed")
	}

	got, err := svc.Get(t.Context(), msg.ID)
	require.NoError(t, err)
	require.Equal(t, "payload", got.Content().Text)
}

// TestUpdate_StructuralFlushUsesMustDeliver covers the second review
// finding: structural terminal events (tool-call add, tool-call
// finish, reasoning end) must publish via the must-deliver path even
// when the message itself is not yet IsFinished.
//
// We detect which path was taken by saturating a subscriber's channel
// buffer with no reader. With a short must-deliver timeout, the
// must-deliver path increments [pubsub.Broker.MustDeliverDropCount]
// after the timeout expires; the lossy path increments
// [pubsub.Broker.DropCount] immediately. The two counters are
// disjoint, so they precisely identify which call site published the
// event.
func TestUpdate_StructuralFlushUsesMustDeliver(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		mut  func(*Message)
	}{
		{
			name: "tool call add",
			mut: func(m *Message) {
				m.AddToolCall(ToolCall{ID: "tc1", Name: "view"})
			},
		},
		{
			name: "tool call finish",
			mut: func(m *Message) {
				m.AddToolCall(ToolCall{ID: "tc1", Name: "view", Input: "{}", Finished: true})
			},
		},
		{
			name: "reasoning end",
			mut: func(m *Message) {
				m.AppendReasoningContent("hmm")
				m.FinishThinking()
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			conn, err := db.Connect(t.Context(), t.TempDir())
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })

			q := db.New(conn)
			sessions := session.NewService(q, conn)
			sess, err := sessions.Create(t.Context(), "test")
			require.NoError(t, err)

			// Replace the default broker with a tiny buffer + short
			// must-deliver timeout so we can fully saturate from a
			// single sender and observe drops without long waits.
			svc := NewService(q, WithDebounce(time.Hour))
			impl := svc.(*service)
			impl.Shutdown()
			impl.Broker = pubsub.NewBrokerWithOptions[Message](1)
			impl.SetMustDeliverTimeout(40 * time.Millisecond)

			subCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			sub := svc.Subscribe(subCtx)

			msg, err := svc.Create(t.Context(), sess.ID, CreateMessageParams{Role: Assistant})
			require.NoError(t, err)

			// Saturate the subscriber's buffer (capacity 1). The
			// CreatedEvent from Create above already left one event
			// in the buffer; we never read sub, so the next publish
			// has nowhere to go.
			_ = sub // intentionally not drained.

			// Drive the structural change. With debounce=1h, Update
			// flushes synchronously and routes through whichever
			// publish path the service chose for structural events.
			tc.mut(&msg)
			require.NoError(t, svc.Update(t.Context(), msg))

			// Must-deliver timeout (40ms) should have expired with
			// no drain. If structural events are routed through
			// must-deliver: MustDeliverDropCount > 0, DropCount
			// unchanged. If routed through lossy Publish:
			// DropCount > 0, MustDeliverDropCount == 0.
			require.Eventually(t, func() bool {
				return impl.MustDeliverDropCount() >= 1
			}, time.Second, 5*time.Millisecond,
				"structural terminal event should publish via must-deliver, not lossy Publish")
			require.Zero(t, impl.DropCount(),
				"structural terminal event must not be silently dropped via lossy Publish")
		})
	}
}
