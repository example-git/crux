package agent

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/lock"
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
	runTurn     func(context.Context, string) (*fantasy.AgentResult, bool, error)

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
	scheduler.runTurn = func(ctx context.Context, sessionID string) (*fantasy.AgentResult, bool, error) {
		accepted, ok := coordinator.currentAgent.BeginIdleAccepted(sessionID)
		if !ok {
			return nil, false, nil
		}
		defer accepted.Close()
		result, err := coordinator.run(ctx, accepted, true, sessionID, goalContinuationPrompt)
		return result, true, err
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
		if !active {
			select {
			case existing.wake <- struct{}{}:
			default:
			}
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
		release, err := s.coordinator.sessions.TryAcquireGoal(ctx, sessionID)
		if err == nil {
			defer release()
			break
		}
		if ctx.Err() != nil {
			return
		}
		if !errors.Is(err, lock.ErrContended) {
			slog.Error("Failed to acquire goal ownership", "sessionID", sessionID, "error", err)
			s.blockGoal(ctx, sessionID, worker.generation, "Exclusive goal ownership could not be acquired. Resolve the storage error before resuming the goal.")
			return
		}
		if !s.wait(ctx, worker.wake, s.idleRetryDelay) {
			return
		}
	}
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

		result, admitted, runErr := s.runTurn(ctx, sessionID)
		if ctx.Err() != nil {
			return
		}
		if admitted {
			if reason := goalBlockingReason(result, runErr); reason != "" {
				s.blockGoal(ctx, sessionID, worker.generation, reason)
				return
			}
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

func goalBlockingReason(result *fantasy.AgentResult, runErr error) string {
	if runErr == nil {
		if result == nil {
			return ""
		}
		finishReason := result.Response.FinishReason
		if len(result.Steps) > 0 {
			finishReason = result.Steps[len(result.Steps)-1].FinishReason
		}
		if finishReason == fantasy.FinishReasonContentFilter {
			return "The provider refused the goal continuation. Review the request before resuming the goal."
		}
		return ""
	}
	var providerErr *fantasy.ProviderError
	if errors.As(runErr, &providerErr) {
		switch {
		case providerErr.AuthError || providerErr.Class == fantasy.ProviderErrorClassAuthentication || providerErr.StatusCode == http.StatusUnauthorized:
			return "Provider authentication failed. Sign in again before resuming the goal."
		case providerErr.Class == fantasy.ProviderErrorClassAuthorization || providerErr.StatusCode == http.StatusForbidden:
			return "The provider denied access to the selected model. Resolve access before resuming the goal."
		case providerErr.Class == fantasy.ProviderErrorClassRateLimit || providerErr.StatusCode == http.StatusTooManyRequests:
			return "The provider rate limit was reached. Wait for the limit to reset before resuming the goal."
		case providerErr.Class == fantasy.ProviderErrorClassContentFilter:
			return "The provider refused the goal continuation. Review the request before resuming the goal."
		case providerErr.Class == fantasy.ProviderErrorClassContextOverflow || providerErr.IsContextTooLarge():
			return "The provider could not accept the conversation context. Resolve the context error before resuming the goal."
		case providerErr.Class == fantasy.ProviderErrorClassInvalidRequest || providerErr.StatusCode >= http.StatusBadRequest && providerErr.StatusCode < http.StatusInternalServerError && !providerErr.IsRetryable():
			return "The provider rejected the request. Resolve the provider error before resuming the goal."
		}
	}
	var refusal interface{ NonRetryable() bool }
	var sessionModelErr *sessionModelSelectionError
	if errors.As(runErr, &sessionModelErr) {
		return "The session model selection is unavailable. Select the intended session models before resuming the goal."
	}
	if errors.As(runErr, &refusal) && refusal.NonRetryable() {
		return "A non-retryable error stopped the goal continuation. Resolve the error before resuming the goal."
	}
	return ""
}

func (s *goalScheduler) blockGoal(ctx context.Context, sessionID string, generation int64, reason string) {
	for {
		_, err := s.coordinator.sessions.BlockActiveGoal(ctx, sessionID, generation, reason)
		if err == nil {
			slog.Warn("Goal continuation blocked", "sessionID", sessionID, "reason", reason)
			return
		}
		if ctx.Err() != nil || errors.Is(err, session.ErrGoalChanged) {
			return
		}
		slog.Error("Failed to persist blocked goal", "sessionID", sessionID, "error", err)
		if !s.wait(ctx, nil, s.errorMaxDelay) {
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
