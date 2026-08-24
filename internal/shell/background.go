package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/crux/internal/pubsub"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
)

const (
	MaxBackgroundJobs            = 50
	CompletedJobRetentionMinutes = 8 * 60
	maxTaskIDGenerationAttempts  = 32
	defaultStopTimeout           = 5 * time.Second
)

type outputCloseError struct {
	error
}

type BackgroundShell struct {
	ID                  string
	Command             string
	Description         string
	Shell               *Shell
	WorkingDir          string
	Ownership           managedtask.Ownership
	ctx                 context.Context
	cancel              context.CancelFunc
	output              *managedtask.Output
	outputStore         *managedtask.OutputStore
	outputRef           string
	createdAt           int64
	persist             func(*BackgroundShell) error
	done                chan struct{}
	executionDone       chan struct{}
	stateMu             sync.Mutex
	state               managedtask.State
	exitErr             error
	backgrounded        bool
	notificationEmitted bool
	notification        *managedtask.Notification
	notify              func(managedtask.Notification)
	release             func()
	completedAt         atomic.Int64
	activeOnce          sync.Once
	terminalOnce        sync.Once
	stopOnce            sync.Once
	foreground          atomic.Bool
	detach              chan struct{}
	detachOnce          sync.Once
}

func (b *BackgroundShell) SetForeground(value bool) {
	b.foreground.Store(value)
}

func (b *BackgroundShell) RequestDetach() {
	b.detachOnce.Do(func() { close(b.detach) })
}

func (b *BackgroundShell) Detached() <-chan struct{} {
	return b.detach
}

func (b *BackgroundShell) MarkBackgrounded() {
	b.stateMu.Lock()
	b.backgrounded = true
	notification := b.notificationLocked()
	_ = b.persistLocked()
	b.stateMu.Unlock()
	b.publishNotification(notification)
}

func (b *BackgroundShell) State() managedtask.State {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return cloneTaskState(b.state)
}

func (b *BackgroundShell) Status() managedtask.Status {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return b.state.Status
}

type BackgroundShellManager struct {
	mu            sync.RWMutex
	workspaceID   string
	shells        map[string]*BackgroundShell
	active        int
	closed        bool
	outputStore   *managedtask.OutputStore
	recordStore   *managedtask.Store
	notifications *pubsub.Broker[managedtask.Notification]
	stopTimeout   time.Duration
}

func NewBackgroundShellManager(workspaceID string) *BackgroundShellManager {
	root := workspaceID
	if !filepath.IsAbs(root) {
		var err error
		root, err = os.MkdirTemp("", "crux-task-output-")
		if err != nil {
			panic(err)
		}
	}
	store, err := managedtask.NewOutputStore(filepath.Join(root, "tasks", "output"), managedtask.OutputStoreOptions{})
	if err != nil {
		panic(err)
	}
	return NewBackgroundShellManagerWithStore(workspaceID, store)
}

func NewBackgroundShellManagerWithStore(workspaceID string, outputStore *managedtask.OutputStore) *BackgroundShellManager {
	manager, err := NewBackgroundShellManagerWithStores(workspaceID, outputStore, nil)
	if err != nil {
		panic(err)
	}
	return manager
}

func NewBackgroundShellManagerWithStores(workspaceID string, outputStore *managedtask.OutputStore, recordStore *managedtask.Store) (*BackgroundShellManager, error) {
	manager := &BackgroundShellManager{
		workspaceID:   workspaceID,
		shells:        make(map[string]*BackgroundShell),
		outputStore:   outputStore,
		recordStore:   recordStore,
		notifications: pubsub.NewBroker[managedtask.Notification](),
		stopTimeout:   defaultStopTimeout,
	}
	if err := manager.recover(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *BackgroundShellManager) recover() error {
	if m.recordStore == nil {
		return nil
	}
	records, err := m.recordStore.List()
	if err != nil {
		return fmt.Errorf("loading background shell records: %w", err)
	}
	for _, record := range records {
		if record.Type != managedtask.TypeShell || record.Ownership.WorkspaceID != m.workspaceID {
			continue
		}
		state := managedtask.StateFromRecord(record.State)
		if !state.Status.Terminal() {
			state.Status = managedtask.StatusLost
			state.EndedAt = time.Now()
			state.LostReason = "task was active when the workspace process restarted"
			record.State = managedtask.StateToRecord(state)
		}
		if record.Notification == nil && record.Shell.Backgrounded {
			record.Notification = newShellNotification(record.ID, record.Description, record.Ownership, state, record.OutputRef, false)
			record.Shell.NotificationEmitted = false
		}
		if err := m.recordStore.Put(record); err != nil {
			return fmt.Errorf("recovering background shell %s: %w", record.ID, err)
		}
		done := make(chan struct{})
		close(done)
		executionDone := make(chan struct{})
		close(executionDone)
		backgroundShell := &BackgroundShell{
			ID:                  record.ID,
			Command:             record.Shell.Command,
			Description:         record.Description,
			WorkingDir:          record.Shell.WorkingDirectory,
			Ownership:           record.Ownership,
			outputStore:         m.outputStore,
			outputRef:           record.OutputRef,
			createdAt:           record.CreatedAt,
			done:                done,
			executionDone:       executionDone,
			state:               state,
			backgrounded:        record.Shell.Backgrounded,
			notificationEmitted: record.Shell.NotificationEmitted,
			notification:        record.Notification,
			detach:              make(chan struct{}),
		}
		if !state.EndedAt.IsZero() {
			backgroundShell.completedAt.Store(state.EndedAt.Unix())
		}
		backgroundShell.persist = m.persistShell
		m.shells[record.ID] = backgroundShell
	}
	return nil
}

func (m *BackgroundShellManager) persistShell(backgroundShell *BackgroundShell) error {
	if m.recordStore == nil {
		return nil
	}
	return m.recordStore.Put(backgroundShell.record())
}

func (b *BackgroundShell) record() managedtask.Record {
	return managedtask.Record{
		ID:           b.ID,
		Type:         managedtask.TypeShell,
		Description:  b.Description,
		Ownership:    b.Ownership,
		CreatedAt:    b.createdAt,
		State:        managedtask.StateToRecord(b.state),
		OutputRef:    b.outputRef,
		Notification: b.notification,
		Shell: &managedtask.ShellRecord{
			Command:             b.Command,
			WorkingDirectory:    b.WorkingDir,
			Backgrounded:        b.backgrounded,
			NotificationEmitted: b.notificationEmitted,
		},
	}
}

func (b *BackgroundShell) persistLocked() error {
	if b.persist == nil {
		return nil
	}
	return b.persist(b)
}

func newBackgroundShellManager() *BackgroundShellManager {
	return NewBackgroundShellManager("")
}

func (m *BackgroundShellManager) Start(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command string, description string) (*BackgroundShell, error) {
	return m.StartOwned(ctx, workingDir, blockFuncs, command, description, managedtask.Ownership{})
}

func (m *BackgroundShellManager) StartOwned(ctx context.Context, workingDir string, blockFuncs []BlockFunc, command string, description string, ownership managedtask.Ownership) (*BackgroundShell, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fmt.Errorf("background shell manager is closed")
	}
	if m.active >= MaxBackgroundJobs {
		m.mu.Unlock()
		return nil, fmt.Errorf("maximum number of background jobs (%d) reached. Please terminate or wait for some jobs to complete", MaxBackgroundJobs)
	}

	var id string
	for range maxTaskIDGenerationAttempts {
		candidate, err := managedtask.NewID(managedtask.TypeShell)
		if err != nil {
			m.mu.Unlock()
			return nil, err
		}
		if _, exists := m.shells[candidate]; !exists {
			id = candidate
			break
		}
	}
	if id == "" {
		m.mu.Unlock()
		return nil, fmt.Errorf("failed to allocate unique background task ID")
	}

	output, err := m.outputStore.Create(id)
	if err != nil {
		m.mu.Unlock()
		return nil, fmt.Errorf("creating background task output: %w", err)
	}
	ownership.WorkspaceID = m.workspaceID
	shell := NewShell(&Options{WorkingDir: workingDir, BlockFuncs: blockFuncs})
	shellCtx, cancel := context.WithCancel(ctx)
	output.SetLimitHandler(cancel)
	backgroundShell := &BackgroundShell{
		ID:            id,
		Command:       command,
		Description:   description,
		WorkingDir:    workingDir,
		Ownership:     ownership,
		Shell:         shell,
		ctx:           shellCtx,
		cancel:        cancel,
		output:        output,
		outputStore:   m.outputStore,
		outputRef:     output.Ref(),
		createdAt:     time.Now().UnixMilli(),
		persist:       m.persistShell,
		done:          make(chan struct{}),
		executionDone: make(chan struct{}),
		state: managedtask.State{
			Status: managedtask.StatusPending,
		},
		detach: make(chan struct{}),
	}
	if err := backgroundShell.persistLocked(); err != nil {
		m.mu.Unlock()
		_ = output.Close()
		return nil, fmt.Errorf("persisting background task: %w", err)
	}
	backgroundShell.notify = func(notification managedtask.Notification) {
		m.notifications.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, notification)
	}
	backgroundShell.release = func() {
		backgroundShell.activeOnce.Do(func() {
			m.mu.Lock()
			m.active--
			m.mu.Unlock()
		})
	}
	m.shells[id] = backgroundShell
	m.active++
	m.mu.Unlock()

	go func() {
		defer close(backgroundShell.executionDone)
		backgroundShell.markRunning()
		executionError := shell.ExecStream(shellCtx, command, output.Stdout(), output.Stderr())
		if output.Metadata().OutputTruncated {
			executionError = managedtask.ErrOutputLimitExceeded
		}
		if err := output.Close(); err != nil && executionError == nil {
			executionError = &outputCloseError{error: err}
		}
		backgroundShell.finishExecution(executionError)
	}()

	return backgroundShell, nil
}

func (b *BackgroundShell) markRunning() {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if b.state.Status != managedtask.StatusPending {
		return
	}
	b.state.Status = managedtask.StatusRunning
	b.state.StartedAt = time.Now()
	_ = b.persistLocked()
}

func (b *BackgroundShell) finishExecution(executionError error) {
	b.stateMu.Lock()
	b.exitErr = executionError
	if !b.state.Status.Terminal() {
		now := time.Now()
		b.state.EndedAt = now
		switch {
		case !b.state.StopRequestedAt.IsZero():
			b.state.Status = managedtask.StatusKilled
			b.state.Interrupted = true
		case errors.Is(executionError, managedtask.ErrOutputLimitExceeded):
			b.state.Status = managedtask.StatusFailed
			b.state.ErrorCode = "output_limit_exceeded"
			b.state.ErrorMessage = executionError.Error()
		case executionError == nil:
			exitCode := 0
			b.state.Status = managedtask.StatusCompleted
			b.state.ExitCode = &exitCode
		case errors.As(executionError, new(*outputCloseError)):
			b.state.Status = managedtask.StatusFailed
			b.state.ErrorCode = "output_close_failed"
			b.state.ErrorMessage = executionError.Error()
		default:
			exitCode := ExitCode(executionError)
			b.state.Status = managedtask.StatusFailed
			b.state.ExitCode = &exitCode
			b.state.Interrupted = IsInterrupt(executionError)
			b.state.ErrorCode = "execution_failed"
			b.state.ErrorMessage = executionError.Error()
		}
		b.completedAt.Store(now.Unix())
	}
	notification := b.notificationLocked()
	_ = b.persistLocked()
	b.terminalOnce.Do(func() { close(b.done) })
	b.stateMu.Unlock()
	b.releaseActive()
	b.publishNotification(notification)
}

func (b *BackgroundShell) requestStop(timeout time.Duration) {
	b.stopOnce.Do(func() {
		b.stateMu.Lock()
		if b.state.Status.Terminal() {
			b.stateMu.Unlock()
			return
		}
		b.state.StopRequestedAt = time.Now()
		_ = b.persistLocked()
		b.stateMu.Unlock()
		b.cancel()
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-b.executionDone:
			case <-timer.C:
				b.markLost("shell termination was not confirmed before the stop deadline")
			}
		}()
	})
}

func (b *BackgroundShell) markLost(reason string) {
	b.stateMu.Lock()
	if b.state.Status.Terminal() {
		b.stateMu.Unlock()
		return
	}
	now := time.Now()
	b.state.Status = managedtask.StatusLost
	b.state.EndedAt = now
	b.state.LostReason = reason
	b.completedAt.Store(now.Unix())
	notification := b.notificationLocked()
	_ = b.persistLocked()
	b.terminalOnce.Do(func() { close(b.done) })
	b.stateMu.Unlock()
	b.releaseActive()
	b.publishNotification(notification)
}

func (b *BackgroundShell) releaseActive() {
	if b.release != nil {
		b.release()
	}
}

func newShellNotification(id, description string, ownership managedtask.Ownership, state managedtask.State, outputRef string, outputTruncated bool) *managedtask.Notification {
	return &managedtask.Notification{
		ID:              uuid.NewString(),
		TaskID:          id,
		TaskType:        managedtask.TypeShell,
		ToolUseID:       ownership.OriginToolCallID,
		WorkspaceID:     ownership.WorkspaceID,
		ParentSessionID: ownership.ParentSessionID,
		Status:          state.Status,
		Summary:         fmt.Sprintf("Background shell %q %s", description, state.Status),
		EndedAt:         state.EndedAt,
		OutputRef:       outputRef,
		OutputTruncated: outputTruncated,
		ExitCode:        state.ExitCode,
		Interrupted:     state.Interrupted,
		ErrorCode:       state.ErrorCode,
		ErrorMessage:    state.ErrorMessage,
		LostReason:      state.LostReason,
	}
}

func (b *BackgroundShell) notificationLocked() *managedtask.Notification {
	if !b.backgrounded || b.notificationEmitted || !b.state.Status.Terminal() {
		return nil
	}
	b.notificationEmitted = true
	b.notification = newShellNotification(b.ID, b.Description, b.Ownership, cloneTaskState(b.state), b.outputRef, b.OutputMetadata().OutputTruncated)
	return b.notification
}

func (b *BackgroundShell) publishNotification(notification *managedtask.Notification) {
	if notification != nil && b.notify != nil {
		b.notify(*notification)
	}
}

func cloneTaskState(state managedtask.State) managedtask.State {
	if state.ExitCode != nil {
		exitCode := *state.ExitCode
		state.ExitCode = &exitCode
	}
	return state
}

func (m *BackgroundShellManager) DetachForeground() int {
	m.mu.RLock()
	shells := make([]*BackgroundShell, 0, len(m.shells))
	for _, shell := range m.shells {
		shells = append(shells, shell)
	}
	m.mu.RUnlock()

	detached := 0
	for _, shell := range shells {
		if shell.foreground.Load() {
			shell.RequestDetach()
			detached++
		}
	}
	return detached
}

func (m *BackgroundShellManager) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[managedtask.Notification] {
	return m.notifications.Subscribe(ctx)
}

func (m *BackgroundShellManager) Get(id string) (*BackgroundShell, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	shell, ok := m.shells[id]
	return shell, ok
}

func (m *BackgroundShellManager) Remove(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.shells[id]; !ok {
		return fmt.Errorf("background shell not found: %s", id)
	}
	delete(m.shells, id)
	if m.recordStore != nil {
		if err := m.recordStore.Remove(id); err != nil {
			return err
		}
	}
	return nil
}

func (m *BackgroundShellManager) Stop(ctx context.Context, id string) (managedtask.State, error) {
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return managedtask.State{}, err
	}
	if taskType != managedtask.TypeShell {
		return managedtask.State{}, fmt.Errorf("task %s is not a background shell", id)
	}
	shell, ok := m.Get(id)
	if !ok {
		return managedtask.State{}, fmt.Errorf("background shell not found: %s", id)
	}
	shell.requestStop(m.stopTimeout)
	select {
	case <-shell.done:
		return shell.State(), nil
	case <-ctx.Done():
		return shell.State(), ctx.Err()
	}
}

func (m *BackgroundShellManager) Kill(id string) error {
	if _, err := m.Stop(context.Background(), id); err != nil {
		return err
	}
	return m.Remove(id)
}

type BackgroundShellInfo struct {
	ID          string
	Command     string
	Description string
}

func (m *BackgroundShellManager) List() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ids := make([]string, 0, len(m.shells))
	for id := range m.shells {
		ids = append(ids, id)
	}
	return ids
}

func (m *BackgroundShellManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func (m *BackgroundShellManager) Cleanup() int {
	now := time.Now().Unix()
	retentionSeconds := int64(CompletedJobRetentionMinutes * 60)
	removed := 0
	m.mu.Lock()
	for id, shell := range m.shells {
		completedAt := shell.completedAt.Load()
		if completedAt > 0 && now-completedAt > retentionSeconds {
			delete(m.shells, id)
			if m.recordStore != nil {
				_ = m.recordStore.Remove(id)
			}
			removed++
		}
	}
	m.mu.Unlock()
	_, _ = m.outputStore.Cleanup(time.Now().Add(-30*24*time.Hour), managedtask.DefaultCleanupLimit)
	return removed
}

func (m *BackgroundShellManager) StopOwned(ctx context.Context, ownerAgentTaskID string) int {
	if ownerAgentTaskID == "" {
		return 0
	}
	m.mu.RLock()
	shells := make([]*BackgroundShell, 0)
	for _, backgroundShell := range m.shells {
		if backgroundShell.Ownership.OwnerAgentTaskID == ownerAgentTaskID {
			shells = append(shells, backgroundShell)
		}
	}
	m.mu.RUnlock()

	var waitGroup sync.WaitGroup
	for _, backgroundShell := range shells {
		waitGroup.Go(func() {
			backgroundShell.requestStop(m.stopTimeout)
			select {
			case <-backgroundShell.done:
			case <-ctx.Done():
				backgroundShell.markLost("owner agent terminated before shell termination was confirmed")
			}
		})
	}
	waitGroup.Wait()
	return len(shells)
}

func (m *BackgroundShellManager) KillAll(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	shells := make([]*BackgroundShell, 0, len(m.shells))
	for _, shell := range m.shells {
		shells = append(shells, shell)
	}
	m.shells = make(map[string]*BackgroundShell)
	m.mu.Unlock()

	var waitGroup sync.WaitGroup
	for _, shell := range shells {
		waitGroup.Go(func() {
			shell.requestStop(m.stopTimeout)
			select {
			case <-shell.done:
			case <-ctx.Done():
				shell.markLost("workspace shutdown deadline expired before shell termination was confirmed")
			}
		})
	}
	waitGroup.Wait()
	m.notifications.Shutdown()
	_ = m.outputStore.Close()
}

func (bs *BackgroundShell) GetOutput() (stdout string, stderr string, done bool, err error) {
	stdout, stdoutErr := bs.readCompleteOutput(managedtask.OutputStreamStdout)
	stderr, stderrErr := bs.readCompleteOutput(managedtask.OutputStreamStderr)
	if stdoutErr != nil {
		return "", "", bs.IsDone(), stdoutErr
	}
	if stderrErr != nil {
		return "", "", bs.IsDone(), stderrErr
	}
	if bs.IsDone() {
		bs.stateMu.Lock()
		exitErr := bs.exitErr
		bs.stateMu.Unlock()
		return stdout, stderr, true, exitErr
	}
	return stdout, stderr, false, nil
}

func (bs *BackgroundShell) ReadOutput(ctx context.Context, options managedtask.ReadOptions, wait bool, timeout time.Duration) (managedtask.ReadResult, managedtask.RetrievalStatus, error) {
	status, err := managedtask.WaitForOutput(ctx, bs.done, wait, timeout)
	if err != nil {
		return managedtask.ReadResult{}, "", err
	}
	var result managedtask.ReadResult
	if bs.output != nil {
		result, err = bs.output.Read(options)
	} else {
		result, err = bs.outputStore.Read(bs.outputRef, options)
	}
	return result, status, err
}

func (bs *BackgroundShell) readCompleteOutput(stream managedtask.OutputStream) (string, error) {
	var buffer bytes.Buffer
	offset := int64(0)
	for {
		var result managedtask.ReadResult
		var err error
		options := managedtask.ReadOptions{Stream: stream, Offset: &offset, MaxBytes: managedtask.MaxReadBytes}
		if bs.output != nil {
			result, err = bs.output.Read(options)
		} else {
			result, err = bs.outputStore.Read(bs.outputRef, options)
		}
		if err != nil {
			return "", err
		}
		buffer.Write(result.Output)
		if result.NextOffset == offset {
			break
		}
		offset = result.NextOffset
	}
	return buffer.String(), nil
}

func (bs *BackgroundShell) IsDone() bool {
	return bs.Status().Terminal()
}

func (bs *BackgroundShell) Wait() {
	<-bs.done
}

func (bs *BackgroundShell) OutputMetadata() managedtask.OutputMetadata {
	if bs.output != nil {
		return bs.output.Metadata()
	}
	result, err := bs.outputStore.Read(bs.outputRef, managedtask.ReadOptions{MaxBytes: 1})
	if err != nil {
		return managedtask.OutputMetadata{TaskID: bs.ID, StorageError: err.Error()}
	}
	return result.Metadata
}

func (bs *BackgroundShell) OutputRef() string {
	return bs.outputRef
}

func (bs *BackgroundShell) WaitContext(ctx context.Context) bool {
	select {
	case <-bs.done:
		return true
	case <-ctx.Done():
		return false
	}
}
