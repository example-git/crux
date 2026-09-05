package session

import (
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/stretchr/testify/require"
)

func TestEstimatedUsageStateSurvivesFetchModifySave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	fetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, fetched.EstimatedUsage)

	fetched.Todos = []Todo{{
		Content:    "Check estimate state",
		Status:     TodoStatusInProgress,
		ActiveForm: "Checking estimate state",
	}}

	updated, err := sessions.Save(t.Context(), fetched)
	require.NoError(t, err)
	require.True(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, refetched.EstimatedUsage)
}

func TestEstimatedUsageStateSurvivesServiceRestart(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.EstimatedUsage = true
	_, err = sessions.Save(t.Context(), created)
	require.NoError(t, err)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	restored, err := NewService(db.New(reopened), reopened).Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.True(t, restored.EstimatedUsage)
}

func TestUpdateTitleAndCostPreservesActiveOccupancy(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.Cost = 1.5
	_, err = sessions.Save(t.Context(), created)
	require.NoError(t, err)

	require.NoError(t, sessions.UpdateTitleAndCost(t.Context(), created.ID, "generated", 0.25))
	updated, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "generated", updated.Title)
	require.Equal(t, int64(100), updated.PromptTokens)
	require.Equal(t, int64(50), updated.CompletionTokens)
	require.Equal(t, 1.75, updated.Cost)
}

func TestSessionPlanLifecycleIsPersistentAndIsolatedFromSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	require.Equal(t, ModeDefault, created.Mode)
	require.Empty(t, created.Plan)

	require.NoError(t, sessions.SetMode(t.Context(), created.ID, ModePlan))
	planned, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModePlan, planned.Mode)
	require.True(t, planned.Mode.IsReadOnlyPlan())

	require.NoError(t, sessions.SetPlanState(t.Context(), created.ID, ModePlanRevision, "Inspect first"))
	revision, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModePlanRevision, revision.Mode)
	require.Equal(t, "Inspect first", revision.Plan)
	require.True(t, revision.Mode.IsReadOnlyPlan())

	require.NoError(t, sessions.SetPlanState(t.Context(), created.ID, ModePlanExecution, "Inspect, then implement"))
	executing, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModePlanExecution, executing.Mode)
	require.Equal(t, "Inspect, then implement", executing.Plan)
	require.True(t, executing.Mode.IsPlan())
	require.False(t, executing.Mode.IsReadOnlyPlan())

	created.Title = "stale save"
	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.Equal(t, ModePlanExecution, saved.Mode)
	require.Equal(t, "Inspect, then implement", saved.Plan)

	require.ErrorContains(t, sessions.SetMode(t.Context(), created.ID, ModeDefault), "can only end after completion approval")
	require.ErrorContains(t, sessions.SetMode(t.Context(), created.ID, ModePlanExecution), "invalid session mode")
	require.ErrorContains(t, sessions.SetPlanState(t.Context(), created.ID, ModePlanRevision, ""), "requires a plan")
	require.ErrorContains(t, sessions.SetPlanState(t.Context(), created.ID, ModeDefault, "retained"), "cannot retain a plan")
	require.ErrorContains(t, sessions.SetMode(t.Context(), created.ID, Mode("invalid")), "invalid session mode")
	unchanged, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModePlanExecution, unchanged.Mode)
	require.Equal(t, "Inspect, then implement", unchanged.Plan)

	require.NoError(t, sessions.SetPlanState(t.Context(), created.ID, ModeDefault, ""))
	completed, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModeDefault, completed.Mode)
	require.Empty(t, completed.Plan)
}

func TestEstimatedUsageStateCanBeClearedByExplicitSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)

	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.EstimatedUsage = true

	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.True(t, saved.EstimatedUsage)

	saved.EstimatedUsage = false
	updated, err := sessions.Save(t.Context(), saved)
	require.NoError(t, err)
	require.False(t, updated.EstimatedUsage)

	refetched, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.False(t, refetched.EstimatedUsage)
}
