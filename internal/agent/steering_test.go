package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
)

type steeringModel struct {
	finishStreamModel
	calls chan fantasy.Call
	first func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
	count atomic.Int32
}

type blockingFailureSessionService struct {
	session.Service
	entered chan struct{}
	release chan struct{}
	err     error
}

func (s *blockingFailureSessionService) Get(context.Context, string) (session.Session, error) {
	close(s.entered)
	<-s.release
	return session.Session{}, s.err
}

func (m *steeringModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls <- call
	if m.count.Add(1) == 1 && m.first != nil {
		return m.first(ctx, call)
	}
	return (&finishStreamModel{text: "redirected"}).Stream(ctx, call)
}

func steeringCall(t *testing.T, calls <-chan fantasy.Call) fantasy.Call {
	t.Helper()
	select {
	case call := <-calls:
		return call
	case <-time.After(5 * time.Second):
		t.Fatal("model request did not arrive")
		return fantasy.Call{}
	}
}

func steeringDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not finish")
		return nil
	}
}

func steeringPrompt(call fantasy.Call) string {
	var text strings.Builder
	for _, msg := range call.Prompt {
		for _, part := range msg.Content {
			if part, ok := part.(fantasy.TextPart); ok {
				text.WriteString(part.Text)
			}
		}
	}
	return text.String()
}

func TestSteeringInterruptsGenerationBeforeCoordinatorPreparation(t *testing.T) {
	for _, requireActive := range []bool{false, true} {
		t.Run(fmt.Sprintf("requireActive=%t", requireActive), func(t *testing.T) {
			env := testEnv(t)
			interrupted := make(chan struct{})
			generated := make(chan struct{})
			model := &steeringModel{calls: make(chan fantasy.Call, 4)}
			model.first = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
				return func(yield func(fantasy.StreamPart) bool) {
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "partial"}) {
						return
					}
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "partial", Delta: "partial response before steering"}) {
						return
					}
					close(generated)
					<-ctx.Done()
					close(interrupted)
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
				}, nil
			}
			a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system").(*sessionAgent)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			completions := make(chan notify.RunComplete, 4)
			done := make(chan error, 1)
			go func() {
				_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "original direction", OnComplete: func(event notify.RunComplete) { completions <- event }})
				done <- err
			}()
			steeringCall(t, model.calls)
			select {
			case <-generated:
			case <-time.After(5 * time.Second):
				t.Fatal("generation did not emit partial text")
			}
			c := &coordinator{currentAgent: a}
			accepted := a.BeginAccepted(current.ID)
			steerContext := WithDeliveryMode(ctx, DeliverySteer)
			if requireActive {
				steerContext = WithRequireActiveRun(steerContext)
			}
			admitted := make(chan error, 1)
			steerContext = WithAdmissionCallback(steerContext, func(err error) { admitted <- err })
			steerContext = WithInputMaterializer(steerContext, func(context.Context) ([]message.ContentPart, []message.Attachment, error) {
				select {
				case <-interrupted:
				default:
					t.Fatal("steering input was materialized before generation was interrupted")
				}
				return []message.ContentPart{message.TextContent{Text: "new direction"}}, nil, nil
			})
			_, err = c.RunAccepted(steerContext, accepted, current.ID, "")
			require.NoError(t, err)
			select {
			case err = <-admitted:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("steering input was not admitted")
			}
			stored, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			persisted := false
			for _, storedMessage := range stored {
				if strings.Contains(storedMessage.Content().String(), "new direction") {
					persisted = true
					break
				}
			}
			require.True(t, persisted)
			replacement := steeringCall(t, model.calls)
			request := steeringPrompt(replacement)
			require.Contains(t, request, "new direction")
			require.Contains(t, request, "partial response before steering")
			require.NoError(t, steeringDone(t, done))
			select {
			case <-interrupted:
			default:
				t.Fatal("generation was not interrupted")
			}
			require.Len(t, completions, 1)
			require.False(t, (<-completions).Cancelled)
			require.False(t, a.IsSessionBusy(current.ID))
			require.Zero(t, a.QueuedPrompts(current.ID))
		})
	}
}

func TestActiveRunFailureRejectsActiveAndQueuedAdmissions(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	expected := errors.New("session lookup failed")
	blockingSessions := &blockingFailureSessionService{
		Service: env.sessions,
		entered: make(chan struct{}),
		release: make(chan struct{}),
		err:     expected,
	}
	env.sessions = blockingSessions
	a := testSessionAgent(env, &finishStreamModel{}, &finishStreamModel{}, "system").(*sessionAgent)
	done := make(chan error, 1)
	activeAdmission := make(chan error, 1)
	go func() {
		_, err := a.Run(t.Context(), SessionAgentCall{
			SessionID:  current.ID,
			Prompt:     "original direction",
			OnAdmitted: func(err error) { activeAdmission <- err },
		})
		done <- err
	}()
	select {
	case <-blockingSessions.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("active run did not reach session lookup")
	}

	admitted := make(chan error, 1)
	materialized := atomic.Bool{}
	_, err = a.Run(t.Context(), SessionAgentCall{
		SessionID:    current.ID,
		DeliveryMode: DeliverySteer,
		InputMaterializer: func(context.Context) ([]message.ContentPart, []message.Attachment, error) {
			materialized.Store(true)
			return []message.ContentPart{message.TextContent{Text: "new direction"}}, nil, nil
		},
		OnAdmitted: func(err error) { admitted <- err },
	})
	require.NoError(t, err)
	close(blockingSessions.release)
	select {
	case err = <-admitted:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("queued steering admission was not rejected")
	}
	select {
	case err = <-activeAdmission:
		require.ErrorIs(t, err, expected)
	case <-time.After(5 * time.Second):
		t.Fatal("active turn admission was not rejected")
	}
	require.ErrorIs(t, steeringDone(t, done), expected)
	require.False(t, materialized.Load())
	require.Zero(t, a.QueuedPrompts(current.ID))
	select {
	case err = <-activeAdmission:
		t.Fatalf("active admission resolved more than once: %v", err)
	default:
	}
	select {
	case err = <-admitted:
		t.Fatalf("queued admission resolved more than once: %v", err)
	default:
	}
}

func TestQueuedTurnAdmissionWaitsForPersistence(t *testing.T) {
	env := testEnv(t)
	release := make(chan struct{})
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			<-release
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonStop})
		}, nil
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system").(*sessionAgent)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(t.Context(), SessionAgentCall{SessionID: current.ID, Prompt: "first"})
		done <- err
	}()
	steeringCall(t, model.calls)

	admitted := make(chan error, 1)
	_, err = a.Run(t.Context(), SessionAgentCall{
		SessionID:  current.ID,
		Prompt:     "second",
		OnAdmitted: func(err error) { admitted <- err },
	})
	require.NoError(t, err)
	select {
	case err = <-admitted:
		t.Fatalf("queued turn was admitted before persistence: %v", err)
	default:
	}
	require.Equal(t, 1, a.QueuedPrompts(current.ID))
	close(release)
	select {
	case err = <-admitted:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("queued turn was not admitted after persistence")
	}
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	persisted := false
	for _, storedMessage := range stored {
		if storedMessage.Role == message.User && strings.Contains(storedMessage.Content().String(), "second") {
			persisted = true
			break
		}
	}
	require.True(t, persisted)
	require.NoError(t, steeringDone(t, done))
	select {
	case err = <-admitted:
		t.Fatalf("queued admission resolved more than once: %v", err)
	default:
	}
}

func TestSteeringMaterializationFailureRejectsAdmissionWithoutPersistingInput(t *testing.T) {
	env := testEnv(t)
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			<-ctx.Done()
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: ctx.Err()})
		}, nil
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system").(*sessionAgent)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "original direction"})
		done <- err
	}()
	steeringCall(t, model.calls)

	expected := errors.New("missing steering input")
	admitted := make(chan error, 1)
	_, err = a.Run(ctx, SessionAgentCall{
		SessionID:    current.ID,
		DeliveryMode: DeliverySteer,
		InputMaterializer: func(context.Context) ([]message.ContentPart, []message.Attachment, error) {
			return nil, nil, expected
		},
		OnAdmitted: func(err error) { admitted <- err },
	})
	require.NoError(t, err)
	select {
	case err = <-admitted:
		require.ErrorIs(t, err, expected)
	case <-time.After(5 * time.Second):
		t.Fatal("steering materialization failure did not reject admission")
	}
	require.ErrorIs(t, steeringDone(t, done), expected)
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	userMessages := 0
	for _, storedMessage := range stored {
		if storedMessage.Role == message.User {
			userMessages++
		}
	}
	require.Equal(t, 1, userMessages)
}

func TestSteeringCancelsParallelBatchAndPreservesCompletedResults(t *testing.T) {
	env := testEnv(t)
	started := make(chan struct{}, 8)
	var canceled atomic.Int32
	var executions atomic.Int32
	complete := fantasy.NewAgentTool("complete", "Completed work", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		return fantasy.NewTextResponse("preserved completed result"), nil
	})
	work := fantasy.NewParallelAgentTool("parallel_work", "Parallel work", func(ctx context.Context, _ struct{}, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
		executions.Add(1)
		started <- struct{}{}
		<-ctx.Done()
		canceled.Add(1)
		return fantasy.ToolResponse{}, ctx.Err()
	})
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "completed", ToolCallName: "complete", ToolCallInput: `{}`}) {
				return
			}
			for i := range 7 {
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: string(rune('a' + i)), ToolCallName: "parallel_work", ToolCallInput: `{}`}) {
					return
				}
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system", complete, work)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "parallel work"})
		done <- err
	}()
	steeringCall(t, model.calls)
	for range 5 {
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("parallel tools did not start")
		}
	}
	_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliverySteer, Prompt: "redirect parallel work"})
	require.NoError(t, err)
	request := steeringCall(t, model.calls)
	require.Contains(t, steeringPrompt(request), "redirect parallel work")
	require.NoError(t, steeringDone(t, done))
	require.EqualValues(t, 5, executions.Load())
	require.EqualValues(t, 5, canceled.Load())
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	var results []message.ToolResult
	for _, msg := range stored {
		results = append(results, msg.ToolResults()...)
	}
	require.Len(t, results, 8)
	require.Equal(t, "preserved completed result", results[0].Content)
	require.False(t, results[0].IsError)
}

func TestRepeatedSteeringSurvivesPersistenceBoundary(t *testing.T) {
	env := testEnv(t)
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system")
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	persisted := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "original"})
		done <- err
	}()
	steeringCall(t, model.calls)
	_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliverySteer, Prompt: "first steering input", OnMessagePersisted: func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliverySteer, Prompt: "second steering input"})
		persisted <- err
	}})
	require.NoError(t, err)
	request := steeringPrompt(steeringCall(t, model.calls))
	require.Equal(t, 1, strings.Count(request, "first steering input"))
	require.Equal(t, 1, strings.Count(request, "second steering input"))
	require.NoError(t, steeringDone(t, persisted))
	require.NoError(t, steeringDone(t, done))
}

func TestDeliveryValidationAndPendingMetadata(t *testing.T) {
	require.ErrorContains(t, ValidateCall(SessionAgentCall{SessionID: "session", Prompt: "input", DeliveryMode: "invalid"}), "invalid delivery mode")
	require.ErrorContains(t, ValidateCall(SessionAgentCall{SessionID: "session", Prompt: "input", DeliveryMode: DeliverySteer, RunID: "separate"}), "separate run ID")
	for _, mode := range []DeliveryMode{"", DeliveryQueue} {
		require.ErrorContains(t, ValidateCall(SessionAgentCall{SessionID: "session", Prompt: "input", DeliveryMode: mode, RequireActiveRun: true}), "requiring an active run is only supported for steer delivery")
	}
	require.NoError(t, ValidateCall(SessionAgentCall{SessionID: "session", Prompt: "input", DeliveryMode: DeliverySteer, RequireActiveRun: true}))
	require.NoError(t, ValidateCall(SessionAgentCall{SessionID: "session", InputParts: []message.ContentPart{}}))
	require.NoError(t, ValidateCall(SessionAgentCall{SessionID: "session", InputMaterializer: func(context.Context) ([]message.ContentPart, []message.Attachment, error) { return nil, nil, nil }}))
	require.NoError(t, ValidateCall(SessionAgentCall{
		SessionID: "session",
		Attachments: []message.Attachment{{
			FileName: "image.png",
			MimeType: "image/png",
			Content:  []byte("image"),
		}},
	}))
	env := testEnv(t)
	a := testSessionAgent(env, &finishStreamModel{}, &finishStreamModel{}, "system").(*sessionAgent)
	a.messageQueue.Set("session", []SessionAgentCall{{SessionID: "session", Prompt: "queued", DeliveryMode: DeliveryQueue}, {SessionID: "session", Prompt: "steering", DeliveryMode: DeliverySteer}})
	require.Equal(t, []QueuedPrompt{{Prompt: "queued", DeliveryMode: DeliveryQueue}, {Prompt: "steering", DeliveryMode: DeliverySteer}}, a.QueuedPromptsList("session"))
	fold, dropped := a.drainQueueForStep("session")
	require.Empty(t, dropped)
	require.Len(t, fold, 1)
	require.Equal(t, "steering", fold[0].Prompt)
	require.Equal(t, []QueuedPrompt{{Prompt: "queued", DeliveryMode: DeliveryQueue}}, a.QueuedPromptsList("session"))
}

func TestQueueDoesNotInterruptGeneration(t *testing.T) {
	env := testEnv(t)
	release := make(chan struct{})
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
		select {
		case <-release:
			return (&finishStreamModel{text: "first done"}).Stream(ctx, call)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system")
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "first"})
		done <- err
	}()
	steeringCall(t, model.calls)
	_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliveryQueue, Prompt: "later queue"})
	require.NoError(t, err)
	select {
	case <-model.calls:
		t.Fatal("queue interrupted the current generation")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.Contains(t, steeringPrompt(steeringCall(t, model.calls)), "later queue")
	require.NoError(t, steeringDone(t, done))
}

func TestSteeringInterruptsForegroundBashAndSkipsRemainingTool(t *testing.T) {
	env := testEnv(t)
	manager := shell.NewBackgroundShellManager(env.workingDir)
	t.Cleanup(func() { manager.KillAll(context.Background()) })
	var unwanted atomic.Int32
	later := fantasy.NewAgentTool("later", "Must not execute", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		unwanted.Add(1)
		return fantasy.NewTextResponse("unwanted"), nil
	})
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
		return func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "running", ToolCallName: "bash", ToolCallInput: `{"command":"printf READY; sleep 30","description":"steering process test"}`}) {
				return
			}
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "later", ToolCallName: "later", ToolCallInput: `{}`}) {
				return
			}
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
		}, nil
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system", tools.NewBashTool(manager, env.permissions, env.workingDir), later)
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "run work"})
		done <- err
	}()
	steeringCall(t, model.calls)
	var process *shell.BackgroundShell
	require.Eventually(t, func() bool {
		for _, id := range manager.List() {
			p, ok := manager.Get(id)
			if ok {
				out, _, done, _ := p.GetOutput()
				if !done && strings.Contains(out, "READY") {
					process = p
					return true
				}
			}
		}
		return false
	}, 5*time.Second, 10*time.Millisecond)
	_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliverySteer, Prompt: "stop that work and redirect"})
	require.NoError(t, err)
	require.Contains(t, steeringPrompt(steeringCall(t, model.calls)), "stop that work and redirect")
	require.NoError(t, steeringDone(t, done))
	require.Zero(t, unwanted.Load())
	require.Eventually(t, func() bool { _, _, done, _ := process.GetOutput(); return done }, 2*time.Second, 10*time.Millisecond)
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	results := 0
	for _, msg := range stored {
		results += len(msg.ToolResults())
	}
	require.Equal(t, 2, results)
}

func TestSteeringThenCancelDoesNotRestartTurn(t *testing.T) {
	env := testEnv(t)
	interrupted := make(chan struct{})
	release := make(chan struct{})
	model := &steeringModel{calls: make(chan fantasy.Call, 4)}
	model.first = func(ctx context.Context, _ fantasy.Call) (fantasy.StreamResponse, error) {
		<-ctx.Done()
		close(interrupted)
		<-release
		return nil, ctx.Err()
	}
	a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system")
	current, err := env.sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "original"})
		done <- err
	}()
	steeringCall(t, model.calls)
	_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliverySteer, Prompt: "redirect"})
	require.NoError(t, err)
	select {
	case <-interrupted:
	case <-time.After(5 * time.Second):
		t.Fatal("steering did not interrupt")
	}
	a.Cancel(current.ID)
	close(release)
	require.ErrorIs(t, steeringDone(t, done), context.Canceled)
	require.EqualValues(t, 1, model.count.Load())
	require.Zero(t, a.QueuedPrompts(current.ID))
}

type steeringSummaryModel struct {
	finishStreamModel
	calls     chan fantasy.Call
	entered   chan context.Context
	release   chan struct{}
	automatic bool
	count     atomic.Int32
}

func (m *steeringSummaryModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if call.Headers["x-request-purpose"] == "summary" {
		m.entered <- ctx
		select {
		case <-m.release:
			return (&finishStreamModel{text: "<summary>retained checkpoint</summary>"}).Stream(ctx, call)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.calls <- call
	if m.automatic && m.count.Add(1) == 1 {
		return (&autoCompactionModel{}).Stream(ctx, call)
	}
	stream, err := (&finishStreamModel{text: "continued"}).Stream(ctx, call)
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for part := range stream {
			if part.Type == fantasy.StreamPartTypeFinish {
				part.Usage = fantasy.Usage{InputTokens: 10, OutputTokens: 1, TotalTokens: 11}
			}
			if !yield(part) {
				return
			}
		}
	}, nil
}

func TestIdleDeliveryStartsCoordinatorTurn(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		for _, mode := range []DeliveryMode{DeliveryQueue, DeliverySteer} {
			t.Run(fmt.Sprintf("accepted=%t/mode=%s", accepted, mode), func(t *testing.T) {
				t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
				env := testEnv(t)
				requests := make(chan string, 8)
				host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, err := io.ReadAll(r.Body)
					if err != nil || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer synthetic-steering-key" {
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					requests <- string(body)
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"idle send completed\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				t.Cleanup(host.Close)
				configuration := fmt.Sprintf(`{
  "options":{"disable_default_providers":true,"disable_auto_summarize":true},
  "providers":{"fixture":{"id":"fixture","name":"Fixture","type":"openai-compat",
    "base_url":%q,"api_key":"synthetic-steering-key",
    "models":[{"id":"fixture","name":"Fixture","context_window":8192,"default_max_tokens":128}]}},
  "models":{"large":{"provider":"fixture","model":"fixture"},"small":{"provider":"fixture","model":"fixture"}}
}`, host.URL+"/v1")
				require.NoError(t, os.WriteFile(filepath.Join(env.workingDir, "crux.json"), []byte(configuration), 0o600))
				store := initTestConfig(t, env.workingDir)
				store.SetupAgents()
				coder := store.Config().Agents[config.AgentCoder]
				coder.AllowedTools = nil
				store.Config().Agents[config.AgentCoder] = coder
				coord, err := NewCoordinator(t.Context(), CoordinatorOptions{
					Config: store, Sessions: env.sessions, Messages: env.messages,
					Permissions: env.permissions, History: env.history, FileTracker: *env.filetracker, Interactive: true,
				})
				require.NoError(t, err)
				t.Cleanup(func() { coord.(*coordinator).CloseContext(context.Background()) })
				current, err := env.sessions.Create(t.Context(), "session")
				require.NoError(t, err)
				for turn, prompt := range []string{"first idle message", "follow-up idle message"} {
					ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
					ctx = WithDeliveryMode(ctx, mode)
					if accepted {
						reservation := coord.BeginAccepted(current.ID)
						_, err = coord.RunAccepted(ctx, reservation, current.ID, prompt)
						reservation.Close()
					} else {
						_, err = coord.Run(ctx, current.ID, prompt)
					}
					cancel()
					require.NoError(t, err)
					require.False(t, coord.IsSessionBusy(current.ID))
					stored, err := env.messages.List(t.Context(), current.ID)
					require.NoError(t, err)
					require.Len(t, stored, (turn+1)*2)
					require.Equal(t, prompt, stored[turn*2].Content().String())
					require.Equal(t, "idle send completed", stored[turn*2+1].Content().String())
					found := false
					for len(requests) > 0 {
						var payload struct {
							Messages []struct {
								Role    string          `json:"role"`
								Content json.RawMessage `json:"content"`
							} `json:"messages"`
						}
						require.NoError(t, json.Unmarshal([]byte(<-requests), &payload))
						for _, msg := range payload.Messages {
							if msg.Role == "user" && strings.Contains(string(msg.Content), prompt) {
								found = true
							}
						}
					}
					require.True(t, found, "idle submission did not reach the model endpoint")
				}
			})
		}
	}
}

func TestActiveOnlySteeringRejectsIdleSessionWithoutStartingRun(t *testing.T) {
	for _, throughCoordinator := range []bool{false, true} {
		t.Run(fmt.Sprintf("coordinator=%t", throughCoordinator), func(t *testing.T) {
			env := testEnv(t)
			model := &steeringModel{calls: make(chan fantasy.Call, 1)}
			a := testSessionAgent(env, model, &finishStreamModel{text: "title"}, "system").(*sessionAgent)
			current, err := env.sessions.Create(t.Context(), "session")
			require.NoError(t, err)
			accepted := a.BeginAccepted(current.ID)
			defer accepted.Close()

			if throughCoordinator {
				c := &coordinator{currentAgent: a}
				ctx := WithRequireActiveRun(WithDeliveryMode(t.Context(), DeliverySteer))
				_, err = c.RunAccepted(ctx, accepted, current.ID, "stale direction")
			} else {
				_, err = a.Run(t.Context(), SessionAgentCall{
					SessionID: current.ID, Prompt: "stale direction", Accepted: accepted,
					DeliveryMode: DeliverySteer, RequireActiveRun: true,
				})
			}
			require.ErrorIs(t, err, ErrNoActiveRunToSteer)
			select {
			case <-model.calls:
				t.Fatal("stale steering input started a model run")
			default:
			}
			require.False(t, a.IsSessionBusy(current.ID))
			require.Zero(t, a.QueuedPrompts(current.ID))
			stored, err := env.messages.List(t.Context(), current.ID)
			require.NoError(t, err)
			require.Empty(t, stored)
		})
	}
}

func TestSteeringWaitsOnlyForSummarizationAndPrecedesQueue(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		for _, requireActive := range []bool{false, true} {
			t.Run(fmt.Sprintf("automatic=%t/requireActive=%t", automatic, requireActive), func(t *testing.T) {
				env := testEnv(t)
				model := &steeringSummaryModel{calls: make(chan fantasy.Call, 8), entered: make(chan context.Context, 1), release: make(chan struct{}), automatic: automatic}
				a := newSummaryTestAgent(env, model)
				runtime := a.Runtime()
				runtime.SmallModel.Model = &finishStreamModel{text: "title"}
				runtime.SummarizationContextCap = 100
				a.SetRuntime(runtime)
				current, err := env.sessions.Create(t.Context(), "session")
				require.NoError(t, err)
				_, err = env.messages.Create(t.Context(), current.ID, message.CreateMessageParams{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "older context"}}})
				require.NoError(t, err)
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				done := make(chan error, 1)
				go func() {
					if automatic {
						_, err := a.Run(ctx, SessionAgentCall{SessionID: current.ID, Prompt: "trigger summary"})
						done <- err
					} else {
						done <- a.Summarize(ctx, current.ID, nil, nil)
					}
				}()
				if automatic {
					steeringCall(t, model.calls)
				}
				var summaryCtx context.Context
				select {
				case summaryCtx = <-model.entered:
				case <-time.After(5 * time.Second):
					t.Fatal("summary did not start")
				}
				_, err = a.Run(ctx, SessionAgentCall{SessionID: current.ID, DeliveryMode: DeliveryQueue, Prompt: "ordinary queued input"})
				require.NoError(t, err)
				c := &coordinator{currentAgent: a}
				steerContext := WithDeliveryMode(ctx, DeliverySteer)
				if requireActive {
					steerContext = WithRequireActiveRun(steerContext)
				}
				admitted := make(chan error, 1)
				steerContext = WithAdmissionCallback(steerContext, func(err error) { admitted <- err })
				_, err = c.Run(steerContext, current.ID, "priority steering input")
				require.NoError(t, err)
				select {
				case <-summaryCtx.Done():
					t.Fatal("steering interrupted summarization")
				case <-model.calls:
					t.Fatal("generation resumed before summary completion")
				case <-time.After(100 * time.Millisecond):
				}
				close(model.release)
				first := steeringPrompt(steeringCall(t, model.calls))
				require.Contains(t, first, "retained checkpoint")
				require.Contains(t, first, "priority steering input")
				require.NotContains(t, first, "ordinary queued input")
				require.Contains(t, steeringPrompt(steeringCall(t, model.calls)), "ordinary queued input")
				require.NoError(t, steeringDone(t, done))
				select {
				case err := <-admitted:
					require.NoError(t, err)
				default:
					t.Fatal("steering admission did not resolve after summarization")
				}
				require.False(t, a.IsSessionBusy(current.ID))
			})
		}
	}
}
