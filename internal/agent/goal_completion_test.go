package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	agenttools "github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

type goalCompletionModel struct {
	finishStreamModel
	generation int64
	calls      atomic.Int32
}

func (m *goalCompletionModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	if m.calls.Add(1) != 1 {
		return nil, errors.New("goal completion requested another provider step")
	}
	input := fmt.Sprintf(`{"generation":%d,"status":"complete"}`, m.generation)
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{
			Type:          fantasy.StreamPartTypeToolCall,
			ID:            "complete-goal",
			ToolCallName:  agenttools.UpdateGoalToolName,
			ToolCallInput: input,
		}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func TestInternalGoalTurnCompletesThroughAgentLoop(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)

	model := &goalCompletionModel{generation: current.Goal.Generation}
	currentAgent := testSessionAgent(
		env,
		model,
		&finishStreamModel{text: "generated title"},
		"system",
		agenttools.NewUpdateGoalTool(env.sessions),
	).(*sessionAgent)
	accepted, ok := currentAgent.BeginIdleAccepted(current.ID)
	require.True(t, ok)

	result, err := currentAgent.Run(t.Context(), SessionAgentCall{
		SessionID: current.ID,
		Prompt:    goalContinuationPrompt,
		Accepted:  accepted,
		Internal:  true,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, int32(1), model.calls.Load())

	completed, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusComplete, completed.Goal.Status)
	require.Equal(t, current.Goal.Generation, completed.Goal.Generation)
	require.Equal(t, "goal session", completed.Title)

	messages, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	var assistantMessages int
	var toolMessages int
	for _, currentMessage := range messages {
		require.NotEqual(t, message.User, currentMessage.Role)
		switch currentMessage.Role {
		case message.Assistant:
			assistantMessages++
			require.Equal(t, message.FinishReasonEndTurn, currentMessage.FinishReason())
		case message.Tool:
			toolMessages++
		}
	}
	require.Equal(t, 1, assistantMessages)
	require.Equal(t, 1, toolMessages)
}

type goalRefusalModel struct {
	finishStreamModel
	calls atomic.Int32
}

func (m *goalRefusalModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls.Add(1)
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonContentFilter})
	}, nil
}

func TestGoalRefusalBlocksThroughAgentAndScheduler(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	model := &goalRefusalModel{}
	currentAgent := testSessionAgent(env, model, &finishStreamModel{text: "generated title"}, "system").(*sessionAgent)
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.reconcileInterval = time.Millisecond
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		accepted, ok := currentAgent.BeginIdleAccepted(sessionID)
		if !ok {
			return nil, false, nil
		}
		defer accepted.Close()
		result, runErr := currentAgent.Run(ctx, SessionAgentCall{
			SessionID: sessionID,
			Prompt:    goalContinuationPrompt,
			Accepted:  accepted,
			Internal:  true,
		})
		return result, true, runErr
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})
	require.Eventually(t, func() bool {
		stored, getErr := env.sessions.Get(t.Context(), current.ID)
		return getErr == nil && stored.Goal.Status == session.GoalStatusBlocked
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), model.calls.Load())
	require.Never(t, func() bool { return model.calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond)
	messages, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	require.Equal(t, message.Assistant, messages[0].Role)
	require.Equal(t, message.FinishReasonContentFilter, messages[0].FinishReason())
}

func TestManualRefusalPreservesActiveGoal(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	model := &goalRefusalModel{}
	currentAgent := testSessionAgent(env, model, &finishStreamModel{text: "generated title"}, "system").(*sessionAgent)
	accepted, ok := currentAgent.BeginIdleAccepted(current.ID)
	require.True(t, ok)
	defer accepted.Close()
	result, err := currentAgent.Run(t.Context(), SessionAgentCall{
		SessionID: current.ID,
		Prompt:    "inspect the work",
		Accepted:  accepted,
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, fantasy.FinishReasonContentFilter, result.Steps[len(result.Steps)-1].FinishReason)
	stored, err := env.sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, current.Goal, stored.Goal)
	require.Equal(t, int32(1), model.calls.Load())
}

type goalHandoffModel struct {
	finishStreamModel
	calls int
}

func (m *goalHandoffModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.calls++
	if m.calls > 1 {
		return m.finishStreamModel.Stream(ctx, call)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "goal-work", ToolCallName: "work", ToolCallInput: `{}`}) {
			return
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, FinishReason: fantasy.FinishReasonToolCalls})
	}, nil
}

func TestSessionKeepsAdmittedModelAcrossUnrelatedSelection(t *testing.T) {
	env := testEnv(t)
	original := &goalHandoffModel{finishStreamModel: finishStreamModel{text: "original goal response"}}
	unrelated := &summaryCaptureModel{finishStreamModel: finishStreamModel{text: "unrelated response"}}
	var currentAgent SessionAgent
	tool := fantasy.NewAgentTool[struct{}]("work", "Perform work", func(context.Context, struct{}, fantasy.ToolCall) (fantasy.ToolResponse, error) {
		next := currentAgent.Runtime()
		next.LargeModel.Model = unrelated
		next.LargeModel.ModelCfg = config.SelectedModel{Provider: "unrelated-provider", Model: "unrelated-model"}
		currentAgent.SetRuntime(next)
		return fantasy.NewTextResponse("work completed"), nil
	})
	currentAgent = testSessionAgent(env, original, &finishStreamModel{text: "title"}, "system", tool)
	runtime := currentAgent.Runtime()
	runtime.LargeModel.ModelCfg = config.SelectedModel{Provider: "original-provider", Model: "original-model"}
	currentAgent.SetRuntime(runtime)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	_, err = currentAgent.Run(t.Context(), SessionAgentCall{
		SessionID:       current.ID,
		Prompt:          goalContinuationPrompt,
		Internal:        true,
		RuntimeProvider: func(context.Context) (InstalledRuntime, error) { return runtime, nil },
		PrepareRuntime: func(context.Context, InstalledRuntime) (fantasy.StepModelSettings, error) {
			t.Error("automatic goal adopted an unrelated runtime")
			return fantasy.StepModelSettings{}, nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, 2, original.calls)
	calls, _ := unrelated.snapshot()
	require.Empty(t, calls)
	stored, err := env.messages.List(t.Context(), current.ID)
	require.NoError(t, err)
	for _, msg := range stored {
		if msg.Role == message.Assistant {
			require.Equal(t, "original-provider", msg.Provider)
			require.Equal(t, "original-model", msg.Model)
		}
	}
	require.Equal(t, "unrelated-provider", currentAgent.Runtime().LargeModel.ModelCfg.Provider)
}
