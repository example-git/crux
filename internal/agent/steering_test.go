package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/shell"
	"github.com/stretchr/testify/require"
)

type steeringModel struct {
	finishStreamModel
	calls chan fantasy.Call
	first func(context.Context, fantasy.Call) (fantasy.StreamResponse, error)
	count atomic.Int32
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
	_, err = c.RunAccepted(WithDeliveryMode(ctx, DeliverySteer), accepted, current.ID, "new direction")
	require.NoError(t, err)
	var replacement fantasy.Call
	select {
	case replacement = <-model.calls:
	case err := <-done:
		t.Fatalf("turn ended before replacement request: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("replacement request did not arrive after partial generation")
	}
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

func TestSteeringWaitsOnlyForSummarizationAndPrecedesQueue(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "explicit", true: "automatic"}[automatic], func(t *testing.T) {
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
			_, err = c.Run(WithDeliveryMode(ctx, DeliverySteer), current.ID, "priority steering input")
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
			require.False(t, a.IsSessionBusy(current.ID))
		})
	}
}
