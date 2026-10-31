package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
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
	models := []byte(`{"large":{"model":{"provider":"fixture","model":"main"},"owner":{"provider_id":"fixture"}},"small":{"model":{"provider":"fixture","model":"small"},"owner":{"provider_id":"fixture"}}}`)
	current, err = sessions.SetModels(t.Context(), current.ID, current.ModelRevision, models)
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
	restored, err := restartedSessions.Get(t.Context(), current.ID)
	require.NoError(t, err)
	require.Equal(t, current.Models, restored.Models)

	started := make(chan struct{})
	scheduler := newGoalScheduler(&coordinator{sessions: restartedSessions})
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		require.Equal(t, current.ID, sessionID)
		close(started)
		<-ctx.Done()
		return nil, true, ctx.Err()
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
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		calls.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			close(canceled)
			return nil, true, ctx.Err()
		case <-release:
			return nil, true, nil
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
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		switch calls.Add(1) {
		case 1:
			close(firstStarted)
			<-ctx.Done()
			close(firstCanceled)
			return nil, true, ctx.Err()
		case 2:
			close(secondStarted)
			<-ctx.Done()
			return nil, true, ctx.Err()
		default:
			return nil, true, errors.New("unexpected extra goal turn")
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

func TestGoalSchedulerStartsFreshTurnAfterTransportFailure(t *testing.T) {
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
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		running := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if running <= observed || maximum.CompareAndSwap(observed, running) {
				break
			}
		}
		if calls.Add(1) == 1 {
			return nil, true, errors.New("websocket: close 1006 after partial response")
		}
		close(secondStarted)
		<-ctx.Done()
		return nil, true, ctx.Err()
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
		t.Fatal("transport failure did not start a fresh continuation turn")
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
	scheduler.runTurn = func(ctx context.Context, _ string) (*fantasy.AgentResult, bool, error) {
		started <- calls.Add(1)
		<-ctx.Done()
		canceled <- struct{}{}
		return nil, true, ctx.Err()
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

func TestGoalSchedulerReconciliationPreservesErrorBackoff(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)

	started := make(chan time.Time, 2)
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.reconcileInterval = time.Millisecond
	scheduler.errorInitialDelay = 200 * time.Millisecond
	scheduler.errorMaxDelay = 400 * time.Millisecond
	scheduler.runTurn = func(ctx context.Context, _ string) (*fantasy.AgentResult, bool, error) {
		started <- time.Now()
		if calls.Add(1) == 1 {
			return nil, true, errors.New("connection reset by peer")
		}
		<-ctx.Done()
		return nil, true, ctx.Err()
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})

	var first time.Time
	select {
	case first = <-started:
	case <-time.After(time.Second):
		t.Fatal("goal turn did not start")
	}
	for range 20 {
		scheduler.reconcile(t.Context(), current)
	}
	require.Never(t, func() bool { return calls.Load() > 1 }, 75*time.Millisecond, time.Millisecond)
	select {
	case second := <-started:
		require.GreaterOrEqual(t, second.Sub(first), scheduler.errorInitialDelay)
	case <-time.After(time.Second):
		t.Fatal("transient goal failure was not retried after backoff")
	}
}

func TestGoalSchedulerPauseInterruptsErrorBackoff(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	started := make(chan struct{}, 1)
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.errorInitialDelay = 5 * time.Second
	scheduler.runTurn = func(context.Context, string) (*fantasy.AgentResult, bool, error) {
		calls.Add(1)
		started <- struct{}{}
		return nil, true, errors.New("connection reset by peer")
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
	require.Eventually(t, func() bool {
		scheduler.mu.Lock()
		defer scheduler.mu.Unlock()
		return scheduler.workers[current.ID] == nil
	}, time.Second, time.Millisecond)
	require.Equal(t, int32(1), calls.Load())
}

func TestGoalSchedulerBlocksTerminalFailureUntilExplicitResume(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusTooManyRequests, http.StatusBadRequest} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			env := testEnv(t)
			current, err := env.sessions.Create(t.Context(), "goal session")
			require.NoError(t, err)
			current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
			require.NoError(t, err)
			var calls atomic.Int32
			resumed := make(chan struct{})
			scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
			scheduler.reconcileInterval = time.Millisecond
			scheduler.runTurn = func(ctx context.Context, _ string) (*fantasy.AgentResult, bool, error) {
				if calls.Add(1) == 1 {
					return nil, true, fmt.Errorf("wrapped: %w", &fantasy.ProviderError{StatusCode: status, Message: "private-provider-body"})
				}
				close(resumed)
				<-ctx.Done()
				return nil, true, ctx.Err()
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
			blocked, err := env.sessions.Get(t.Context(), current.ID)
			require.NoError(t, err)
			require.Equal(t, current.Goal.Generation, blocked.Goal.Generation)
			require.Equal(t, current.Goal.Objective, blocked.Goal.Objective)
			require.NotEmpty(t, blocked.Goal.Error)
			require.NotContains(t, blocked.Goal.Error, "private-provider-body")
			require.Never(t, func() bool { return calls.Load() > 1 }, 50*time.Millisecond, time.Millisecond)
			_, err = env.sessions.SetGoalStatus(t.Context(), current.ID, current.Goal.Generation, session.GoalStatusActive, "")
			require.NoError(t, err)
			select {
			case <-resumed:
			case <-time.After(time.Second):
				t.Fatal("blocked goal did not resume when explicitly requested")
			}
			require.Equal(t, int32(2), calls.Load())
		})
	}
}

type retryGoalBlockService struct {
	session.Service
	attempts atomic.Int32
}

func (s *retryGoalBlockService) BlockActiveGoal(ctx context.Context, sessionID string, generation int64, reason string) (session.Session, error) {
	if s.attempts.Add(1) < 3 {
		return session.Session{}, errors.New("database busy")
	}
	return s.Service.BlockActiveGoal(ctx, sessionID, generation, reason)
}

func TestGoalSchedulerRetriesBlockedStatePersistenceWithoutCallingModel(t *testing.T) {
	env := testEnv(t)
	current, err := env.sessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	sessions := &retryGoalBlockService{Service: env.sessions}
	var calls atomic.Int32
	scheduler := newGoalScheduler(&coordinator{sessions: sessions})
	scheduler.errorMaxDelay = 10 * time.Millisecond
	scheduler.reconcileInterval = time.Millisecond
	scheduler.runTurn = func(context.Context, string) (*fantasy.AgentResult, bool, error) {
		calls.Add(1)
		return nil, true, &fantasy.ProviderError{StatusCode: http.StatusTooManyRequests}
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
	require.Equal(t, int32(3), sessions.attempts.Load())
	require.Equal(t, int32(1), calls.Load())
}

func TestGoalBlockingReasonPreservesTransientFailuresAndLastStep(t *testing.T) {
	for _, runErr := range []error{
		nil,
		errors.New("connection reset by peer"),
		&fantasy.ProviderError{StatusCode: http.StatusRequestTimeout},
		&fantasy.ProviderError{StatusCode: http.StatusConflict},
		&fantasy.ProviderError{StatusCode: http.StatusServiceUnavailable},
		fantasy.NewServerOverloadError(),
	} {
		require.Empty(t, goalBlockingReason(nil, runErr))
	}
	for _, class := range []fantasy.ProviderErrorClass{
		fantasy.ProviderErrorClassAuthentication,
		fantasy.ProviderErrorClassAuthorization,
		fantasy.ProviderErrorClassRateLimit,
		fantasy.ProviderErrorClassContentFilter,
		fantasy.ProviderErrorClassContextOverflow,
		fantasy.ProviderErrorClassInvalidRequest,
	} {
		require.NotEmpty(t, goalBlockingReason(nil, &fantasy.ProviderError{Class: class}))
	}
	require.NotEmpty(t, goalBlockingReason(nil, &fantasy.ProviderError{AuthError: true}))
	result := &fantasy.AgentResult{
		Response: fantasy.Response{FinishReason: fantasy.FinishReasonStop},
		Steps: []fantasy.StepResult{
			{Response: fantasy.Response{FinishReason: fantasy.FinishReasonToolCalls}},
			{Response: fantasy.Response{FinishReason: fantasy.FinishReasonContentFilter}},
		},
	}
	require.NotEmpty(t, goalBlockingReason(result, nil))
	result.Steps[1].FinishReason = fantasy.FinishReasonStop
	require.Empty(t, goalBlockingReason(result, nil))
}

func TestGoalSchedulersShareExclusiveOwnershipAcrossCoordinators(t *testing.T) {
	dataDir := t.TempDir()
	conn, err := db.Connect(t.Context(), dataDir)
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, db.Release(dataDir))
		db.ResetPool()
	})
	firstSessions := session.NewService(db.New(conn), conn)
	secondSessions := session.NewService(db.New(conn), conn)
	current, err := firstSessions.Create(t.Context(), "goal session")
	require.NoError(t, err)
	current, err = firstSessions.SetGoal(t.Context(), current.ID, "finish the work")
	require.NoError(t, err)
	first := newGoalScheduler(&coordinator{sessions: firstSessions})
	second := newGoalScheduler(&coordinator{sessions: secondSessions})
	first.reconcileInterval = time.Millisecond
	second.reconcileInterval = time.Millisecond
	second.idleRetryDelay = time.Millisecond
	firstStarted := make(chan struct{})
	secondStarted := make(chan struct{})
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32
	var active atomic.Int32
	var maximum atomic.Int32
	runTurn := func(calls *atomic.Int32, started chan struct{}) func(context.Context, string) (*fantasy.AgentResult, bool, error) {
		return func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
			require.Equal(t, current.ID, sessionID)
			calls.Add(1)
			running := active.Add(1)
			defer active.Add(-1)
			for {
				observed := maximum.Load()
				if running <= observed || maximum.CompareAndSwap(observed, running) {
					break
				}
			}
			close(started)
			<-ctx.Done()
			return nil, true, ctx.Err()
		}
	}
	first.runTurn = runTurn(&firstCalls, firstStarted)
	second.runTurn = runTurn(&secondCalls, secondStarted)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		first.stop(ctx)
		second.stop(ctx)
	})
	first.start(t.Context())
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first coordinator did not start the goal")
	}
	second.start(t.Context())
	require.Never(t, func() bool { return secondCalls.Load() != 0 }, 75*time.Millisecond, time.Millisecond)
	require.Equal(t, int32(1), firstCalls.Load())
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	first.stop(ctx)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("goal ownership was not recovered after the first coordinator stopped")
	}
	require.Equal(t, int32(1), secondCalls.Load())
	require.Equal(t, int32(1), maximum.Load())
}

func TestGoalSchedulerOwnershipAllowsDifferentGoalsToRunConcurrently(t *testing.T) {
	env := testEnv(t)
	for _, title := range []string{"first goal", "second goal"} {
		current, err := env.sessions.Create(t.Context(), title)
		require.NoError(t, err)
		_, err = env.sessions.SetGoal(t.Context(), current.ID, "finish the work")
		require.NoError(t, err)
	}
	started := make(chan string, 2)
	scheduler := newGoalScheduler(&coordinator{sessions: env.sessions})
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		started <- sessionID
		<-ctx.Done()
		return nil, true, ctx.Err()
	}
	scheduler.start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		scheduler.stop(ctx)
	})
	seen := make(map[string]bool)
	for range 2 {
		select {
		case sessionID := <-started:
			seen[sessionID] = true
		case <-time.After(time.Second):
			t.Fatal("per-goal ownership blocked an unrelated goal")
		}
	}
	require.Len(t, seen, 2)
}
