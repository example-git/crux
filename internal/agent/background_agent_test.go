package agent

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestDeliverTaskNotificationQueuesStructuredParentMessage(t *testing.T) {
	env := testEnv(t)
	parentAgent := NewSessionAgent(SessionAgentOptions{Sessions: env.sessions, Messages: env.messages}).(*sessionAgent)
	parentAgent.activeRequests.Set("parent", &activeCancel{cancel: func() {}})
	coordinator := &coordinator{currentAgent: parentAgent}
	persisted := false
	discarded := false
	notification := managedtask.Notification{
		ID:              "notification",
		TaskID:          "a12345678",
		TaskType:        managedtask.TypeAgent,
		ToolUseID:       "tool-call",
		ParentSessionID: "parent",
		Status:          managedtask.StatusCompleted,
		Summary:         "Agent completed",
		OutputRef:       "session:child",
		FinalOutput:     "finished",
		Usage:           managedtask.AgentUsage{PromptTokens: 3, CompletionTokens: 2},
	}

	require.NoError(t, coordinator.DeliverTaskNotification(t.Context(), notification, func() { persisted = true }, func() { discarded = true }))
	queued, ok := parentAgent.messageQueue.Get("parent")
	require.True(t, ok)
	require.Len(t, queued, 1)
	require.Contains(t, queued[0].Prompt, "<task-notification>")
	require.Contains(t, queued[0].Prompt, "<task-id>a12345678</task-id>")
	require.Contains(t, queued[0].Prompt, "<output-file>session:child</output-file>")
	require.Contains(t, queued[0].Prompt, "<result>finished</result>")
	require.False(t, persisted)
	require.False(t, discarded)
	parentAgent.publishCanceledQueueDrops(queued)
	require.True(t, discarded)
	require.False(t, persisted)
}

func TestBackgroundAgentManagerLifecycle(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundAgentManager("workspace")
	notifications := manager.SubscribeNotifications(t.Context())
	ownership := managedtask.Ownership{ParentSessionID: "parent", OriginToolCallID: "call"}
	backgroundTask, err := manager.Reserve("prompt", "task", "description", ownership)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusPending, backgroundTask.Info().State.Status)
	require.Equal(t, 1, manager.ActiveCount())

	usage := managedtask.AgentUsage{PromptTokens: 10, CompletionTokens: 4, Cost: 0.25, ToolUseCount: 2}
	manager.Start(backgroundTask, "child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "finished", Usage: usage}
	})
	info, status, err := manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.RetrievalReady, status)
	require.Equal(t, managedtask.StatusCompleted, info.State.Status)
	require.Equal(t, "finished", info.FinalOutput)
	require.Equal(t, usage, info.Usage)
	require.Equal(t, "child", info.ChildSessionID)
	require.Len(t, manager.List(), 1)
	require.Equal(t, 0, manager.ActiveCount())

	select {
	case event := <-notifications:
		require.Equal(t, backgroundTask.ID, event.Payload.TaskID)
		require.Equal(t, "finished", event.Payload.FinalOutput)
		require.Equal(t, usage, event.Payload.Usage)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for agent notification")
	}

	_, err = manager.Reserve("next", "task", "next", ownership)
	require.NoError(t, err)
}

func TestBackgroundAgentManagerRunsSameParentTasksConcurrently(t *testing.T) {
	manager := NewBackgroundAgentManager("workspace")
	ownership := managedtask.Ownership{ParentSessionID: "parent"}
	tasks := make([]*BackgroundAgentTask, 3)
	started := make(chan string, len(tasks))
	release := make(chan struct{})
	for index := range tasks {
		backgroundTask, err := manager.Reserve("prompt", "task", "description", ownership)
		require.NoError(t, err)
		tasks[index] = backgroundTask
		require.NoError(t, manager.Start(backgroundTask, "child-"+backgroundTask.ID, func(context.Context) backgroundAgentResult {
			started <- backgroundTask.ID
			<-release
			return backgroundAgentResult{Output: backgroundTask.ID}
		}))
	}

	seen := make(map[string]bool)
	for range tasks {
		select {
		case id := <-started:
			seen[id] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for concurrent background agents")
		}
	}
	require.Len(t, seen, len(tasks))
	require.Equal(t, len(tasks), manager.ActiveCount())
	close(release)
	for _, backgroundTask := range tasks {
		info, status, err := manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
		require.NoError(t, err)
		require.Equal(t, managedtask.RetrievalReady, status)
		require.Equal(t, managedtask.StatusCompleted, info.State.Status)
		require.Equal(t, backgroundTask.ID, info.FinalOutput)
	}
	require.Equal(t, 0, manager.ActiveCount())
}

func TestBackgroundAgentManagerAdmissionCapacityIsAtomic(t *testing.T) {
	manager := NewBackgroundAgentManager("workspace")
	manager.maxActive = 8
	type reservation struct {
		task *BackgroundAgentTask
		err  error
	}
	results := make(chan reservation, 32)
	for range 32 {
		go func() {
			backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
			results <- reservation{task: backgroundTask, err: err}
		}()
	}

	admitted := make([]*BackgroundAgentTask, 0, manager.maxActive)
	for range 32 {
		result := <-results
		if result.err != nil {
			require.ErrorContains(t, result.err, "capacity reached")
			continue
		}
		admitted = append(admitted, result.task)
	}
	require.Len(t, admitted, manager.maxActive)
	require.Equal(t, manager.maxActive, manager.ActiveCount())
	for _, backgroundTask := range admitted {
		manager.FailReservation(backgroundTask, errors.New("test cleanup"))
	}
	require.Equal(t, 0, manager.ActiveCount())
}

func TestBackgroundAgentManagerMarksDetachedExecution(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundAgentManager("workspace")
	backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	detached := make(chan bool, 1)
	ownership := make(chan managedtask.Ownership, 1)
	manager.Start(backgroundTask, "child", func(ctx context.Context) backgroundAgentResult {
		detached <- permission.IsDetachedAgent(ctx)
		ownership <- managedtask.OwnershipFromContext(ctx)
		return backgroundAgentResult{Output: "finished"}
	})

	select {
	case marked := <-detached:
		require.True(t, marked)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for detached agent execution")
	}
	owner := <-ownership
	require.Equal(t, backgroundTask.ID, owner.OwnerAgentTaskID)
	require.Equal(t, "parent", owner.ParentSessionID)
	_, _, err = manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
	require.NoError(t, err)
}

func TestBackgroundAgentManagerCleansOwnedShells(t *testing.T) {
	for _, test := range []struct {
		name     string
		result   backgroundAgentResult
		stop     bool
		lost     bool
		expected managedtask.Status
	}{
		{name: "completed", result: backgroundAgentResult{Output: "done"}, expected: managedtask.StatusCompleted},
		{name: "failed", result: backgroundAgentResult{Err: errors.New("failed")}, expected: managedtask.StatusFailed},
		{name: "killed", stop: true, expected: managedtask.StatusKilled},
		{name: "lost", stop: true, lost: true, expected: managedtask.StatusLost},
	} {
		t.Run(test.name, func(t *testing.T) {
			shellManager := shell.NewBackgroundShellManager("workspace")
			manager := NewBackgroundAgentManager("workspace", shellManager)
			if test.lost {
				manager.stopTimeout = 20 * time.Millisecond
			}
			backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
			require.NoError(t, err)
			ownedShell, err := shellManager.StartOwned(t.Context(), t.TempDir(), nil, "sleep 10", "owned", managedtask.Ownership{OwnerAgentTaskID: backgroundTask.ID})
			require.NoError(t, err)
			unrelatedShell, err := shellManager.StartOwned(t.Context(), t.TempDir(), nil, "sleep 10", "unrelated", managedtask.Ownership{OwnerAgentTaskID: "a87654321"})
			require.NoError(t, err)
			t.Cleanup(func() { _ = shellManager.Kill(unrelatedShell.ID) })
			release := make(chan struct{})
			manager.Start(backgroundTask, "child", func(ctx context.Context) backgroundAgentResult {
				if test.stop {
					if test.lost {
						<-release
					} else {
						<-ctx.Done()
						return backgroundAgentResult{Err: ctx.Err()}
					}
				}
				return test.result
			})

			var info BackgroundAgentInfo
			if test.stop {
				info, err = manager.Stop(t.Context(), backgroundTask.ID)
			} else {
				info, _, err = manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
			}
			require.NoError(t, err)
			require.Equal(t, test.expected, info.State.Status)
			require.Equal(t, managedtask.StatusKilled, ownedShell.State().Status)
			require.False(t, unrelatedShell.State().Status.Terminal())
			if test.lost {
				close(release)
			}
		})
	}
}

func TestBackgroundAgentManagerStopAllIsBounded(t *testing.T) {
	manager := NewBackgroundAgentManager("workspace")
	manager.stopTimeout = time.Second
	backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	release := make(chan struct{})
	manager.Start(backgroundTask, "child", func(context.Context) backgroundAgentResult {
		<-release
		return backgroundAgentResult{Output: "late"}
	})

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	manager.StopAll(ctx)
	require.Equal(t, managedtask.StatusLost, backgroundTask.Info().State.Status)
	_, err = manager.Reserve("next", "task", "next", managedtask.Ownership{ParentSessionID: "other"})
	require.ErrorContains(t, err, "manager is closed")
	close(release)
}

func TestBackgroundAgentManagerRecoversPersistedTasks(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	recordStore, err := managedtask.NewStore(filepath.Join(root, "metadata"))
	require.NoError(t, err)
	manager, err := NewBackgroundAgentManagerWithStore("workspace", nil, recordStore)
	require.NoError(t, err)
	backgroundTask, err := manager.Reserve("prompt", "reviewer", "description", managedtask.Ownership{ParentSessionID: "parent", OriginToolCallID: "call"})
	require.NoError(t, err)
	require.NoError(t, manager.Start(backgroundTask, "child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "persisted", Usage: managedtask.AgentUsage{PromptTokens: 3, CompletionTokens: 2, Cost: 0.1}}
	}))
	_, _, err = manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
	require.NoError(t, err)
	require.NoError(t, recordStore.Close())

	recordStore, err = managedtask.NewStore(filepath.Join(root, "metadata"))
	require.NoError(t, err)
	recovered, err := NewBackgroundAgentManagerWithStore("workspace", nil, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		recovered.StopAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	info, status, err := recovered.Output(t.Context(), backgroundTask.ID, false, 0)
	require.NoError(t, err)
	require.Equal(t, managedtask.RetrievalReady, status)
	require.Equal(t, managedtask.StatusCompleted, info.State.Status)
	require.Equal(t, "persisted", info.FinalOutput)
	require.Equal(t, "reviewer", info.AgentType)
	require.Equal(t, "child", info.ChildSessionID)
	require.Equal(t, int64(3), info.Usage.PromptTokens)
}

func TestBackgroundAgentManagerRecoveryMarksActiveLost(t *testing.T) {
	t.Parallel()

	recordStore, err := managedtask.NewStore(filepath.Join(t.TempDir(), "metadata"))
	require.NoError(t, err)
	require.NoError(t, recordStore.Put(managedtask.Record{
		ID:        "a12345678",
		Type:      managedtask.TypeAgent,
		Ownership: managedtask.Ownership{WorkspaceID: "workspace", ParentSessionID: "parent"},
		State:     managedtask.StateToRecord(managedtask.State{Status: managedtask.StatusRunning, StartedAt: time.Now()}),
		OutputRef: "session:child",
		Agent:     &managedtask.AgentRecord{Prompt: "prompt", AgentType: "task", ChildSessionID: "child"},
	}))
	manager, err := NewBackgroundAgentManagerWithStore("workspace", nil, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		manager.StopAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	info, status, err := manager.Output(t.Context(), "a12345678", false, 0)
	require.NoError(t, err)
	require.Equal(t, managedtask.RetrievalReady, status)
	require.Equal(t, managedtask.StatusLost, info.State.Status)
	require.Contains(t, info.State.LostReason, "restarted")
	require.Equal(t, 0, manager.ActiveCount())
	record, err := recordStore.Get("a12345678")
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusLost, record.State.Status)
}

func TestManagedTaskViewsReconstructAcrossSessionsAndManagers(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	root := t.TempDir()
	outputStore, err := managedtask.NewOutputStore(filepath.Join(root, "output"), managedtask.OutputStoreOptions{})
	require.NoError(t, err)
	output, err := outputStore.Create("b12345678")
	require.NoError(t, err)
	_, err = output.Stdout().Write([]byte("persisted shell"))
	require.NoError(t, err)
	require.NoError(t, output.Close())
	recordStore, err := managedtask.NewStore(filepath.Join(root, "metadata"))
	require.NoError(t, err)
	endedAt := time.Now()
	exitCode := 0
	require.NoError(t, recordStore.Put(managedtask.Record{
		ID:          "b12345678",
		Type:        managedtask.TypeShell,
		Description: "shell from first session",
		Ownership:   managedtask.Ownership{WorkspaceID: "workspace", ParentSessionID: "parent-one"},
		State:       managedtask.StateToRecord(managedtask.State{Status: managedtask.StatusCompleted, EndedAt: endedAt, ExitCode: &exitCode}),
		OutputRef:   "task-output:b12345678",
		Shell:       &managedtask.ShellRecord{Command: "printf persisted", WorkingDirectory: root},
	}))
	require.NoError(t, recordStore.Put(managedtask.Record{
		ID:          "a12345678",
		Type:        managedtask.TypeAgent,
		Description: "agent from second session",
		Ownership:   managedtask.Ownership{WorkspaceID: "workspace", ParentSessionID: "parent-two"},
		State:       managedtask.StateToRecord(managedtask.State{Status: managedtask.StatusCompleted, EndedAt: endedAt}),
		OutputRef:   "session:child",
		Agent:       &managedtask.AgentRecord{Prompt: "review", AgentType: config.AgentTask, ChildSessionID: "child", FinalOutput: "persisted agent"},
	}))
	require.NoError(t, recordStore.Put(managedtask.Record{
		ID:          "a87654321",
		Type:        managedtask.TypeAgent,
		Description: "other workspace",
		Ownership:   managedtask.Ownership{WorkspaceID: "other", ParentSessionID: "parent"},
		State:       managedtask.StateToRecord(managedtask.State{Status: managedtask.StatusCompleted, EndedAt: endedAt}),
		OutputRef:   "session:other-child",
		Agent:       &managedtask.AgentRecord{Prompt: "other", AgentType: config.AgentTask, ChildSessionID: "other-child", FinalOutput: "other"},
	}))

	firstShells, err := shell.NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	firstAgents, err := NewBackgroundAgentManagerWithStore("workspace", firstShells, recordStore)
	require.NoError(t, err)
	secondShells, err := shell.NewBackgroundShellManagerWithStores("workspace", outputStore, recordStore)
	require.NoError(t, err)
	secondAgents, err := NewBackgroundAgentManagerWithStore("workspace", secondShells, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		firstAgents.StopAll(t.Context())
		secondAgents.StopAll(t.Context())
		require.NoError(t, outputStore.Close())
		require.NoError(t, recordStore.Close())
	})

	first := (&coordinator{sessions: env.sessions, messages: env.messages, backgroundShells: firstShells, backgroundAgents: firstAgents}).listManagedTasks()
	second := (&coordinator{sessions: env.sessions, messages: env.messages, backgroundShells: secondShells, backgroundAgents: secondAgents}).listManagedTasks()
	require.Equal(t, first, second)
	require.Len(t, first, 2)
	require.Equal(t, []string{"a12345678", "b12345678"}, []string{first[0].ID, first[1].ID})
	require.Equal(t, "parent-two", first[0].Ownership.ParentSessionID)
	require.Equal(t, "parent-one", first[1].Ownership.ParentSessionID)
	require.Equal(t, "printf persisted", first[1].Command)
	result, ok := secondShells.Get("b12345678")
	require.True(t, ok)
	stdout, stderr, done, err := result.GetOutput()
	require.NoError(t, err)
	require.True(t, done)
	require.Equal(t, "persisted shell", stdout)
	require.Empty(t, stderr)
}

//nolint:tparallel // Subtests exercise shared lifecycle timing and remain serial.
func TestBackgroundAgentManagerStopAndLost(t *testing.T) {
	t.Parallel()

	t.Run("confirmed cancellation", func(t *testing.T) {
		manager := NewBackgroundAgentManager("workspace")
		backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
		require.NoError(t, err)
		manager.Start(backgroundTask, "child", func(ctx context.Context) backgroundAgentResult {
			<-ctx.Done()
			return backgroundAgentResult{Err: ctx.Err()}
		})

		info, err := manager.Stop(t.Context(), backgroundTask.ID)
		require.NoError(t, err)
		require.Equal(t, managedtask.StatusKilled, info.State.Status)
		require.True(t, info.State.Interrupted)
		repeated, err := manager.Stop(t.Context(), backgroundTask.ID)
		require.NoError(t, err)
		require.Equal(t, info, repeated)
	})

	t.Run("unconfirmed cancellation", func(t *testing.T) {
		manager := NewBackgroundAgentManager("workspace")
		manager.stopTimeout = 20 * time.Millisecond
		backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
		require.NoError(t, err)
		release := make(chan struct{})
		manager.Start(backgroundTask, "child", func(context.Context) backgroundAgentResult {
			<-release
			return backgroundAgentResult{Output: "late"}
		})

		info, err := manager.Stop(t.Context(), backgroundTask.ID)
		require.NoError(t, err)
		require.Equal(t, managedtask.StatusLost, info.State.Status)
		close(release)
		require.Eventually(t, func() bool {
			return backgroundTask.Info().State.Status == managedtask.StatusLost
		}, time.Second, 10*time.Millisecond)
	})
}

func TestBackgroundAgentManagerValidation(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundAgentManager("workspace")
	_, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{})
	require.ErrorContains(t, err, "parent session ID is required")
	_, _, err = manager.Output(t.Context(), "invalid", false, 0)
	require.ErrorContains(t, err, "invalid task ID")
	_, err = manager.Stop(t.Context(), "b12345678")
	require.ErrorContains(t, err, "not a background agent")
	_, err = manager.Stop(t.Context(), "a12345678")
	require.ErrorContains(t, err, "background agent not found")
}

func TestStartBackgroundSubAgentPersistsAndReturnsImmediately(t *testing.T) {
	env := testEnv(t)
	providerID := "background-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.backgroundAgents = NewBackgroundAgentManager(env.workingDir)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	started := make(chan struct{})
	release := make(chan struct{})
	mockAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		close(started)
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		child, getErr := env.sessions.Get(context.Background(), call.SessionID)
		if getErr != nil {
			return nil, getErr
		}
		child.PromptTokens = 12
		child.CompletionTokens = 5
		child.Cost = 0.1
		if _, saveErr := env.sessions.Save(context.Background(), child); saveErr != nil {
			return nil, saveErr
		}
		return agentResultWithText("background result"), nil
	})
	runParams := subAgentParams{
		Agent:          mockAgent,
		SessionID:      parent.ID,
		AgentMessageID: "message",
		ToolCallID:     "call",
		Prompt:         "work",
		SessionTitle:   "Background",
	}
	parentCtx, cancelParent := context.WithCancel(t.Context())
	response, err := coord.startBackgroundSubAgent(parentCtx, AgentParams{Prompt: "work", RunInBackground: true}, fantasy.ToolCall{ID: "call"}, presetSubagent{agent: mockAgent, title: "Background"}, runParams)
	require.NoError(t, err)
	require.False(t, response.IsError)
	var metadata AgentResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.True(t, metadata.Background)
	require.Regexp(t, `^a[0-9a-z]{8}$`, metadata.TaskID)
	require.Contains(t, response.Content, metadata.TaskID)
	require.Contains(t, response.Content, "Child session ID: "+metadata.ChildSessionID)
	child, err := env.sessions.Get(t.Context(), metadata.ChildSessionID)
	require.NoError(t, err)
	require.Equal(t, parent.ID, child.ParentSessionID)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background agent did not start")
	}
	cancelParent()
	stillRunning, status, err := coord.backgroundAgents.Output(t.Context(), metadata.TaskID, false, 0)
	require.NoError(t, err)
	require.Equal(t, managedtask.RetrievalNotReady, status)
	require.Equal(t, managedtask.StatusRunning, stillRunning.State.Status)

	close(release)
	output, err := coord.managedTaskOutput(t.Context(), metadata.TaskID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, "background result", output.Output)
	require.Equal(t, managedtask.StatusCompleted, output.Task.State.Status)
	require.Equal(t, int64(12), output.Task.Usage.PromptTokens)
	require.Equal(t, int64(5), output.Task.Usage.CompletionTokens)
	require.InDelta(t, 0.1, output.Task.Usage.Cost, 1e-9)
	require.Contains(t, coord.listManagedTasks(), output.Task)
}

func TestStartBackgroundSubAgentsFromSameParentRunConcurrently(t *testing.T) {
	env := testEnv(t)
	providerID := "parallel-background-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	coord.backgroundAgents = NewBackgroundAgentManager(env.workingDir)
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	started := make(chan string, 2)
	release := make(chan struct{})
	mockAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		started <- call.SessionID
		select {
		case <-release:
			return agentResultWithText(call.SessionID), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})

	responses := make([]fantasy.ToolResponse, 2)
	for index, toolCallID := range []string{"call-one", "call-two"} {
		responses[index], err = coord.startBackgroundSubAgent(t.Context(), AgentParams{Prompt: "work", RunInBackground: true}, fantasy.ToolCall{ID: toolCallID}, presetSubagent{agent: mockAgent, title: "Background"}, subAgentParams{
			Agent:          mockAgent,
			SessionID:      parent.ID,
			AgentMessageID: "message",
			ToolCallID:     toolCallID,
			Prompt:         "work",
			SessionTitle:   "Background",
		})
		require.NoError(t, err)
		require.False(t, responses[index].IsError)
	}

	childSessions := make(map[string]bool)
	for range responses {
		select {
		case childSessionID := <-started:
			childSessions[childSessionID] = true
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for parallel subagents")
		}
	}
	require.Len(t, childSessions, len(responses))
	require.Equal(t, len(responses), coord.backgroundAgents.ActiveCount())
	close(release)
	for _, response := range responses {
		var metadata AgentResponseMetadata
		require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
		_, status, outputErr := coord.backgroundAgents.Output(t.Context(), metadata.TaskID, true, time.Second)
		require.NoError(t, outputErr)
		require.Equal(t, managedtask.RetrievalReady, status)
	}
	require.Equal(t, 0, coord.backgroundAgents.ActiveCount())
}

func TestContinueBackgroundSubAgentAfterRestart(t *testing.T) {
	env := testEnv(t)
	providerID := "background-provider"
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "child", parent.ID, "Background")
	require.NoError(t, err)
	child.PromptTokens = 12
	child.CompletionTokens = 5
	child.Cost = 0.1
	child, err = env.sessions.Save(t.Context(), child)
	require.NoError(t, err)

	metadataRoot := filepath.Join(t.TempDir(), "metadata")
	recordStore, err := managedtask.NewStore(metadataRoot)
	require.NoError(t, err)
	manager, err := NewBackgroundAgentManagerWithStore(env.workingDir, nil, recordStore)
	require.NoError(t, err)
	source, err := manager.Reserve("first", config.AgentTask, "Background", managedtask.Ownership{ParentSessionID: parent.ID, OriginToolCallID: "first-call"})
	require.NoError(t, err)
	require.NoError(t, manager.Start(source, child.ID, func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "first result", Usage: managedtask.AgentUsage{PromptTokens: 12, CompletionTokens: 5, Cost: 0.1}}
	}))
	_, _, err = manager.Output(t.Context(), source.ID, true, time.Second)
	require.NoError(t, err)
	require.NoError(t, recordStore.Close())

	recordStore, err = managedtask.NewStore(metadataRoot)
	require.NoError(t, err)
	recovered, err := NewBackgroundAgentManagerWithStore(env.workingDir, nil, recordStore)
	require.NoError(t, err)
	coord.backgroundAgents = recovered
	source, ok := recovered.Get(source.ID)
	require.True(t, ok)
	sourceBefore := source.Info()

	requestingParent, err := env.sessions.Create(t.Context(), "Requesting parent")
	require.NoError(t, err)
	mockAgent := newMockAgent(providerID, 4096, func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		require.Equal(t, child.ID, call.SessionID)
		persisted, getErr := env.sessions.Get(ctx, call.SessionID)
		if getErr != nil {
			return nil, getErr
		}
		persisted.PromptTokens = 20
		persisted.CompletionTokens = 8
		persisted.Cost = 0.16
		if _, saveErr := env.sessions.Save(ctx, persisted); saveErr != nil {
			return nil, saveErr
		}
		return agentResultWithText("continued result"), nil
	})
	response, err := coord.continueBackgroundSubAgent(t.Context(), AgentParams{
		Prompt:          "continue",
		SubagentType:    config.AgentTask,
		RunInBackground: true,
		ContinueTaskID:  source.ID,
	}, fantasy.ToolCall{ID: "continue-call"}, presetSubagent{agent: mockAgent, title: "Background"}, subAgentParams{
		Agent:          mockAgent,
		SessionID:      requestingParent.ID,
		AgentMessageID: "message",
		ToolCallID:     "continue-call",
		Prompt:         "continue",
		SessionTitle:   "Background",
	}, source)
	require.NoError(t, err)
	require.False(t, response.IsError)
	var metadata AgentResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	require.NotEqual(t, source.ID, metadata.TaskID)
	require.Equal(t, child.ID, metadata.ChildSessionID)
	require.Contains(t, response.Content, metadata.TaskID)
	require.Contains(t, response.Content, "Child session ID: "+metadata.ChildSessionID)

	output, err := coord.managedTaskOutput(t.Context(), metadata.TaskID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusCompleted, output.Task.State.Status)
	require.Equal(t, source.ID, output.Task.ContinuationOf)
	require.Equal(t, "continued result", output.Output)
	require.Equal(t, int64(8), output.Task.Usage.PromptTokens)
	require.Equal(t, int64(3), output.Task.Usage.CompletionTokens)
	require.InDelta(t, 0.06, output.Task.Usage.Cost, 1e-9)
	require.Equal(t, sourceBefore, source.Info())
	updatedParent, err := env.sessions.Get(t.Context(), requestingParent.ID)
	require.NoError(t, err)
	require.InDelta(t, 0.06, updatedParent.Cost, 1e-9)
	require.NoError(t, recordStore.Close())

	recordStore, err = managedtask.NewStore(metadataRoot)
	require.NoError(t, err)
	reconstructed, err := NewBackgroundAgentManagerWithStore(env.workingDir, nil, recordStore)
	require.NoError(t, err)
	t.Cleanup(func() {
		reconstructed.StopAll(t.Context())
		require.NoError(t, recordStore.Close())
	})
	reconstructedInfo, status, err := reconstructed.Output(t.Context(), metadata.TaskID, false, 0)
	require.NoError(t, err)
	require.Equal(t, managedtask.RetrievalReady, status)
	require.Equal(t, source.ID, reconstructedInfo.ContinuationOf)
	require.Equal(t, child.ID, reconstructedInfo.ChildSessionID)
	require.Equal(t, int64(8), reconstructedInfo.Usage.PromptTokens)
}

func TestManagedTaskStopDispatchesAgent(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundAgentManager("workspace")
	coord := &coordinator{backgroundAgents: manager}
	backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	manager.Start(backgroundTask, "child", func(ctx context.Context) backgroundAgentResult {
		<-ctx.Done()
		return backgroundAgentResult{Err: ctx.Err()}
	})

	info, err := coord.stopManagedTask(t.Context(), backgroundTask.ID)
	require.NoError(t, err)
	require.Equal(t, managedtask.TypeAgent, info.Type)
	require.Equal(t, managedtask.StatusKilled, info.State.Status)
}

func TestBackgroundAgentRunFailure(t *testing.T) {
	t.Parallel()

	manager := NewBackgroundAgentManager("workspace")
	backgroundTask, err := manager.Reserve("prompt", "task", "description", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	manager.Start(backgroundTask, "child", func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Err: errors.New("provider failed")}
	})
	info, _, err := manager.Output(t.Context(), backgroundTask.ID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, managedtask.StatusFailed, info.State.Status)
	require.Equal(t, "agent_run_failed", info.State.ErrorCode)
	require.Equal(t, "provider failed", info.State.ErrorMessage)
}
