package agent

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/db"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestGoalSchedulerRecoversPersistedActiveGoalAtStartup(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	sessions := session.NewService(db.New(conn), conn)
	current, err := sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)

	require.NoError(t, db.Release(dataDir))
	db.ResetPool()

	reopened, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	restartedSessions := session.NewService(db.New(reopened), reopened)

	started := make(chan struct{})
	scheduler := newGoalScheduler(&coordinator{sessions: restartedSessions})
	scheduler.runTurn = func(ctx context.Context, sessionID string) (bool, error) {
		require.Equal(t, current.ID, sessionID)
		close(started)
		<-ctx.Done()
		return true, ctx.Err()
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("persisted active goal was not resumed after service restart")
	}
}

func TestGoalSchedulerPauseDoesNotCancelInflightTurn(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)

	started := make(chan struct{})
	release := make(chan struct{})
	canceled := make(chan struct{})
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.runTurn = func(ctx context.Context, sessionID string) (bool, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return true, ctx.Err()
		case <-release:
			return true, nil
		}
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal turn did not start")
	}
	_, err = env.sessions.SetGoalStatus(t.Context(), current.ID, current.Goal.Generation, session.GoalStatusPaused, "")
	require.NoError(t, err)

	select {
	case <-canceled:
		t.Fatal("pausing a goal canceled the in-flight turn")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return scheduler.workers[current.ID] == nil
	}, time.Second, 10*time.Millisecond)
	require.Equal(t, int32(1), calls.Load())
}

func TestGoalSchedulerReplacementCancelsStaleGeneration(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "first objective")
	require.NoError(t, err)

	firstStarted := make(chan struct{})
	firstCanceled := make(chan struct{})
	secondStarted := make(chan struct{})
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.runTurn = func(ctx context.Context, sessionID string) (bool, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return true, ctx.Err()
		case 2:
			close(secondStarted)
			<-ctx.Done()
			return true, ctx.Err()
		default:
			return true, errors.New("unexpected extra goal turn")
		}
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first goal generation did not start")
	}
	replacement, err := env.sessions.SetGoal(t.Context(), current.ID, "replacement objective")
	require.NoError(t, err)
	require.Greater(t, replacement.Goal.Generation, current.Goal.Generation)

	select {
	case <-firstCanceled:
	case <-time.After(time.Second):
		t.Fatal("stale goal generation was not canceled")
	}
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("replacement goal generation did not start")
	}
}

func TestGoalSchedulerStartsFreshTurnAfterTerminalFailure(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	_, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)

	secondStarted := make(chan struct{})
	var calls atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.errorInitialDelay = time.Millisecond
	scheduler.errorMaxDelay = time.Millisecond
	scheduler.runTurn = func(ctx context.Context, sessionID string) (bool, error) {
		running := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if running <= observed || maximum.CompareAndSwap(observed, running) {
				break
			}
		}
		if calls.Add(1) == 1 {
			return true, errors.New("websocket: close 1006 after partial response")
		}
		close(secondStarted)
		<-ctx.Done()
		return true, ctx.Err()
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})

	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("terminal failure did not start a fresh continuation turn")
	}
	require.Equal(t, int32(2), calls.Load())
	require.Equal(t, int32(1), maximum.Load())
}

func TestGoalSchedulerStopCancelsInflightTurnAndCanResume(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	started := make(chan int32, 4)
	canceled := make(chan struct{}, 4)
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.reconcileInterval = 10 * time.Millisecond
	scheduler.runTurn = func(ctx context.Context, _ string) (bool, error) {
		started <- calls.Add(1)
		<-ctx.Done()
		canceled <- struct{}{}
		return true, ctx.Err()
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("goal turn did not start")
	}
	stopped, err := env.sessions.StopGoal(t.Context(), current.ID, current.Goal.Generation)
	require.NoError(t, err)
	require.Equal(t, session.GoalStatusPaused, stopped.Goal.Status)
	require.Equal(t, current.Goal.Objective, stopped.Goal.Objective)
	require.Greater(t, stopped.Goal.Generation, current.Goal.Generation)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("stopped goal did not cancel its in-flight turn")
	}
	require.Never(t, func() bool { return calls.Load() != 1 }, 100*time.Millisecond, time.Millisecond)
	_, err = env.sessions.SetGoalStatus(t.Context(), current.ID, current.Goal.Generation, session.GoalStatusActive, "")
	require.ErrorIs(t, err, session.ErrGoalChanged)
	_, err = env.sessions.SetGoalStatus(t.Context(), current.ID, stopped.Goal.Generation, session.GoalStatusActive, "")
	require.NoError(t, err)
	select {
	case count := <-started:
		require.Equal(t, int32(2), count)
	case <-time.After(time.Second):
		t.Fatal("stopped goal did not resume when explicitly requested")
	}
}
