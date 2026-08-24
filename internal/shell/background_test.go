package shell

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestBackgroundShellManager_Start(t *testing.T) {
	t.Skip("Skipping this until I figure out why its flaky")
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'hello world'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	if bgShell.ID == "" {
		t.Error("expected shell ID to be non-empty")
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, err := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}

	if !strings.Contains(stdout, "hello world") {
		t.Errorf("expected stdout to contain 'hello world', got: %s", stdout)
	}

	if stderr != "" {
		t.Errorf("expected empty stderr, got: %s", stderr)
	}
}

func TestBackgroundShellManager_Get(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'test'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Retrieve the shell
	retrieved, ok := manager.Get(bgShell.ID)
	if !ok {
		t.Error("expected to find the background shell")
	}

	if retrieved.ID != bgShell.ID {
		t.Errorf("expected shell ID %s, got %s", bgShell.ID, retrieved.ID)
	}

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShellManager_Kill(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a long-running command
	bgShell, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Kill it
	err = manager.Kill(bgShell.ID)
	if err != nil {
		t.Errorf("failed to kill background shell: %v", err)
	}

	// Verify it's no longer in the manager
	_, ok := manager.Get(bgShell.ID)
	if ok {
		t.Error("expected shell to be removed after kill")
	}

	// Verify the shell is done
	if !bgShell.IsDone() {
		t.Error("expected shell to be done after kill")
	}
}

func TestBackgroundShellManager_KillNonExistent(t *testing.T) {
	t.Parallel()

	manager := newBackgroundShellManager()

	err := manager.Kill("non-existent-id")
	if err == nil {
		t.Error("expected error when killing non-existent shell")
	}
}

func TestBackgroundShell_IsDone(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	bgShell, err := manager.Start(ctx, workingDir, nil, "echo 'quick'", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete (Windows is slower to spin up).
	require.Eventually(t, bgShell.IsDone, 5*time.Second, 50*time.Millisecond, "expected shell to be done")

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShell_WithBlockFuncs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	blockFuncs := []BlockFunc{
		CommandsBlocker([]string{"curl", "wget"}),
	}

	bgShell, err := manager.Start(ctx, workingDir, blockFuncs, "curl example.com", "")
	if err != nil {
		t.Fatalf("failed to start background shell: %v", err)
	}

	// Wait for the command to complete
	bgShell.Wait()

	stdout, stderr, done, execErr := bgShell.GetOutput()
	if !done {
		t.Error("expected shell to be done")
	}

	// The command should have been blocked
	output := stdout + stderr
	if !strings.Contains(output, "not allowed") && execErr == nil {
		t.Errorf("expected command to be blocked, got stdout: %s, stderr: %s, err: %v", stdout, stderr, execErr)
	}

	// Clean up
	manager.Kill(bgShell.ID)
}

func TestBackgroundShellManager_List(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("skipping flacky test on windows")
	}

	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start two shells
	bgShell1, err := manager.Start(ctx, workingDir, nil, "sleep 1", "")
	if err != nil {
		t.Fatalf("failed to start first background shell: %v", err)
	}

	bgShell2, err := manager.Start(ctx, workingDir, nil, "sleep 1", "")
	if err != nil {
		t.Fatalf("failed to start second background shell: %v", err)
	}

	ids := manager.List()

	// Check that both shells are in the list
	found1 := false
	found2 := false
	for _, id := range ids {
		if id == bgShell1.ID {
			found1 = true
		}
		if id == bgShell2.ID {
			found2 = true
		}
	}

	if !found1 {
		t.Errorf("expected to find shell %s in list", bgShell1.ID)
	}
	if !found2 {
		t.Errorf("expected to find shell %s in list", bgShell2.ID)
	}

	// Clean up
	manager.Kill(bgShell1.ID)
	manager.Kill(bgShell2.ID)
}

func TestBackgroundShellManager_KillAll(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start multiple long-running shells
	shell1, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 1: %v", err)
	}

	shell2, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 2: %v", err)
	}

	shell3, err := manager.Start(ctx, workingDir, nil, "sleep 10", "")
	if err != nil {
		t.Fatalf("failed to start shell 3: %v", err)
	}

	// Verify shells are running
	if shell1.IsDone() || shell2.IsDone() || shell3.IsDone() {
		t.Error("shells should not be done yet")
	}

	// Kill all shells
	manager.KillAll(t.Context())

	// Verify all shells are done
	if !shell1.IsDone() {
		t.Error("shell1 should be done after KillAll")
	}
	if !shell2.IsDone() {
		t.Error("shell2 should be done after KillAll")
	}
	if !shell3.IsDone() {
		t.Error("shell3 should be done after KillAll")
	}

	// Verify they're removed from the manager
	if _, ok := manager.Get(shell1.ID); ok {
		t.Error("shell1 should be removed from manager")
	}
	if _, ok := manager.Get(shell2.ID); ok {
		t.Error("shell2 should be removed from manager")
	}
	if _, ok := manager.Get(shell3.ID); ok {
		t.Error("shell3 should be removed from manager")
	}

	// Verify list is empty (or doesn't contain our shells)
	ids := manager.List()
	for _, id := range ids {
		if id == shell1.ID || id == shell2.ID || id == shell3.ID {
			t.Errorf("shell %s should not be in list after KillAll", id)
		}
	}
}

func TestBackgroundShellManager_KillAll_Timeout(t *testing.T) {
	t.Parallel()

	// XXX: can't use synctest here - causes --race to trip.

	workingDir := t.TempDir()
	manager := newBackgroundShellManager()

	// Start a shell that traps signals and ignores cancellation.
	_, err := manager.Start(t.Context(), workingDir, nil, "trap '' TERM INT; sleep 60", "")
	require.NoError(t, err)

	// Short timeout to test the timeout path.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	t.Cleanup(cancel)

	start := time.Now()
	manager.KillAll(ctx)

	elapsed := time.Since(start)

	// Must return promptly after timeout, not hang for 60 seconds.
	require.Less(t, elapsed, 2*time.Second)
}

func TestBackgroundShell_WaitContext_Completed(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	close(done)

	bgShell := &BackgroundShell{done: done}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	t.Cleanup(cancel)

	require.True(t, bgShell.WaitContext(ctx))
}

func TestBackgroundShell_WaitContext_Canceled(t *testing.T) {
	t.Parallel()

	bgShell := &BackgroundShell{done: make(chan struct{})}

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	require.False(t, bgShell.WaitContext(ctx))
}

func TestBackgroundShellManager_OwnershipAndWorkspaceIsolation(t *testing.T) {
	t.Parallel()

	first := NewBackgroundShellManager("workspace-one")
	second := NewBackgroundShellManager("workspace-two")
	ownership := task.Ownership{
		ParentSessionID:  "session-one",
		OwnerAgentTaskID: "a12345678",
		OriginToolCallID: "tool-one",
	}
	backgroundShell, err := first.StartOwned(t.Context(), t.TempDir(), nil, "sleep 10", "owned", ownership)
	require.NoError(t, err)
	t.Cleanup(func() { _ = first.Kill(backgroundShell.ID) })

	require.Regexp(t, `^b[0-9a-z]{8}$`, backgroundShell.ID)
	require.Equal(t, "workspace-one", backgroundShell.Ownership.WorkspaceID)
	require.Equal(t, ownership.ParentSessionID, backgroundShell.Ownership.ParentSessionID)
	require.Equal(t, ownership.OwnerAgentTaskID, backgroundShell.Ownership.OwnerAgentTaskID)
	require.Equal(t, ownership.OriginToolCallID, backgroundShell.Ownership.OriginToolCallID)
	_, ok := second.Get(backgroundShell.ID)
	require.False(t, ok)
	require.Equal(t, 0, second.DetachForeground())
	require.Error(t, second.Kill(backgroundShell.ID))
}

func TestBackgroundShellManager_StopOwned(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	workingDir := t.TempDir()
	owned, err := manager.StartOwned(t.Context(), workingDir, nil, "sleep 10", "owned", task.Ownership{OwnerAgentTaskID: "a12345678"})
	require.NoError(t, err)
	unrelated, err := manager.StartOwned(t.Context(), workingDir, nil, "sleep 10", "unrelated", task.Ownership{OwnerAgentTaskID: "a87654321"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Kill(unrelated.ID) })

	require.Equal(t, 1, manager.StopOwned(t.Context(), "a12345678"))
	require.Equal(t, task.StatusKilled, owned.State().Status)
	require.False(t, unrelated.State().Status.Terminal())
	require.Equal(t, 1, manager.StopOwned(t.Context(), "a12345678"))
	require.Equal(t, 0, manager.StopOwned(t.Context(), ""))
}

func TestBackgroundShellManager_AtomicAdmission(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	workingDir := t.TempDir()
	var waitGroup sync.WaitGroup
	var admitted atomic.Int64
	errorsChannel := make(chan error, MaxBackgroundJobs+20)
	for range MaxBackgroundJobs + 20 {
		waitGroup.Go(func() {
			backgroundShell, err := manager.Start(t.Context(), workingDir, nil, "sleep 300", "")
			if err != nil {
				errorsChannel <- err
				return
			}
			admitted.Add(1)
			t.Cleanup(func() { _ = manager.Kill(backgroundShell.ID) })
		})
	}
	waitGroup.Wait()
	close(errorsChannel)

	denied := 0
	for err := range errorsChannel {
		require.ErrorContains(t, err, "maximum number of background jobs")
		denied++
	}
	require.EqualValues(t, MaxBackgroundJobs, admitted.Load())
	require.Equal(t, 20, denied)
	require.Equal(t, MaxBackgroundJobs, manager.ActiveCount())
}

func TestBackgroundShellManager_RetainedHistoryDoesNotConsumeCapacity(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	workingDir := t.TempDir()
	for range MaxBackgroundJobs {
		backgroundShell, err := manager.Start(t.Context(), workingDir, nil, "echo done", "")
		require.NoError(t, err)
		backgroundShell.Wait()
	}

	require.Equal(t, 0, manager.ActiveCount())
	require.Len(t, manager.List(), MaxBackgroundJobs)
	backgroundShell, err := manager.Start(t.Context(), workingDir, nil, "sleep 10", "")
	require.NoError(t, err)
	require.NoError(t, manager.Kill(backgroundShell.ID))
}

func TestBackgroundShellManagerRecoversPersistedTasks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outputRoot := filepath.Join(root, "output")
	metadataRoot := filepath.Join(root, "metadata")
	outputStore, err := task.NewOutputStore(outputRoot, task.OutputStoreOptions{})
	require.NoError(t, err)
	recordStore, err := task.NewStore(metadataRoot)
	require.NoError(t, err)
	manager, err := NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	backgroundShell, err := manager.StartOwned(t.Context(), root, nil, "printf persisted", "persisted", task.Ownership{ParentSessionID: "parent", OriginToolCallID: "call"})
	require.NoError(t, err)
	backgroundShell.MarkBackgrounded()
	backgroundShell.Wait()
	require.Equal(t, task.StatusCompleted, backgroundShell.State().Status)
	require.NoError(t, outputStore.Close())
	require.NoError(t, recordStore.Close())

	outputStore, err = task.NewOutputStore(outputRoot, task.OutputStoreOptions{})
	require.NoError(t, err)
	recordStore, err = task.NewStore(metadataRoot)
	require.NoError(t, err)
	recovered, err := NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		recovered.KillAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	recoveredShell, ok := recovered.Get(backgroundShell.ID)
	require.True(t, ok)
	require.Equal(t, task.StatusCompleted, recoveredShell.State().Status)
	stdout, stderr, done, err := recoveredShell.GetOutput()
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, "persisted", stdout)
	require.Empty(t, stderr)
	require.Equal(t, 0, recovered.ActiveCount())
}

func TestBackgroundShellManagerGracefulShutdownPersistsKilledAcrossRestart(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outputRoot := filepath.Join(root, "output")
	metadataRoot := filepath.Join(root, "metadata")
	outputStore, err := task.NewOutputStore(outputRoot, task.OutputStoreOptions{})
	require.NoError(t, err)
	recordStore, err := task.NewStore(metadataRoot)
	require.NoError(t, err)
	manager, err := NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	backgroundShell, err := manager.StartOwned(context.Background(), root, nil, "sleep 30", "graceful shutdown", task.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	backgroundShell.MarkBackgrounded()
	require.Eventually(t, func() bool {
		return backgroundShell.State().Status == task.StatusRunning
	}, time.Second, 10*time.Millisecond)

	shutdownCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	manager.KillAll(shutdownCtx)
	require.Equal(t, task.StatusKilled, backgroundShell.State().Status)
	require.NoError(t, recordStore.Close())

	outputStore, err = task.NewOutputStore(outputRoot, task.OutputStoreOptions{})
	require.NoError(t, err)
	recordStore, err = task.NewStore(metadataRoot)
	require.NoError(t, err)
	recovered, err := NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		recovered.KillAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	recoveredShell, ok := recovered.Get(backgroundShell.ID)
	require.True(t, ok)
	require.Equal(t, task.StatusKilled, recoveredShell.State().Status)
	require.Equal(t, 0, recovered.ActiveCount())
	notifications, err := recordStore.ListNotifications("workspace", "parent", false, false)
	require.NoError(t, err)
	require.Len(t, notifications, 1)
	require.Equal(t, backgroundShell.ID, notifications[0].TaskID)
	require.Equal(t, task.StatusKilled, notifications[0].Status)
}

func TestBackgroundShellManagerRecoveryMarksActiveLost(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outputStore, err := task.NewOutputStore(filepath.Join(root, "output"), task.OutputStoreOptions{})
	require.NoError(t, err)
	output, err := outputStore.Create("b12345678")
	require.NoError(t, err)
	require.NoError(t, output.Close())
	recordStore, err := task.NewStore(filepath.Join(root, "metadata"))
	require.NoError(t, err)
	require.NoError(t, recordStore.Put(task.Record{
		ID:        "b12345678",
		Type:      task.TypeShell,
		Ownership: task.Ownership{WorkspaceID: "workspace", ParentSessionID: "parent"},
		State:     task.StateToRecord(task.State{Status: task.StatusRunning, StartedAt: time.Now()}),
		OutputRef: "task-output:b12345678",
		Shell:     &task.ShellRecord{Command: "sleep 10", WorkingDirectory: root, Backgrounded: true},
	}))

	manager, err := NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		manager.KillAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	backgroundShell, ok := manager.Get("b12345678")
	require.True(t, ok)
	require.Equal(t, task.StatusLost, backgroundShell.State().Status)
	require.Contains(t, backgroundShell.State().LostReason, "restarted")
	require.Equal(t, 0, manager.ActiveCount())
	record, err := recordStore.Get("b12345678")
	require.NoError(t, err)
	require.Equal(t, task.StatusLost, record.State.Status)
}

func TestBackgroundShellManager_DiskOutputLimit(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := task.NewOutputStore(filepath.Join(root, "output"), task.OutputStoreOptions{MaxOutputBytes: 5})
	require.NoError(t, err)
	manager := NewBackgroundShellManagerWithStore("workspace", store)
	backgroundShell, err := manager.Start(t.Context(), root, nil, "printf 123456789", "")
	require.NoError(t, err)
	backgroundShell.Wait()

	stdout, stderr, done, executionError := backgroundShell.GetOutput()
	require.True(t, done)
	require.Equal(t, "12345", stdout)
	require.Empty(t, stderr)
	require.ErrorIs(t, executionError, task.ErrOutputLimitExceeded)
	metadata := backgroundShell.OutputMetadata()
	require.Equal(t, int64(5), metadata.OutputBytes)
	require.True(t, metadata.OutputTruncated)
	state := backgroundShell.State()
	require.Equal(t, task.StatusFailed, state.Status)
	require.Nil(t, state.ExitCode)
	require.Equal(t, "output_limit_exceeded", state.ErrorCode)
	require.Equal(t, "task-output:"+backgroundShell.ID, backgroundShell.OutputRef())
	require.NoError(t, manager.Kill(backgroundShell.ID))
}

func TestBackgroundShell_ReadOutputWaitAndRanges(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := task.NewOutputStore(filepath.Join(root, "output"), task.OutputStoreOptions{})
	require.NoError(t, err)
	manager := NewBackgroundShellManagerWithStore("workspace", store)
	backgroundShell, err := manager.Start(t.Context(), root, nil, "printf first; sleep 0.2; printf second", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Kill(backgroundShell.ID) })

	result, status, err := backgroundShell.ReadOutput(t.Context(), task.ReadOptions{}, false, 0)
	require.NoError(t, err)
	require.Equal(t, task.RetrievalNotReady, status)
	require.Contains(t, []string{"", "first"}, string(result.Output))

	result, status, err = backgroundShell.ReadOutput(t.Context(), task.ReadOptions{}, true, 10*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, task.RetrievalTimeout, status)

	result, status, err = backgroundShell.ReadOutput(t.Context(), task.ReadOptions{}, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, task.RetrievalReady, status)
	require.Equal(t, "firstsecond", string(result.Output))
	offset := int64(5)
	result, status, err = backgroundShell.ReadOutput(t.Context(), task.ReadOptions{Offset: &offset, MaxBytes: 6}, false, 0)
	require.NoError(t, err)
	require.Equal(t, task.RetrievalReady, status)
	require.Equal(t, "second", string(result.Output))
}

func TestBackgroundShell_ReadOutputCancellation(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := task.NewOutputStore(filepath.Join(root, "output"), task.OutputStoreOptions{})
	require.NoError(t, err)
	manager := NewBackgroundShellManagerWithStore("workspace", store)
	backgroundShell, err := manager.Start(t.Context(), root, nil, "sleep 10", "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = manager.Kill(backgroundShell.ID) })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = backgroundShell.ReadOutput(ctx, task.ReadOptions{}, true, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, backgroundShell.IsDone())
}

func TestBackgroundShellTerminalOutcomes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		command   string
		status    task.Status
		exitCode  int
		errorCode string
	}{
		{name: "completed", command: "exit 0", status: task.StatusCompleted, exitCode: 0},
		{name: "failed", command: "exit 7", status: task.StatusFailed, exitCode: 7, errorCode: "execution_failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			manager := NewBackgroundShellManager(t.TempDir())
			backgroundShell, err := manager.Start(t.Context(), t.TempDir(), nil, test.command, test.name)
			require.NoError(t, err)
			backgroundShell.Wait()

			state := backgroundShell.State()
			require.Equal(t, test.status, state.Status)
			require.NotNil(t, state.ExitCode)
			require.Equal(t, test.exitCode, *state.ExitCode)
			require.Equal(t, test.errorCode, state.ErrorCode)
			require.False(t, state.EndedAt.IsZero())
		})
	}
}

func TestBackgroundShellManagerStopIsBoundedAndIdempotent(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	backgroundShell, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 10", "stopped")
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return backgroundShell.Status() == task.StatusRunning
	}, time.Second, 10*time.Millisecond)

	first, err := manager.Stop(t.Context(), backgroundShell.ID)
	require.NoError(t, err)
	require.Equal(t, task.StatusKilled, first.Status)
	require.True(t, first.Interrupted)
	require.False(t, first.StopRequestedAt.IsZero())

	second, err := manager.Stop(t.Context(), backgroundShell.ID)
	require.NoError(t, err)
	require.Equal(t, first, second)
	_, retained := manager.Get(backgroundShell.ID)
	require.True(t, retained)
}

func TestBackgroundShellManagerStopLostIsImmutable(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	store, err := task.NewOutputStore(filepath.Join(root, "output"), task.OutputStoreOptions{})
	require.NoError(t, err)
	manager := NewBackgroundShellManagerWithStore("workspace", store)
	manager.stopTimeout = 20 * time.Millisecond
	id, err := task.NewID(task.TypeShell)
	require.NoError(t, err)
	output, err := store.Create(id)
	require.NoError(t, err)
	backgroundShell := &BackgroundShell{
		ID:            id,
		Ownership:     task.Ownership{WorkspaceID: "workspace"},
		cancel:        func() {},
		output:        output,
		done:          make(chan struct{}),
		executionDone: make(chan struct{}),
		state: task.State{
			Status:    task.StatusRunning,
			StartedAt: time.Now(),
		},
	}
	backgroundShell.release = func() {
		backgroundShell.activeOnce.Do(func() {
			manager.mu.Lock()
			manager.active--
			manager.mu.Unlock()
		})
	}
	manager.shells[id] = backgroundShell
	manager.active = 1

	state, err := manager.Stop(t.Context(), id)
	require.NoError(t, err)
	require.Equal(t, task.StatusLost, state.Status)
	require.NotEmpty(t, state.LostReason)
	require.Equal(t, 0, manager.ActiveCount())

	backgroundShell.finishExecution(nil)
	require.Equal(t, task.StatusLost, backgroundShell.Status())
	require.NoError(t, output.Close())
}

func TestBackgroundShellManagerStopCompletionRace(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	for range 20 {
		backgroundShell, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 0.01", "race")
		require.NoError(t, err)
		state, err := manager.Stop(t.Context(), backgroundShell.ID)
		require.NoError(t, err)
		require.Contains(t, []task.Status{task.StatusCompleted, task.StatusKilled}, state.Status)
		repeated, err := manager.Stop(t.Context(), backgroundShell.ID)
		require.NoError(t, err)
		require.Equal(t, state, repeated)
	}
}

func TestBackgroundShellManagerStopValidation(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	_, err := manager.Stop(t.Context(), "invalid")
	require.ErrorContains(t, err, "invalid task ID")
	_, err = manager.Stop(t.Context(), "a12345678")
	require.ErrorContains(t, err, "not a background shell")
	_, err = manager.Stop(t.Context(), "b12345678")
	require.ErrorContains(t, err, "background shell not found")
}

func TestBackgroundShellNotificationsAreDetachedAndDeduplicated(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	notifications := manager.SubscribeNotifications(t.Context())
	backgroundShell, err := manager.StartOwned(t.Context(), t.TempDir(), nil, "exit 7", "failed", task.Ownership{ParentSessionID: "session"})
	require.NoError(t, err)
	backgroundShell.Wait()

	select {
	case <-notifications:
		t.Fatal("synchronous shell emitted a task notification")
	default:
	}

	backgroundShell.MarkBackgrounded()
	event := requireNotification(t, notifications)
	require.Equal(t, backgroundShell.ID, event.TaskID)
	require.Equal(t, task.TypeShell, event.TaskType)
	require.Equal(t, "workspace", event.WorkspaceID)
	require.Equal(t, "session", event.ParentSessionID)
	require.Equal(t, task.StatusFailed, event.Status)
	require.NotNil(t, event.ExitCode)
	require.Equal(t, 7, *event.ExitCode)
	require.Equal(t, backgroundShell.OutputRef(), event.OutputRef)

	backgroundShell.MarkBackgrounded()
	_, err = manager.Stop(t.Context(), backgroundShell.ID)
	require.NoError(t, err)
	select {
	case duplicate := <-notifications:
		t.Fatalf("duplicate notification emitted: %#v", duplicate.Payload)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestBackgroundShellCompletedNotification(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	notifications := manager.SubscribeNotifications(t.Context())
	backgroundShell, err := manager.Start(t.Context(), t.TempDir(), nil, "exit 0", "completed")
	require.NoError(t, err)
	backgroundShell.MarkBackgrounded()
	backgroundShell.Wait()

	notification := requireNotification(t, notifications)
	require.Equal(t, task.StatusCompleted, notification.Status)
	require.NotNil(t, notification.ExitCode)
	require.Equal(t, 0, *notification.ExitCode)
}

func TestBackgroundShellKilledNotification(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundShellManager("workspace")
	notifications := manager.SubscribeNotifications(t.Context())
	backgroundShell, err := manager.Start(t.Context(), t.TempDir(), nil, "sleep 10", "killed")
	require.NoError(t, err)
	backgroundShell.MarkBackgrounded()

	state, err := manager.Stop(t.Context(), backgroundShell.ID)
	require.NoError(t, err)
	require.Equal(t, task.StatusKilled, state.Status)
	notification := requireNotification(t, notifications)
	require.Equal(t, task.StatusKilled, notification.Status)
	require.True(t, notification.Interrupted)
}

func requireNotification(t *testing.T, notifications <-chan pubsub.Event[task.Notification]) task.Notification {
	t.Helper()
	select {
	case event := <-notifications:
		return event.Payload
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for task notification")
		return task.Notification{}
	}
}
