package agent

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/google/uuid"
)

const (
	defaultBackgroundAgentStopTimeout = 5 * time.Second
	defaultMaxActiveBackgroundAgents  = 50
)

type BackgroundAgentInfo struct {
	ID             string
	Prompt         string
	AgentType      string
	Description    string
	ChildSessionID string
	ContinuationOf string
	Ownership      managedtask.Ownership
	State          managedtask.State
	FinalOutput    string
	Usage          managedtask.AgentUsage
}

type ManagedTaskInfo = managedtask.View

type ManagedTaskOutput = managedtask.OutputResult

type TaskLiveCoordinator interface {
	TaskLog(context.Context, string, managedtask.LogRequest) (managedtask.LogResult, error)
	SendTask(context.Context, string, string, string) (managedtask.View, error)
}

type TaskCoordinator interface {
	ListTasks() []managedtask.View
	TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (managedtask.OutputResult, error)
	StopTask(ctx context.Context, id string) (managedtask.View, error)
	RestartTask(ctx context.Context, id string) (managedtask.View, error)
	ContinueTask(ctx context.Context, id, parentSessionID, prompt, originToolCallID string) (managedtask.View, error)
	DeliverTaskNotification(ctx context.Context, notification managedtask.Notification, onPersisted, onDiscarded func()) error
}

type backgroundAgentResult struct {
	Output string
	Usage  managedtask.AgentUsage
	Err    error
}

type BackgroundAgentTask struct {
	ID          string
	Prompt      string
	AgentType   string
	Description string
	Ownership   managedtask.Ownership

	mu               sync.Mutex
	agent            SessionAgent
	state            managedtask.State
	childSessionID   string
	continuationOf   string
	createdAt        int64
	finalOutput      string
	usageBaseline    managedtask.AgentUsage
	usage            managedtask.AgentUsage
	cancel           context.CancelFunc
	done             chan struct{}
	executionDone    chan struct{}
	terminalOnce     sync.Once
	stopOnce         sync.Once
	ownerCleanupOnce sync.Once
	capacityOnce     sync.Once
	notified         bool
	notification     *managedtask.Notification
	notify           func(managedtask.Notification)
	release          func()
	admissionRelease func()
	ownerCleanup     func()
	persist          func(*BackgroundAgentTask) error

	// executionClaimed is guarded by mu. Launch, reservation failure, or
	// stopping a pending task claims sole responsibility for executionDone.
	executionClaimed bool
}

func (t *BackgroundAgentTask) bindAgent(agent SessionAgent) func() {
	t.mu.Lock()
	t.agent = agent
	t.mu.Unlock()
	return func() {
		t.mu.Lock()
		t.agent = nil
		t.mu.Unlock()
	}
}

type BackgroundAgentManager struct {
	mu               sync.RWMutex
	workspaceID      string
	tasks            map[string]*BackgroundAgentTask
	active           int
	maxActive        int
	globalAdmission  *backgroundAgentAdmission
	backgroundShells *shell.BackgroundShellManager
	recordStore      *managedtask.Store
	notifications    *pubsub.Broker[managedtask.Notification]
	stopTimeout      time.Duration
	closed           bool
}

func NewBackgroundAgentManager(workspaceID string, backgroundShells ...*shell.BackgroundShellManager) *BackgroundAgentManager {
	var shellManager *shell.BackgroundShellManager
	if len(backgroundShells) > 0 {
		shellManager = backgroundShells[0]
	}
	manager, err := NewBackgroundAgentManagerWithStore(workspaceID, shellManager, nil)
	if err != nil {
		panic(err)
	}
	return manager
}

func NewBackgroundAgentManagerWithStore(workspaceID string, backgroundShells *shell.BackgroundShellManager, recordStore *managedtask.Store) (*BackgroundAgentManager, error) {
	admission, err := newSystemBackgroundAgentAdmission(defaultMaxActiveBackgroundAgents)
	if err != nil {
		return nil, err
	}
	return newBackgroundAgentManager(workspaceID, backgroundShells, recordStore, admission)
}

func NewBackgroundAgentManagerWithAdmissionDirectory(workspaceID string, backgroundShells *shell.BackgroundShellManager, recordStore *managedtask.Store, directory string) (*BackgroundAgentManager, error) {
	admission, err := newBackgroundAgentAdmission(directory, defaultMaxActiveBackgroundAgents)
	if err != nil {
		return nil, err
	}
	return newBackgroundAgentManager(workspaceID, backgroundShells, recordStore, admission)
}

func newBackgroundAgentManager(workspaceID string, backgroundShells *shell.BackgroundShellManager, recordStore *managedtask.Store, admission *backgroundAgentAdmission) (*BackgroundAgentManager, error) {
	if admission == nil {
		return nil, fmt.Errorf("global background agent admission tracker is required")
	}
	manager := &BackgroundAgentManager{
		workspaceID:      workspaceID,
		tasks:            make(map[string]*BackgroundAgentTask),
		maxActive:        defaultMaxActiveBackgroundAgents,
		globalAdmission:  admission,
		backgroundShells: backgroundShells,
		recordStore:      recordStore,
		notifications:    pubsub.NewBroker[managedtask.Notification](),
		stopTimeout:      defaultBackgroundAgentStopTimeout,
	}
	if err := manager.recover(); err != nil {
		return nil, err
	}
	return manager, nil
}

func (m *BackgroundAgentManager) recover() error {
	if m.recordStore == nil {
		return nil
	}
	records, err := m.recordStore.List()
	if err != nil {
		return fmt.Errorf("loading background agent records: %w", err)
	}
	for _, record := range records {
		if record.Type != managedtask.TypeAgent || record.Ownership.WorkspaceID != m.workspaceID {
			continue
		}
		state := managedtask.StateFromRecord(record.State)
		if !state.Status.Terminal() {
			state.Status = managedtask.StatusLost
			state.EndedAt = time.Now()
			state.LostReason = "task was active when the workspace process restarted"
			record.State = managedtask.StateToRecord(state)
		}
		if record.Notification == nil {
			record.Notification = newAgentNotification(record.ID, record.Description, record.Ownership, state, record.Agent.ChildSessionID, record.Agent.FinalOutput, record.Agent.Usage)
			record.Agent.NotificationEmitted = false
		}
		if err := m.recordStore.Put(record); err != nil {
			return fmt.Errorf("recovering background agent %s: %w", record.ID, err)
		}
		done := make(chan struct{})
		close(done)
		executionDone := make(chan struct{})
		close(executionDone)
		backgroundTask := &BackgroundAgentTask{
			ID:             record.ID,
			Prompt:         record.Agent.Prompt,
			AgentType:      record.Agent.AgentType,
			Description:    record.Description,
			Ownership:      record.Ownership,
			state:          state,
			childSessionID: record.Agent.ChildSessionID,
			continuationOf: record.Agent.ContinuationOf,
			createdAt:      record.CreatedAt,
			finalOutput:    record.Agent.FinalOutput,
			usageBaseline:  record.Agent.UsageBaseline,
			usage:          record.Agent.Usage,
			done:           done,
			executionDone:  executionDone,
			notified:       record.Agent.NotificationEmitted,
			notification:   record.Notification,
			persist:        m.persistAgent,
		}
		backgroundTask.executionClaimed = true
		m.configureTask(backgroundTask)
		m.tasks[record.ID] = backgroundTask
	}
	return nil
}

func (m *BackgroundAgentManager) persistAgent(backgroundTask *BackgroundAgentTask) error {
	if m.recordStore == nil {
		return nil
	}
	return m.recordStore.Put(backgroundTask.record())
}

func (t *BackgroundAgentTask) record() managedtask.Record {
	outputRef := ""
	if t.childSessionID != "" {
		outputRef = "session:" + t.childSessionID
	}
	return managedtask.Record{
		ID:           t.ID,
		Type:         managedtask.TypeAgent,
		Description:  t.Description,
		Ownership:    t.Ownership,
		CreatedAt:    t.createdAt,
		State:        managedtask.StateToRecord(t.state),
		OutputRef:    outputRef,
		Notification: t.notification,
		Agent: &managedtask.AgentRecord{
			Prompt:              t.Prompt,
			AgentType:           t.AgentType,
			ChildSessionID:      t.childSessionID,
			ContinuationOf:      t.continuationOf,
			FinalOutput:         t.finalOutput,
			UsageBaseline:       t.usageBaseline,
			Usage:               t.usage,
			NotificationEmitted: t.notified,
		},
	}
}

func (t *BackgroundAgentTask) persistLocked() error {
	if t.persist == nil {
		return nil
	}
	return t.persist(t)
}

func (m *BackgroundAgentManager) configureTask(backgroundTask *BackgroundAgentTask) {
	id := backgroundTask.ID
	backgroundTask.release = func() {
		backgroundTask.capacityOnce.Do(func() {
			m.mu.Lock()
			if m.active > 0 {
				m.active--
			}
			m.mu.Unlock()
			if backgroundTask.admissionRelease != nil {
				backgroundTask.admissionRelease()
			}
		})
	}
	backgroundTask.notify = func(notification managedtask.Notification) {
		m.notifications.PublishMustDeliver(context.Background(), pubsub.CreatedEvent, notification)
	}
	if m.backgroundShells != nil {
		backgroundTask.ownerCleanup = func() {
			// An agent may be declared lost after its own short stop deadline,
			// but its child shells still get the normal bounded graceful-stop
			// window. Reusing the agent deadline here made healthy shell exits
			// race into StatusLost under scheduler pressure.
			cleanupCtx, cancel := context.WithTimeout(context.Background(), defaultBackgroundAgentStopTimeout)
			defer cancel()
			m.backgroundShells.StopOwned(cleanupCtx, id)
		}
	}
	backgroundTask.persist = m.persistAgent
}

func (m *BackgroundAgentManager) Reserve(prompt, agentType, description string, ownership managedtask.Ownership) (*BackgroundAgentTask, error) {
	return m.ReserveContext(context.Background(), prompt, agentType, description, ownership)
}

func (m *BackgroundAgentManager) ReserveContext(ctx context.Context, prompt, agentType, description string, ownership managedtask.Ownership) (*BackgroundAgentTask, error) {
	return m.reserve(ctx, prompt, agentType, description, "", managedtask.AgentUsage{}, ownership)
}

func (m *BackgroundAgentManager) ReserveContinuation(prompt, agentType, description, continuationOf string, usageBaseline managedtask.AgentUsage, ownership managedtask.Ownership) (*BackgroundAgentTask, error) {
	return m.ReserveContinuationContext(context.Background(), prompt, agentType, description, continuationOf, usageBaseline, ownership)
}

func (m *BackgroundAgentManager) ReserveContinuationContext(ctx context.Context, prompt, agentType, description, continuationOf string, usageBaseline managedtask.AgentUsage, ownership managedtask.Ownership) (*BackgroundAgentTask, error) {
	if taskType, err := managedtask.ParseID(continuationOf); err != nil || taskType != managedtask.TypeAgent {
		return nil, fmt.Errorf("invalid background agent continuation task ID %q", continuationOf)
	}
	return m.reserve(ctx, prompt, agentType, description, continuationOf, usageBaseline, ownership)
}

func (m *BackgroundAgentManager) reserve(ctx context.Context, prompt, agentType, description, continuationOf string, usageBaseline managedtask.AgentUsage, ownership managedtask.Ownership) (*BackgroundAgentTask, error) {
	if ownership.ParentSessionID == "" {
		return nil, fmt.Errorf("parent session ID is required for a background agent")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, fmt.Errorf("background agent manager is closed")
	}
	if m.active >= m.maxActive {
		return nil, fmt.Errorf("background agent workspace capacity reached: %d active tasks", m.maxActive)
	}
	id, err := managedtask.NewID(managedtask.TypeAgent)
	if err != nil {
		return nil, err
	}
	admissionRelease, err := m.globalAdmission.acquire(ctx)
	if err != nil {
		return nil, err
	}
	ownership.WorkspaceID = m.workspaceID
	task := &BackgroundAgentTask{
		ID:               id,
		Prompt:           prompt,
		AgentType:        agentType,
		Description:      description,
		Ownership:        ownership,
		continuationOf:   continuationOf,
		createdAt:        time.Now().UnixMilli(),
		usageBaseline:    usageBaseline,
		state:            managedtask.State{Status: managedtask.StatusPending},
		admissionRelease: admissionRelease,
		done:             make(chan struct{}),
		executionDone:    make(chan struct{}),
	}
	m.configureTask(task)
	if err := task.persistLocked(); err != nil {
		admissionRelease()
		return nil, fmt.Errorf("persisting background agent: %w", err)
	}
	m.tasks[id] = task
	m.active++
	return task, nil
}

func (m *BackgroundAgentManager) Start(task *BackgroundAgentTask, childSessionID string, run func(context.Context) backgroundAgentResult) error {
	return m.start(task, childSessionID, false, run)
}

func (m *BackgroundAgentManager) StartApproved(task *BackgroundAgentTask, childSessionID string, run func(context.Context) backgroundAgentResult) error {
	return m.start(task, childSessionID, true, run)
}

func (m *BackgroundAgentManager) start(task *BackgroundAgentTask, childSessionID string, approved bool, run func(context.Context) backgroundAgentResult) error {
	// Keep closure and launch in the same ordering. Once StopAll marks the
	// manager closed, no delayed session/approval result may start a task.
	// When both locks are needed, acquire manager.mu before task.mu; terminal
	// cleanup releases task.mu before returning manager/global capacity.
	m.mu.RLock()
	if m.closed {
		m.mu.RUnlock()
		return fmt.Errorf("background agent manager is closed")
	}
	if task == nil || m.tasks[task.ID] != task {
		m.mu.RUnlock()
		return fmt.Errorf("background agent reservation does not belong to this manager")
	}
	task.mu.Lock()
	if task.executionClaimed || task.state.Status != managedtask.StatusPending || !task.state.StopRequestedAt.IsZero() {
		task.mu.Unlock()
		m.mu.RUnlock()
		return fmt.Errorf("background agent reservation is no longer pending")
	}
	task.executionClaimed = true
	ownership := task.Ownership
	ownership.OwnerAgentTaskID = task.ID
	runCtx := permission.WithDetachedAgent(managedtask.WithOwnership(context.Background(), ownership))
	if approved {
		runCtx = permission.WithRunApproval(runCtx)
	}
	runCtx, cancel := context.WithCancel(runCtx)
	task.childSessionID = childSessionID
	task.cancel = cancel
	task.state.Status = managedtask.StatusRunning
	task.state.StartedAt = time.Now()
	persistErr := task.persistLocked()
	task.mu.Unlock()
	m.mu.RUnlock()
	if persistErr != nil {
		cancel()
		task.finish(backgroundAgentResult{Err: fmt.Errorf("persisting background agent start: %w", persistErr)})
		close(task.executionDone)
		return persistErr
	}
	go func() {
		defer close(task.executionDone)
		defer cancel()
		result := run(runCtx)
		task.finish(result)
	}()
	return nil
}

func (m *BackgroundAgentManager) FailReservation(task *BackgroundAgentTask, err error) {
	m.mu.RLock()
	if task == nil || m.tasks[task.ID] != task {
		m.mu.RUnlock()
		return
	}
	task.mu.Lock()
	if task.executionClaimed || task.state.Status != managedtask.StatusPending {
		task.mu.Unlock()
		m.mu.RUnlock()
		return
	}
	task.executionClaimed = true
	task.mu.Unlock()
	m.mu.RUnlock()
	task.finish(backgroundAgentResult{Err: err})
	close(task.executionDone)
}

func (m *BackgroundAgentManager) Get(id string) (*BackgroundAgentTask, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	task, ok := m.tasks[id]
	return task, ok
}

func (m *BackgroundAgentManager) List() []BackgroundAgentInfo {
	m.mu.RLock()
	tasks := make([]*BackgroundAgentTask, 0, len(m.tasks))
	for _, task := range m.tasks {
		tasks = append(tasks, task)
	}
	m.mu.RUnlock()
	infos := make([]BackgroundAgentInfo, 0, len(tasks))
	for _, task := range tasks {
		infos = append(infos, task.Info())
	}
	slices.SortFunc(infos, func(a, b BackgroundAgentInfo) int { return stringCompare(a.ID, b.ID) })
	return infos
}

func (m *BackgroundAgentManager) ActiveCount() int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.active
}

func stringCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (m *BackgroundAgentManager) Output(ctx context.Context, id string, wait bool, timeout time.Duration) (BackgroundAgentInfo, managedtask.RetrievalStatus, error) {
	task, err := m.task(id)
	if err != nil {
		return BackgroundAgentInfo{}, "", err
	}
	status, err := managedtask.WaitForOutput(ctx, task.done, wait, timeout)
	return task.Info(), status, err
}

func (m *BackgroundAgentManager) Stop(ctx context.Context, id string) (BackgroundAgentInfo, error) {
	task, err := m.task(id)
	if err != nil {
		return BackgroundAgentInfo{}, err
	}
	task.requestStop(m.stopTimeout)
	select {
	case <-task.done:
		return task.Info(), nil
	case <-ctx.Done():
		return task.Info(), ctx.Err()
	}
}

func (m *BackgroundAgentManager) UpdateProgress(id string, usage managedtask.AgentUsage) error {
	task, err := m.task(id)
	if err != nil {
		return err
	}
	task.mu.Lock()
	if !task.state.Status.Terminal() {
		task.usage = agentUsageSince(usage, task.usageBaseline)
		if err := task.persistLocked(); err != nil {
			task.mu.Unlock()
			return err
		}
	}
	task.mu.Unlock()
	return nil
}

func (m *BackgroundAgentManager) SubscribeNotifications(ctx context.Context) <-chan pubsub.Event[managedtask.Notification] {
	return m.notifications.Subscribe(ctx)
}

func (m *BackgroundAgentManager) StopAll(ctx context.Context) {
	m.mu.Lock()
	m.closed = true
	tasks := make([]*BackgroundAgentTask, 0, len(m.tasks))
	for _, backgroundTask := range m.tasks {
		tasks = append(tasks, backgroundTask)
	}
	m.mu.Unlock()

	var waitGroup sync.WaitGroup
	for _, backgroundTask := range tasks {
		waitGroup.Go(func() {
			backgroundTask.requestStop(m.stopTimeout)
			select {
			case <-backgroundTask.done:
			case <-ctx.Done():
				backgroundTask.markLost("workspace shutdown deadline expired before agent termination was confirmed")
			}
		})
	}
	waitGroup.Wait()
	m.notifications.Shutdown()
}

func (m *BackgroundAgentManager) task(id string) (*BackgroundAgentTask, error) {
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return nil, err
	}
	if taskType != managedtask.TypeAgent {
		return nil, fmt.Errorf("task %s is not a background agent", id)
	}
	task, ok := m.Get(id)
	if !ok {
		return nil, fmt.Errorf("background agent not found: %s", id)
	}
	return task, nil
}

func (t *BackgroundAgentTask) Info() BackgroundAgentInfo {
	t.mu.Lock()
	defer t.mu.Unlock()
	return BackgroundAgentInfo{
		ID:             t.ID,
		Prompt:         t.Prompt,
		AgentType:      t.AgentType,
		Description:    t.Description,
		ChildSessionID: t.childSessionID,
		ContinuationOf: t.continuationOf,
		Ownership:      t.Ownership,
		State:          t.state,
		FinalOutput:    t.finalOutput,
		Usage:          t.usage,
	}
}

func (t *BackgroundAgentTask) finish(result backgroundAgentResult) {
	t.mu.Lock()
	if t.state.Status.Terminal() {
		t.mu.Unlock()
		return
	}
	now := time.Now()
	t.finalOutput = result.Output
	t.usage = result.Usage
	t.state.EndedAt = now
	switch {
	case !t.state.StopRequestedAt.IsZero():
		t.state.Status = managedtask.StatusKilled
		t.state.Interrupted = true
	case result.Err != nil:
		t.state.Status = managedtask.StatusFailed
		t.state.ErrorCode = "agent_run_failed"
		t.state.ErrorMessage = result.Err.Error()
	case result.Output == "":
		t.state.Status = managedtask.StatusFailed
		t.state.ErrorCode = "empty_agent_output"
		t.state.ErrorMessage = "sub-agent completed but produced no text output"
	default:
		t.state.Status = managedtask.StatusCompleted
	}
	notification := t.persistTerminalLocked()
	t.mu.Unlock()
	t.cleanupOwnedShells()
	t.release()
	t.terminalOnce.Do(func() { close(t.done) })
	if notification != nil {
		t.notify(*notification)
	}
}

func (t *BackgroundAgentTask) requestStop(timeout time.Duration) {
	t.stopOnce.Do(func() {
		t.mu.Lock()
		if t.state.Status.Terminal() {
			t.mu.Unlock()
			return
		}
		t.state.StopRequestedAt = time.Now()
		cancel := t.cancel
		pending := !t.executionClaimed
		if pending {
			t.executionClaimed = true
		}
		_ = t.persistLocked()
		t.mu.Unlock()
		if pending {
			// No execution exists to wait for. Claim it under mu so a late
			// Start or FailReservation cannot replace this terminal result or
			// close executionDone again.
			t.finish(backgroundAgentResult{})
			close(t.executionDone)
			return
		}
		if cancel != nil {
			cancel()
		}
		go func() {
			timer := time.NewTimer(timeout)
			defer timer.Stop()
			select {
			case <-t.executionDone:
			case <-timer.C:
				t.markLost("agent termination was not confirmed before the stop deadline")
			}
		}()
	})
}

func (t *BackgroundAgentTask) markLost(reason string) {
	t.mu.Lock()
	if t.state.Status.Terminal() {
		t.mu.Unlock()
		return
	}
	t.state.Status = managedtask.StatusLost
	t.state.EndedAt = time.Now()
	t.state.LostReason = reason
	notification := t.persistTerminalLocked()
	t.mu.Unlock()
	t.cleanupOwnedShells()
	t.release()
	t.terminalOnce.Do(func() { close(t.done) })
	if notification != nil {
		t.notify(*notification)
	}
}

func (t *BackgroundAgentTask) cleanupOwnedShells() {
	t.ownerCleanupOnce.Do(func() {
		if t.ownerCleanup != nil {
			t.ownerCleanup()
		}
	})
}

func (c *coordinator) ListTasks() []managedtask.View {
	return c.listManagedTasks()
}

func (c *coordinator) TaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (output managedtask.OutputResult, resultErr error) {
	defer func() {
		if resultErr == nil && c.backgroundShells != nil {
			if current, ok := c.backgroundShells.Get(id); ok {
				output.OutputPaths, resultErr = current.ReadOutputPaths()
				if resultErr != nil {
					output = managedtask.OutputResult{}
				}
			}
		}
	}()
	if !wait || timeout <= 0 || c.backgroundShells == nil || tools.GetSessionFromContext(ctx) == "" {
		return c.managedTaskOutput(ctx, id, wait, timeout)
	}
	initial, err := c.managedTaskOutput(ctx, id, false, 0)
	if err != nil || initial.Task.State.Status.Terminal() {
		return initial, err
	}
	foregroundWait := c.backgroundShells.ForegroundWaits.Register(tools.GetSessionFromContext(ctx))
	defer c.backgroundShells.ForegroundWaits.Remove(foregroundWait)
	waitCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	type outputResult struct {
		output ManagedTaskOutput
		err    error
	}
	result := make(chan outputResult, 1)
	go func() {
		output, err := c.managedTaskOutput(waitCtx, id, true, timeout)
		result <- outputResult{output, err}
	}()
	select {
	case output := <-result:
		return output.output, output.err
	case <-foregroundWait.Detached:
		cancel()
		<-result
		return c.managedTaskOutput(ctx, id, false, 0)
	case <-ctx.Done():
		return ManagedTaskOutput{}, ctx.Err()
	}
}

func (c *coordinator) SendTask(ctx context.Context, id, parentSessionID, prompt string) (managedtask.View, error) {
	if permission.IsSubagent(ctx) {
		return managedtask.View{}, permission.ErrSubagentBackgroundTask
	}
	if strings.TrimSpace(prompt) == "" || parentSessionID == "" {
		return managedtask.View{}, fmt.Errorf("parent session and prompt are required")
	}
	if c.backgroundAgents == nil {
		return managedtask.View{}, fmt.Errorf("background agents are unavailable")
	}
	task, err := c.backgroundAgents.task(id)
	if err != nil {
		return managedtask.View{}, err
	}
	info := task.Info()
	if info.Ownership.ParentSessionID != parentSessionID {
		return managedtask.View{}, fmt.Errorf("task does not belong to the parent session")
	}
	if info.State.Status.Terminal() {
		return c.ContinueTask(ctx, id, parentSessionID, prompt, "")
	}
	task.mu.Lock()
	agent := task.agent
	task.mu.Unlock()
	if agent == nil {
		return managedtask.View{}, fmt.Errorf("background agent is not ready for input")
	}
	admitted := make(chan error, 1)
	_, err = agent.Run(ctx, SessionAgentCall{
		SessionID:        info.ChildSessionID,
		Prompt:           prompt,
		DeliveryMode:     DeliverySteer,
		RequireActiveRun: true,
		NonInteractive:   true,
		OnAdmitted:       func(err error) { admitted <- err },
	})
	if err != nil {
		return managedtask.View{}, err
	}
	select {
	case err := <-admitted:
		return managedAgentInfo(task.Info()), err
	case <-ctx.Done():
		return managedtask.View{}, ctx.Err()
	}
}

func (c *coordinator) TaskLog(ctx context.Context, id string, request managedtask.LogRequest) (managedtask.LogResult, error) {
	if request.Offset < 0 {
		return managedtask.LogResult{}, fmt.Errorf("log offset must be nonnegative")
	}
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return managedtask.LogResult{}, err
	}
	if taskType == managedtask.TypeShell {
		if c.backgroundShells == nil {
			return managedtask.LogResult{}, fmt.Errorf("background shells are unavailable")
		}
		command, ok := c.backgroundShells.Get(id)
		if !ok {
			return managedtask.LogResult{}, fmt.Errorf("background shell not found: %s", id)
		}
		view := managedtask.View{ID: id, Type: taskType, Description: command.Description, Command: command.Command, WorkingDirectory: command.WorkingDir, Ownership: command.Ownership, State: command.State(), OutputRef: command.OutputRef()}
		result := managedtask.LogResult{Task: view, Generation: view.State.StartedAt.Format(time.RFC3339Nano)}
		offset := request.Offset
		if request.Generation != result.Generation {
			offset = 0
		}
		read, _, err := command.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged, Offset: &offset, MaxBytes: 64 << 10}, false, 0)
		if err != nil {
			return managedtask.LogResult{}, err
		}
		result.Data = read.Output
		result.NextOffset = read.NextOffset
		result.Truncated = read.Metadata.OutputTruncated
		return result, nil
	}
	output, err := c.TaskOutput(ctx, id, false, 0)
	if err != nil {
		return managedtask.LogResult{}, err
	}
	result := managedtask.LogResult{Task: output.Task, Generation: output.Task.State.StartedAt.Format(time.RFC3339Nano), Snapshot: true}
	if output.Task.Type != managedtask.TypeAgent || output.Task.ChildSessionID == "" {
		result.Data = []byte(output.Output)
		return result, nil
	}
	messages, err := c.messages.List(ctx, output.Task.ChildSessionID)
	if err != nil {
		return managedtask.LogResult{}, err
	}
	captured := make(map[string]bool)
	var text strings.Builder
	for _, msg := range messages {
		if text.Len() > 0 {
			text.WriteString("\n\n")
		}
		if thinking := msg.ReasoningContent().Thinking; thinking != "" {
			fmt.Fprintf(&text, "thinking> %s\n", thinking)
		}
		if content := msg.Content().Text; content != "" {
			fmt.Fprintf(&text, "%s> %s\n", msg.Role, content)
		}
		for _, call := range msg.ToolCalls() {
			fmt.Fprintf(&text, "$ %s %s\n", call.Name, call.Input)
			if c.backgroundShells == nil {
				continue
			}
			for _, shellID := range c.backgroundShells.List() {
				command, ok := c.backgroundShells.Get(shellID)
				if !ok || command.Ownership.OwnerAgentTaskID != output.Task.ID || command.Ownership.OriginToolCallID != call.ID {
					continue
				}
				captured[call.ID] = true
				offset := int64(0)
				limit := int64(-1)
				for {
					read, _, err := command.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged, Offset: &offset, MaxBytes: 64 << 10}, false, 0)
					if err != nil {
						return managedtask.LogResult{}, err
					}
					text.Write(read.Output)
					result.Truncated = result.Truncated || read.Metadata.OutputTruncated
					if limit < 0 {
						limit = read.Metadata.MergedBytes
					}
					if read.NextOffset >= limit || read.NextOffset <= offset || len(read.Output) == 0 {
						break
					}
					offset = read.NextOffset
				}
			}
		}
		for _, item := range msg.ToolResults() {
			if captured[item.ToolCallID] {
				continue
			}
			var metadata tools.BashResponseMetadata
			if item.Name == tools.BashToolName && c.backgroundShells != nil && json.Unmarshal([]byte(item.Metadata), &metadata) == nil && metadata.OutputRef != "" {
				offset := int64(0)
				for {
					if err := ctx.Err(); err != nil {
						return managedtask.LogResult{}, err
					}
					read, err := c.backgroundShells.ReadRetainedOutput(metadata.OutputRef, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged, Offset: &offset, MaxBytes: 64 << 10})
					if err != nil {
						return managedtask.LogResult{}, fmt.Errorf("read retained command output: %w", err)
					}
					text.Write(read.Output)
					result.Truncated = result.Truncated || read.Metadata.OutputTruncated
					if read.NextOffset >= read.Metadata.MergedBytes || read.NextOffset <= offset {
						break
					}
					offset = read.NextOffset
				}
				text.WriteByte('\n')
			} else {
				fmt.Fprintln(&text, item.Content)
			}
		}
	}
	result.Data = []byte(text.String())
	return result, nil
}

func (c *coordinator) RestartTask(ctx context.Context, id string) (managedtask.View, error) {
	if permission.IsSubagent(ctx) {
		return managedtask.View{}, permission.ErrSubagentBackgroundTask
	}
	if c.backgroundShells == nil {
		return managedtask.View{}, fmt.Errorf("background shell service is unavailable")
	}
	restarted, err := c.backgroundShells.Restart(ctx, id)
	if err != nil {
		return managedtask.View{}, err
	}
	return managedtask.View{ID: restarted.ID, Type: managedtask.TypeShell, Description: restarted.Description, Command: restarted.Command, WorkingDirectory: restarted.WorkingDir, Ownership: restarted.Ownership, State: restarted.State(), OutputRef: restarted.OutputRef()}, nil
}

func (c *coordinator) StopTask(ctx context.Context, id string) (managedtask.View, error) {
	return c.stopManagedTask(ctx, id)
}

func (c *coordinator) DeliverTaskNotification(ctx context.Context, notification managedtask.Notification, onPersisted, onDiscarded func()) error {
	payload, err := xml.Marshal(taskNotificationPrompt{
		TaskID:       notification.TaskID,
		TaskType:     notification.TaskType,
		ToolUseID:    notification.ToolUseID,
		OutputRef:    notification.OutputRef,
		Status:       notification.Status,
		Summary:      notification.Summary,
		Result:       notification.FinalOutput,
		ErrorCode:    notification.ErrorCode,
		ErrorMessage: notification.ErrorMessage,
		LostReason:   notification.LostReason,
		ExitCode:     notification.ExitCode,
		Usage:        notification.Usage,
	})
	if err != nil {
		return fmt.Errorf("encoding task notification: %w", err)
	}
	call := SessionAgentCall{
		SessionID:          notification.ParentSessionID,
		SubmissionID:       notification.ID,
		taskNotification:   true,
		Prompt:             string(payload),
		NonInteractive:     true,
		OnMessagePersisted: onPersisted,
		OnDiscarded:        onDiscarded,
	}
	ctx, err = c.bindSessionRuntime(ctx, &call)
	if err != nil {
		if onDiscarded != nil {
			onDiscarded()
		}
		return err
	}
	_, err = c.currentAgent.Run(ctx, call)
	return err
}

type taskNotificationPrompt struct {
	XMLName      xml.Name               `xml:"task-notification"`
	TaskID       string                 `xml:"task-id"`
	TaskType     managedtask.Type       `xml:"task-type"`
	ToolUseID    string                 `xml:"tool-use-id,omitempty"`
	OutputRef    string                 `xml:"output-file,omitempty"`
	Status       managedtask.Status     `xml:"status"`
	Summary      string                 `xml:"summary"`
	Result       string                 `xml:"result,omitempty"`
	ErrorCode    string                 `xml:"error-code,omitempty"`
	ErrorMessage string                 `xml:"error-message,omitempty"`
	LostReason   string                 `xml:"lost-reason,omitempty"`
	ExitCode     *int                   `xml:"exit-code,omitempty"`
	Usage        managedtask.AgentUsage `xml:"usage,omitempty"`
}

func (c *coordinator) listManagedTasks() []ManagedTaskInfo {
	var active []ManagedTaskInfo
	var terminal []ManagedTaskInfo
	appendTask := func(task ManagedTaskInfo) {
		if task.State.Status.Terminal() {
			terminal = append(terminal, task)
		} else {
			active = append(active, task)
		}
	}
	if c.backgroundShells != nil {
		for _, id := range c.backgroundShells.List() {
			backgroundShell, ok := c.backgroundShells.Get(id)
			if !ok {
				continue
			}
			appendTask(ManagedTaskInfo{
				ID:               id,
				Type:             managedtask.TypeShell,
				Description:      backgroundShell.Description,
				Command:          backgroundShell.Command,
				WorkingDirectory: backgroundShell.WorkingDir,
				Ownership:        backgroundShell.Ownership,
				State:            backgroundShell.State(),
				OutputRef:        backgroundShell.OutputRef(),
			})
		}
	}
	if c.backgroundAgents != nil {
		for _, backgroundAgent := range c.backgroundAgents.List() {
			view := managedAgentInfo(backgroundAgent)
			view.FinalOutput = ""
			appendTask(view)
		}
	}
	if c.backgroundImages != nil {
		for _, imageJob := range c.backgroundImages.List() {
			appendTask(imageJob)
		}
	}
	slices.SortFunc(active, func(a, b ManagedTaskInfo) int {
		return b.State.StartedAt.Compare(a.State.StartedAt)
	})
	slices.SortFunc(terminal, func(a, b ManagedTaskInfo) int {
		return b.State.EndedAt.Compare(a.State.EndedAt)
	})
	if len(terminal) > managedtask.RecentTerminalLimit {
		terminal = terminal[:managedtask.RecentTerminalLimit]
	}
	return append(active, terminal...)
}

func (c *coordinator) managedTaskOutput(ctx context.Context, id string, wait bool, timeout time.Duration) (ManagedTaskOutput, error) {
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return ManagedTaskOutput{}, err
	}
	switch taskType {
	case managedtask.TypeShell:
		if c.backgroundShells == nil {
			return ManagedTaskOutput{}, fmt.Errorf("background shell not found: %s", id)
		}
		backgroundShell, ok := c.backgroundShells.Get(id)
		if !ok {
			return ManagedTaskOutput{}, fmt.Errorf("background shell not found: %s", id)
		}
		tailBytes := int64(managedtask.DefaultReadBytes)
		result, status, err := backgroundShell.ReadOutput(ctx, managedtask.ReadOptions{Stream: managedtask.OutputStreamMerged, TailBytes: &tailBytes}, wait, timeout)
		return ManagedTaskOutput{
			Task: ManagedTaskInfo{
				ID:               id,
				Type:             managedtask.TypeShell,
				Description:      backgroundShell.Description,
				Command:          backgroundShell.Command,
				WorkingDirectory: backgroundShell.WorkingDir,
				Ownership:        backgroundShell.Ownership,
				State:            backgroundShell.State(),
				OutputRef:        backgroundShell.OutputRef(),
			},
			Output:          string(result.Output),
			RetrievalStatus: status,
			Status:          status,
			NextOffset:      result.NextOffset,
			OutputTruncated: result.OutputTruncated || result.NextOffset > int64(len(result.Output)),
		}, err
	case managedtask.TypeAgent:
		if c.backgroundAgents == nil {
			return ManagedTaskOutput{}, fmt.Errorf("background agent not found: %s", id)
		}
		info, status, err := c.backgroundAgents.Output(ctx, id, wait, timeout)
		info = c.refreshBackgroundAgentProgress(info)
		return ManagedTaskOutput{
			Task:            managedAgentInfo(info),
			Output:          info.FinalOutput,
			RetrievalStatus: status,
			Status:          status,
			NextOffset:      int64(len(info.FinalOutput)),
		}, err
	case managedtask.TypeImage:
		if c.backgroundImages == nil {
			return ManagedTaskOutput{}, fmt.Errorf("background image job not found: %s", id)
		}
		return c.backgroundImages.Output(ctx, id, wait, timeout)
	default:
		return ManagedTaskOutput{}, fmt.Errorf("unsupported task type %q", taskType)
	}
}

func (c *coordinator) stopManagedTask(ctx context.Context, id string) (ManagedTaskInfo, error) {
	taskType, err := managedtask.ParseID(id)
	if err != nil {
		return ManagedTaskInfo{}, err
	}
	switch taskType {
	case managedtask.TypeShell:
		if c.backgroundShells == nil {
			return ManagedTaskInfo{}, fmt.Errorf("background shell not found: %s", id)
		}
		state, err := c.backgroundShells.Stop(ctx, id)
		if err != nil {
			return ManagedTaskInfo{}, err
		}
		backgroundShell, _ := c.backgroundShells.Get(id)
		return ManagedTaskInfo{ID: id, Type: managedtask.TypeShell, Description: backgroundShell.Description, Command: backgroundShell.Command, WorkingDirectory: backgroundShell.WorkingDir, Ownership: backgroundShell.Ownership, State: state, OutputRef: backgroundShell.OutputRef()}, nil
	case managedtask.TypeAgent:
		if c.backgroundAgents == nil {
			return ManagedTaskInfo{}, fmt.Errorf("background agent not found: %s", id)
		}
		info, err := c.backgroundAgents.Stop(ctx, id)
		return managedAgentInfo(info), err
	case managedtask.TypeImage:
		if c.backgroundImages == nil {
			return ManagedTaskInfo{}, fmt.Errorf("background image job not found: %s", id)
		}
		return c.backgroundImages.Stop(ctx, id)
	default:
		return ManagedTaskInfo{}, fmt.Errorf("unsupported task type %q", taskType)
	}
}

func (c *coordinator) refreshBackgroundAgentProgress(info BackgroundAgentInfo) BackgroundAgentInfo {
	if info.ChildSessionID == "" {
		return info
	}
	usage := managedtask.AgentUsage{}
	if persisted, err := c.sessions.Get(context.Background(), info.ChildSessionID); err == nil {
		usage.PromptTokens = persisted.PromptTokens
		usage.CompletionTokens = persisted.CompletionTokens
		usage.Cost = persisted.Cost
	}
	if messages, err := c.messages.List(context.Background(), info.ChildSessionID); err == nil {
		for _, msg := range messages {
			usage.ToolUseCount += len(msg.ToolCalls())
		}
	}
	_ = c.backgroundAgents.UpdateProgress(info.ID, usage)
	updated, ok := c.backgroundAgents.Get(info.ID)
	if !ok {
		return info
	}
	return updated.Info()
}

func managedAgentInfo(info BackgroundAgentInfo) ManagedTaskInfo {
	return ManagedTaskInfo{
		ID:             info.ID,
		Type:           managedtask.TypeAgent,
		Description:    info.Description,
		Ownership:      info.Ownership,
		State:          info.State,
		OutputRef:      "session:" + info.ChildSessionID,
		ChildSessionID: info.ChildSessionID,
		ContinuationOf: info.ContinuationOf,
		AgentType:      info.AgentType,
		FinalOutput:    info.FinalOutput,
		Usage:          info.Usage,
	}
}

func newAgentNotification(id, description string, ownership managedtask.Ownership, state managedtask.State, childSessionID, finalOutput string, usage managedtask.AgentUsage) *managedtask.Notification {
	return &managedtask.Notification{
		ID:              uuid.NewString(),
		TaskID:          id,
		TaskType:        managedtask.TypeAgent,
		ToolUseID:       ownership.OriginToolCallID,
		WorkspaceID:     ownership.WorkspaceID,
		ParentSessionID: ownership.ParentSessionID,
		Status:          state.Status,
		Summary:         fmt.Sprintf("Background agent %q %s", description, state.Status),
		EndedAt:         state.EndedAt,
		OutputRef:       "session:" + childSessionID,
		Interrupted:     state.Interrupted,
		ErrorCode:       state.ErrorCode,
		ErrorMessage:    state.ErrorMessage,
		LostReason:      state.LostReason,
		FinalOutput:     finalOutput,
		Usage:           usage,
	}
}

func (t *BackgroundAgentTask) persistTerminalLocked() *managedtask.Notification {
	notification := t.notificationLocked()
	if err := t.persistLocked(); err != nil {
		if t.state.Status == managedtask.StatusCompleted {
			t.state.Status = managedtask.StatusFailed
		}
		previousError := t.state.ErrorMessage
		t.state.ErrorCode = "agent_persistence_failed"
		t.state.ErrorMessage = fmt.Sprintf("Failed to persist terminal task record; output may be lost on restart: %v", err)
		if previousError != "" {
			t.state.ErrorMessage = previousError + "; " + t.state.ErrorMessage
		}
		if notification != nil {
			id := notification.ID
			*notification = *newAgentNotification(t.ID, t.Description, t.Ownership, t.state, t.childSessionID, t.finalOutput, t.usage)
			notification.ID = id
		}
		if retryErr := t.persistLocked(); retryErr != nil {
			slog.Error("Failed to persist background agent terminal failure", "task_id", t.ID, "error", retryErr)
		}
	}
	return notification
}

func (t *BackgroundAgentTask) notificationLocked() *managedtask.Notification {
	if t.notified || !t.state.Status.Terminal() {
		return nil
	}
	t.notified = true
	t.notification = newAgentNotification(t.ID, t.Description, t.Ownership, t.state, t.childSessionID, t.finalOutput, t.usage)
	return t.notification
}
