package app

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestSessionModelsValidateOwnersAndPreserveGoal(t *testing.T) {
	app, models := newAgentRecoveryTestApp(t)
	conn, err := db.Connect(t.Context(), t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	app.Sessions = session.NewService(db.New(conn), conn)
	current, err := app.Sessions.Create(t.Context(), "session")
	require.NoError(t, err)
	current, err = app.Sessions.SetGoal(t.Context(), current.ID, "finish")
	require.NoError(t, err)
	current, err = app.Sessions.SetGoalStatus(t.Context(), current.ID, current.Goal.Generation, session.GoalStatusPaused, "")
	require.NoError(t, err)
	goal := current.Goal
	current, err = app.SetSessionModels(t.Context(), current.ID, 0, models)
	require.NoError(t, err)
	require.Equal(t, goal, current.Goal)
	require.EqualValues(t, 1, current.ModelRevision)
	var actual map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(current.Models, &actual))
	require.Contains(t, actual, "large")
	require.Contains(t, actual, "small")
	_, err = app.SetSessionModels(t.Context(), current.ID, 0, models)
	require.ErrorIs(t, err, session.ErrModelsChanged)
	models.Large.Owner.AccountNamespace = "different-owner"
	_, err = app.SetSessionModels(t.Context(), current.ID, 1, models)
	require.ErrorContains(t, err, "owner is unavailable")
	stored, err := app.Sessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, current, stored)
}
