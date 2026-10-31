package agent

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/session"
)

const (
	goalContinuationPrompt = "Continue working toward the active goal from the current conversation and workspace state."
	goalIdleRetryDelay     = 250 * time.Millisecond
	goalSuccessDelay       = 100 * time.Millisecond
	goalErrorInitialDelay  = 500 * time.Millisecond
	goalErrorMaxDelay      = 30 * time.Second
	goalReconcileInterval  = 2 * time.Second
)

type goalWorker struct {
	generation int64
	cancel     context.CancelFunc
	wake       chan struct{}
}

type goalScheduler struct {
	coordinator *coordinator
	runTurn     func(context.Context, string) (bool, error)

	idleRetryDelay    time.Duration
	successDelay      time.Duration
	errorInitialDelay time.Duration
	errorMaxDelay     time.Duration
	reconcileInterval time.Duration

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	workers map[string]*goalWorker
	work    sync.WaitGroup
}

func newGoalScheduler(coordinator *coordinator) *goalScheduler {
	scheduler := &goalScheduler{
		coordinator:       coordinator,
		idleRetryDelay:    goalIdleRetryDelay,
		successDelay:      goalSuccessDelay,
		errorInitialDelay: goalErrorInitialDelay,
		errorMaxDelay:     goalErrorMaxDelay,
		reconcileInterval: goalReconcileInterval,
		workers:           make(map[string]*goalWorker),
	}
	scheduler.runTurn = func(ctx context.Context, sessionID string) (bool, error) {
		accepted, ok := coordinator.currentAgent.BeginIdleAccepted(sessionID)
		if !ok {
			return false, nil
		}
		defer accepted.Close()
		_, err := coordinator.run(ctx, accepted, true, sessionID, goalContinuationPrompt)
		return true, err
	}
	return scheduler
}

func (s *goalScheduler) start(parent context.Context) {
	s.mu.Lock()
	if s.cancel != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	s.ctx = ctx
	s.cancel = cancel
	s.done = done
	s.mu.Unlock()

	events := s.coordinator.sessions.Subscribe(ctx)
	go s.loop(ctx, done, events)
}

func (s *goalScheduler) loop(ctx context.Context, done chan struct{}, events <-chan pubsub.Event[session.Session]) {
	defer close(done)
	s.reconcileAll(ctx)
	ticker := time.NewTicker(s.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				return
			}
			s.reconcile(ctx, event.Payload)
		case <-ticker.C:
			s.reconcileAll(ctx)
		}
	}
}

func (s *goalScheduler) reconcileAll(ctx context.Context) {
	sessions, err := s.coordinator.sessions.List(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("Failed to reconcile active goals", "error", err)
		}
		return
	}
	for _, current := range sessions {
		s.reconcile(ctx, current)
	}
}

func (s *goalScheduler) reconcile(ctx context.Context, current session.Session) {
	active := !current.Archived && current.Goal.Active() && !current.Mode.IsPlan()

	s.mu.Lock()
	existing := s.workers[current.ID]
	if existing != nil && existing.generation == current.Goal.Generation {
		select {
		case existing.wake <- struct{}{}:
		default:
		}
		s.mu.Unlock()
		return
	}
	if existing != nil {
		existing.cancel()
		delete(s.workers, current.ID)
	}
	if !active || ctx.Err() != nil {
		s.mu.Unlock()
		return
	}
	workerCtx, cancel := context.WithCancel(ctx)
	worker := &goalWorker{
		generation: current.Goal.Generation,
		cancel:     cancel,
		wake:       make(chan struct{}, 1),
	}
	s.workers[current.ID] = worker
	s.work.Add(1)
	s.mu.Unlock()

	go s.run(workerCtx, current.ID, worker)
}

func (s *goalScheduler) run(ctx context.Context, sessionID string, worker *goalWorker) {
	defer s.work.Done()
	defer s.finish(sessionID, worker)
	errorDelay := s.errorInitialDelay
	for {
		current, err := s.coordinator.sessions.Get(ctx, sessionID)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("Failed to load active goal", "sessionID", sessionID, "error", err)
			}
			return
		}
		if current.Archived || current.Goal.Generation != worker.generation || !current.Goal.Active() || current.Mode.IsPlan() {
			return
		}

		admitted, runErr := s.runTurn(ctx, sessionID)
		if ctx.Err() != nil {
			return
		}
		delay := s.idleRetryDelay
		if admitted && runErr == nil {
			delay = s.successDelay
			errorDelay = s.errorInitialDelay
		} else if admitted {
			delay = errorDelay
			errorDelay = min(errorDelay*2, s.errorMaxDelay)
			if !errors.Is(runErr, context.Canceled) {
				slog.Warn("Goal continuation failed; scheduling a fresh turn", "sessionID", sessionID, "error", runErr, "retryIn", delay)
			}
		}
		if !s.wait(ctx, worker.wake, delay) {
			return
		}
	}
}

func (s *goalScheduler) wait(ctx context.Context, wake <-chan struct{}, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-wake:
		return true
	case <-timer.C:
		return true
	}
}

func (s *goalScheduler) finish(sessionID string, worker *goalWorker) {
	s.mu.Lock()
	if s.workers[sessionID] == worker {
		delete(s.workers, sessionID)
	}
	s.mu.Unlock()
}

func (s *goalScheduler) stop(ctx context.Context) {
	s.mu.Lock()
	cancel := s.cancel
	done := s.done
	if cancel == nil {
		s.mu.Unlock()
		return
	}
	s.cancel = nil
	s.done = nil
	cancel()
	s.mu.Unlock()

	select {
	case <-done:
	case <-ctx.Done():
		return
	}
	workDone := make(chan struct{})
	go func() {
		s.work.Wait()
		close(workDone)
	}()
	select {
	case <-workDone:
	case <-ctx.Done():
	}
}
