package tools

import (
	"context"
	"encoding/json"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestUpdateGoalToolLifecycle(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "goal test")
	require.NoError(t, err)
	created, err = sessions.SetGoal(t.Context(), created.ID, "finish the work")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)
	tool := NewUpdateGoalTool(sessions)

	paused := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "paused"})
	require.False(t, paused.IsError)
	require.True(t, paused.StopTurn)
	current, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusPaused, current.Goal.Status)

	active := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "active"})
	require.False(t, active.IsError)
	require.False(t, active.StopTurn)
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusActive, current.Goal.Status)

	blocked := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "blocked", Error: "required service is unavailable"})
	require.False(t, blocked.IsError)
	require.True(t, blocked.StopTurn)
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusBlocked, current.Goal.Status)
	require.Equal(t, "required service is unavailable", current.Goal.Error)

	complete := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "complete"})
	require.False(t, complete.IsError)
	require.True(t, complete.StopTurn)
	current, err = sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusComplete, current.Goal.Status)
	require.Empty(t, current.Goal.Error)
}

func TestUpdateGoalToolRejectsInvalidAndStaleMutations(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "goal test")
	require.NoError(t, err)
	created, err = sessions.SetGoal(t.Context(), created.ID, "first objective")
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, created.ID)
	tool := NewUpdateGoalTool(sessions)

	invalid := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "done"})
	require.True(t, invalid.IsError)
	missingBlocker := runUpdateGoalTool(t, ctx, tool, UpdateGoalParams{Generation: created.Goal.Generation, Status: "blocked"})
	require.True(t, missingBlocker.IsError)

	replacement, err := sessions.SetGoal(t.Context(), created.ID, "replacement objective")
	require.NoError(t, err)
	input, err := json.Marshal(UpdateGoalParams{Generation: created.Goal.Generation, Status: "complete"})
	require.NoError(t, err)
	_, err = tool.Run(ctx, fantasy.ToolCall{ID: "stale", Name: UpdateGoalToolName, Input: string(input)})
	require.ErrorIs(t, err, session.ErrGoalChanged)
	current, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, replacement.Goal.Generation, current.Goal.Generation)
	require.Equal(t, session.GoalStatusActive, current.Goal.Status)
}

func runUpdateGoalTool(t *testing.T, ctx context.Context, tool fantasy.AgentTool, params UpdateGoalParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "goal", Name: UpdateGoalToolName, Input: string(input)})
	require.NoError(t, err)
	return response
}
