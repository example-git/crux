package session

import (
	"database/sql"
	"testing"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestContextOccupancyIncludesUnseenTokens(t *testing.T) {
	current := Session{PromptTokens: 100, CompletionTokens: 20, UnseenLocalTokens: 30}
	require.EqualValues(t, 150, current.ContextTokens())
	require.True(t, current.ContextEstimated())
	current.UnseenLocalTokens = 0
	require.EqualValues(t, 120, current.ContextTokens())
	require.False(t, current.ContextEstimated())
	current.EstimatedUsage = true
	require.True(t, current.ContextEstimated())
}

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
	created.Model = "stored-model"
	created.Provider = "stored-provider"
	created.PromptTokens = 100
	created.CompletionTokens = 50
	created.UnseenLocalTokens = 25
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
	require.EqualValues(t, 25, refetched.UnseenLocalTokens)
	require.EqualValues(t, 175, refetched.ContextTokens())
	require.Equal(t, created.Model, refetched.Model)
	require.Equal(t, created.Provider, refetched.Provider)
}

func TestEstimatedUsageStateSurvivesServiceRestart(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	created.Model = "stored-model"
	created.Provider = "stored-provider"
	created.EstimatedUsage = true
	created.UnseenLocalTokens = 25
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
	require.EqualValues(t, 25, restored.UnseenLocalTokens)
	require.EqualValues(t, 25, restored.ContextTokens())
	require.Equal(t, created.Model, restored.Model)
	require.Equal(t, created.Provider, restored.Provider)
}

func TestGoalLifecycleSurvivesServiceRestart(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	created, err := sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	created, err = sessions.SetGoal(t.Context(), created.ID, "finish the work")
	require.NoError(t, err)
	blocked, err := sessions.SetGoalStatus(t.Context(), created.ID, created.Goal.Generation, GoalStatusBlocked, "required service is unavailable")
	require.NoError(t, err)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	sessions = NewService(db.New(reopened), reopened)
	restored, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, blocked.Goal.Objective, restored.Goal.Objective)
	require.Equal(t, GoalStatusBlocked, restored.Goal.Status)
	require.Equal(t, blocked.Goal.Generation, restored.Goal.Generation)
	require.Equal(t, blocked.Goal.Error, restored.Goal.Error)
	require.NotZero(t, restored.Goal.UpdatedAt)

	replacement, err := sessions.SetGoal(t.Context(), created.ID, "replacement objective")
	require.NoError(t, err)
	require.Equal(t, "replacement objective", replacement.Goal.Objective)
	require.Equal(t, GoalStatusActive, replacement.Goal.Status)
	require.Equal(t, restored.Goal.Generation+1, replacement.Goal.Generation)
	require.Empty(t, replacement.Goal.Error)
}

func TestForkOriginSurvivesServiceRestart(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	source, err := sessions.Create(t.Context(), "source")
	require.NoError(t, err)
	forked, err := sessions.CreateFork(t.Context(), source.ID, "fork")
	require.NoError(t, err)
	require.Equal(t, source.ID, forked.ForkedFromSessionID)
	require.Empty(t, forked.ParentSessionID)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	restored, err := NewService(db.New(reopened), reopened).Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Equal(t, source.ID, restored.ForkedFromSessionID)
	require.Empty(t, restored.ParentSessionID)
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

	require.ErrorContains(t, sessions.SetMode(t.Context(), created.ID, ModePlanExecution), "invalid session mode")
	require.ErrorContains(t, sessions.SetPlanState(t.Context(), created.ID, ModePlanRevision, ""), "requires a plan")
	require.ErrorContains(t, sessions.SetPlanState(t.Context(), created.ID, ModeDefault, "retained"), "cannot retain a plan")
	require.ErrorContains(t, sessions.SetMode(t.Context(), created.ID, Mode("invalid")), "invalid session mode")
	unchanged, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModePlanExecution, unchanged.Mode)
	require.Equal(t, "Inspect, then implement", unchanged.Plan)

	require.NoError(t, sessions.SetMode(t.Context(), created.ID, ModeDefault))
	completed, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, ModeDefault, completed.Mode)
	require.Empty(t, completed.Plan)
}

func TestSessionAnalysisEffortIsPersistentAndIsolatedFromSave(t *testing.T) {
	dataDir := t.TempDir()
	t.Cleanup(func() {
		_ = db.Release(dataDir)
		db.ResetPool()
	})

	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)
	created, err := sessions.Create(t.Context(), "test")
	require.NoError(t, err)
	require.Empty(t, created.AnalysisEffort)

	require.NoError(t, sessions.SetAnalysisEffort(t.Context(), created.ID, "high"))
	created.Title = "stale save"
	saved, err := sessions.Save(t.Context(), created)
	require.NoError(t, err)
	require.Equal(t, "high", saved.AnalysisEffort)

	require.ErrorContains(t, sessions.SetAnalysisEffort(t.Context(), created.ID, "invalid"), "invalid analysis effort")
	unchanged, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "high", unchanged.AnalysisEffort)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()
	conn, err = db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions = NewService(db.New(conn), conn)
	restarted, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, "high", restarted.AnalysisEffort)

	require.NoError(t, sessions.SetAnalysisEffort(t.Context(), created.ID, ""))
	cleared, err := sessions.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Empty(t, cleared.AnalysisEffort)
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

func TestDeleteSubtreePersistsRemovalAndPreservesForks(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	root, err := sessions.Create(t.Context(), "root")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), "child", root.ID, "child")
	require.NoError(t, err)
	grandchild, err := sessions.CreateTaskSession(t.Context(), "grandchild", child.ID, "grandchild")
	require.NoError(t, err)
	forked, err := sessions.CreateFork(t.Context(), root.ID, "fork")
	require.NoError(t, err)

	events := sessions.Subscribe(t.Context())
	deleted, err := sessions.DeleteSubtree(t.Context(), root.ID)
	require.NoError(t, err)
	require.Equal(t, []string{grandchild.ID, child.ID, root.ID}, []string{deleted[0].ID, deleted[1].ID, deleted[2].ID})
	for _, expectedID := range []string{grandchild.ID, child.ID, root.ID} {
		event := <-events
		require.Equal(t, pubsub.DeletedEvent, event.Type)
		require.Equal(t, expectedID, event.Payload.ID)
	}
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		_, getErr := sessions.Get(t.Context(), id)
		require.ErrorIs(t, getErr, sql.ErrNoRows)
	}
	storedFork, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Equal(t, root.ID, storedFork.ForkedFromSessionID)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	sessions = NewService(db.New(reopened), reopened)
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		_, getErr := sessions.Get(t.Context(), id)
		require.ErrorIs(t, getErr, sql.ErrNoRows)
	}
	restoredFork, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.Equal(t, root.ID, restoredFork.ForkedFromSessionID)
}

func TestArchivePersistsDescendantStateAndPreservesForks(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := NewService(db.New(conn), conn)

	root, err := sessions.Create(t.Context(), "root")
	require.NoError(t, err)
	child, err := sessions.CreateTaskSession(t.Context(), "child", root.ID, "child")
	require.NoError(t, err)
	grandchild, err := sessions.CreateTaskSession(t.Context(), "grandchild", child.ID, "grandchild")
	require.NoError(t, err)
	forked, err := sessions.CreateFork(t.Context(), root.ID, "fork")
	require.NoError(t, err)

	events := sessions.Subscribe(t.Context())
	archived, err := sessions.Archive(t.Context(), root.ID)
	require.NoError(t, err)
	require.Len(t, archived, 3)
	require.Equal(t, []string{root.ID, grandchild.ID, child.ID}, []string{archived[0].ID, archived[1].ID, archived[2].ID})
	for _, expectedID := range []string{root.ID, grandchild.ID, child.ID} {
		event := <-events
		require.Equal(t, expectedID, event.Payload.ID)
		require.True(t, event.Payload.Archived)
	}

	storedRoot, err := sessions.Get(t.Context(), root.ID)
	require.NoError(t, err)
	require.True(t, storedRoot.Archived)
	storedFork, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.False(t, storedFork.Archived)
	active, err := sessions.List(t.Context())
	require.NoError(t, err)
	require.Len(t, active, 1)
	require.Equal(t, forked.ID, active[0].ID)
	last, err := sessions.GetLast(t.Context())
	require.NoError(t, err)
	require.Equal(t, forked.ID, last.ID)
	archivedRoots, err := sessions.ListArchived(t.Context())
	require.NoError(t, err)
	require.Len(t, archivedRoots, 1)
	require.Equal(t, root.ID, archivedRoots[0].ID)
	_, err = sessions.Archive(t.Context(), root.ID)
	require.ErrorIs(t, err, ErrAlreadyArchived)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	sessions = NewService(db.New(reopened), reopened)
	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		restored, restoreErr := sessions.Get(t.Context(), id)
		require.NoError(t, restoreErr)
		require.True(t, restored.Archived)
	}
	restoredFork, err := sessions.Get(t.Context(), forked.ID)
	require.NoError(t, err)
	require.False(t, restoredFork.Archived)

	unarchived, err := sessions.Unarchive(t.Context(), root.ID)
	require.NoError(t, err)
	require.False(t, unarchived.Archived)
	for _, id := range []string{child.ID, grandchild.ID} {
		restored, restoreErr := sessions.Get(t.Context(), id)
		require.NoError(t, restoreErr)
		require.True(t, restored.Archived)
	}
	_, err = sessions.Unarchive(t.Context(), root.ID)
	require.ErrorIs(t, err, ErrNotArchived)
	archivedRoots, err = sessions.ListArchived(t.Context())
	require.NoError(t, err)
	require.Empty(t, archivedRoots)
}

func TestStopGoalPersistsPausedGenerationAndRejectsStaleWrites(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	sessions := NewService(db.New(conn), conn)
	current, err := sessions.Create(t.Context(), "stop goal")
	require.NoError(t, err)
	current, err = sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	stopped, err := sessions.StopGoal(t.Context(), current.ID, current.Goal.Generation)
	require.NoError(t, err)
	require.Equal(t, current.Goal.Objective, stopped.Goal.Objective)
	require.Equal(t, GoalStatusPaused, stopped.Goal.Status)
	require.Equal(t, current.Goal.Generation+1, stopped.Goal.Generation)
	_, err = sessions.StopGoal(t.Context(), current.ID, current.Goal.Generation)
	require.ErrorIs(t, err, ErrGoalChanged)
	_, err = sessions.SetGoalStatus(t.Context(), current.ID, current.Goal.Generation, GoalStatusComplete, "")
	require.ErrorIs(t, err, ErrGoalChanged)
	restarted := NewService(db.New(conn), conn)
	persisted, err := restarted.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, stopped.Goal, persisted.Goal)
	_, err = restarted.SetGoalStatus(t.Context(), current.ID, stopped.Goal.Generation, GoalStatusActive, "")
	require.NoError(t, err)
}
