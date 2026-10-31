package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	agenttools "github.com/example-git/crux/internal/agent/tools"
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
