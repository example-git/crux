package agent

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestBackgroundAgentManagerStopAllRejectsDelayedStart(t *testing.T) {
	for _, approved := range []bool{false, true} {
		name := "ordinary"
		if approved {
			name = "approved"
		}
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			manager, records := backgroundAgentStartTestManager(t, "closing", directory)
			peer, _ := backgroundAgentStartTestManager(t, "retained", directory)
			pending := reserveBackgroundAgentStartTestTask(t, manager)
			retained := reserveBackgroundAgentStartTestTask(t, peer)
			peerStarted := make(chan struct{})
			peerRelease := make(chan struct{})
			releasePeer := sync.OnceFunc(func() { close(peerRelease) })
			defer releasePeer()
			var peerCanceled atomic.Bool
			require.NoError(t, peer.Start(retained, "retained-child", func(ctx context.Context) backgroundAgentResult {
				close(peerStarted)
				select {
				case <-ctx.Done():
					peerCanceled.Store(true)
					return backgroundAgentResult{Err: ctx.Err()}
				case <-peerRelease:
					return backgroundAgentResult{Output: "retained result"}
				}
			}))
			awaitBackgroundAgentStartTestSignal(t, peerStarted)

			// StopAll has closed the manager before requestStop persists this
			// pending reservation. Delay that real transition while a late
			// approval/session-creation result tries to launch the task.
			stopEntered := make(chan struct{})
			stopRelease := make(chan struct{})
			releaseStop := sync.OnceFunc(func() { close(stopRelease) })
			defer releaseStop()
			var pauseStop sync.Once
			persist := pending.persist
			pending.persist = func(task *BackgroundAgentTask) error {
				if !task.state.StopRequestedAt.IsZero() {
					pauseStop.Do(func() {
						close(stopEntered)
						<-stopRelease
					})
				}
				return persist(task)
			}
			stopCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			stopped := make(chan struct{})
			go func() {
				manager.StopAll(stopCtx)
				close(stopped)
			}()
			awaitBackgroundAgentStartTestSignal(t, stopEntered)
			var called atomic.Bool
			startResult := make(chan error, 1)
			go func() {
				start := manager.Start
				if approved {
					start = manager.StartApproved
				}
				startResult <- start(pending, "late-child", func(context.Context) backgroundAgentResult {
					called.Store(true)
					return backgroundAgentResult{Output: "must not run"}
				})
			}()
			releaseStop()
			var startErr error
			select {
			case err := <-startResult:
				startErr = err
			case <-stopCtx.Done():
				t.Fatal("delayed start did not return")
			}
			awaitBackgroundAgentStartTestSignal(t, stopped)
			awaitBackgroundAgentStartTestSignal(t, pending.executionDone)
			require.ErrorContains(t, startErr, "manager is closed")
			require.False(t, called.Load())
			info := pending.Info()
			require.Equal(t, managedtask.StatusKilled, info.State.Status)
			require.True(t, info.State.Interrupted)
			require.True(t, info.State.StartedAt.IsZero())
			require.Empty(t, info.ChildSessionID)
			require.Zero(t, manager.ActiveCount())
			record, err := records.Get(pending.ID)
			require.NoError(t, err)
			require.Equal(t, managedtask.StateToRecord(info.State), record.State)
			require.Empty(t, record.Agent.ChildSessionID)
			manager.FailReservation(pending, errors.New("late session creation failure"))
			require.Equal(t, info, pending.Info())

			require.False(t, peerCanceled.Load())
			require.Equal(t, 1, peer.ActiveCount())
			releasePeer()
			awaitBackgroundAgentStartTestSignal(t, retained.executionDone)
			require.Equal(t, managedtask.StatusCompleted, retained.Info().State.Status)
			require.Equal(t, "retained result", retained.Info().FinalOutput)
			require.False(t, peerCanceled.Load())
		})
	}
}

func TestBackgroundAgentManagerStopPendingPreservesOtherTasks(t *testing.T) {
	manager, _ := backgroundAgentStartTestManager(t, "workspace", t.TempDir())
	pending := reserveBackgroundAgentStartTestTask(t, manager)
	running := reserveBackgroundAgentStartTestTask(t, manager)
	runningCtx := make(chan context.Context, 1)
	release := make(chan struct{})
	releaseRun := sync.OnceFunc(func() { close(release) })
	defer releaseRun()
	require.NoError(t, manager.Start(running, "running-child", func(ctx context.Context) backgroundAgentResult {
		runningCtx <- ctx
		<-release
		return backgroundAgentResult{Output: "normal completion"}
	}))
	ctx := <-runningCtx
	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	info, err := manager.Stop(stopCtx, pending.ID)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusKilled, info.State.Status)
	require.True(t, info.State.StartedAt.IsZero())
	awaitBackgroundAgentStartTestSignal(t, pending.executionDone)
	require.ErrorContains(t, manager.StartApproved(pending, "late-child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "must not run"}
	}), "no longer pending")
	manager.FailReservation(pending, errors.New("late failure"))
	require.Equal(t, info, pending.Info())
	require.NoError(t, ctx.Err(), "stopping one reservation must preserve independent work")
	require.Equal(t, 1, manager.ActiveCount())
	releaseRun()
	awaitBackgroundAgentStartTestSignal(t, running.executionDone)
	require.Equal(t, managedtask.StatusCompleted, running.Info().State.Status)

	// A stopped reservation releases its slot without closing the manager.
	next := reserveBackgroundAgentStartTestTask(t, manager)
	require.NoError(t, manager.StartApproved(next, "next-child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "next result"}
	}))
	awaitBackgroundAgentStartTestSignal(t, next.executionDone)
	require.Equal(t, managedtask.StatusCompleted, next.Info().State.Status)
}

func TestBackgroundAgentManagerReservationHasOneExecution(t *testing.T) {
	manager, _ := backgroundAgentStartTestManager(t, "workspace", t.TempDir())
	peer, _ := backgroundAgentStartTestManager(t, "peer", t.TempDir())
	task := reserveBackgroundAgentStartTestTask(t, manager)
	var calls atomic.Int32
	started := make(chan struct{})
	run := func(ctx context.Context) backgroundAgentResult {
		calls.Add(1)
		close(started)
		<-ctx.Done()
		return backgroundAgentResult{Err: ctx.Err()}
	}
	require.ErrorContains(t, peer.StartApproved(task, "wrong-manager", run), "does not belong")
	require.NoError(t, manager.Start(task, "child", run))
	awaitBackgroundAgentStartTestSignal(t, started)
	require.ErrorContains(t, manager.StartApproved(task, "second-child", run), "no longer pending")
	manager.FailReservation(task, errors.New("late failure after launch"))
	require.Equal(t, managedtask.StatusRunning, task.Info().State.Status)
	stopCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	manager.StopAll(stopCtx)
	awaitBackgroundAgentStartTestSignal(t, task.executionDone)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, managedtask.StatusKilled, task.Info().State.Status)
	require.Equal(t, "child", task.Info().ChildSessionID)
	require.Zero(t, manager.ActiveCount())
}

func TestBackgroundAgentManagerFailedReservationCannotStart(t *testing.T) {
	manager, _ := backgroundAgentStartTestManager(t, "workspace", t.TempDir())
	task := reserveBackgroundAgentStartTestTask(t, manager)
	manager.FailReservation(task, errors.New("session creation failed"))
	before := task.Info()
	require.Equal(t, managedtask.StatusFailed, before.State.Status)
	manager.FailReservation(task, errors.New("duplicate failure"))
	require.ErrorContains(t, manager.Start(task, "late-child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "must not run"}
	}), "no longer pending")
	awaitBackgroundAgentStartTestSignal(t, task.executionDone)
	require.Equal(t, before, task.Info())
	require.Zero(t, manager.ActiveCount())
}

func TestBackgroundAgentManagerStartPersistenceFailureCannotRestart(t *testing.T) {
	manager, records := backgroundAgentStartTestManager(t, "workspace", t.TempDir())
	task := reserveBackgroundAgentStartTestTask(t, manager)
	require.NoError(t, records.Close())
	var called atomic.Bool
	run := func(context.Context) backgroundAgentResult {
		called.Store(true)
		return backgroundAgentResult{Output: "must not run"}
	}
	require.Error(t, manager.StartApproved(task, "child", run))
	awaitBackgroundAgentStartTestSignal(t, task.executionDone)
	before := task.Info()
	require.Equal(t, managedtask.StatusFailed, before.State.Status)
	require.False(t, called.Load())
	require.ErrorContains(t, manager.Start(task, "late-child", run), "no longer pending")
	manager.FailReservation(task, errors.New("late failure"))
	require.Equal(t, before, task.Info())
	require.Zero(t, manager.ActiveCount())
}

func backgroundAgentStartTestManager(t *testing.T, workspaceID, admissionDirectory string) (*BackgroundAgentManager, *managedtask.Store) {
	t.Helper()
	records, err := managedtask.NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	manager, err := NewBackgroundAgentManagerWithAdmissionDirectory(workspaceID, nil, records, admissionDirectory)
	require.NoError(t, err)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		manager.StopAll(ctx)
		require.NoError(t, records.Close())
	})
	return manager, records
}

func reserveBackgroundAgentStartTestTask(t *testing.T, manager *BackgroundAgentManager) *BackgroundAgentTask {
	t.Helper()
	task, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	return task
}

func awaitBackgroundAgentStartTestSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for background agent transition")
	}
}
