package agent

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestForegroundAgentTransfer(t *testing.T) {
	for _, stop := range []bool{false, true} {
		t.Run(map[bool]string{false: "completion", true: "stop"}[stop], func(t *testing.T) {
			env := testEnv(t)
			providerID := "foreground-provider"
			coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
			coord.backgroundShells = shell.NewBackgroundShellManager(env.workingDir)
			coord.backgroundAgents = NewBackgroundAgentManager(env.workingDir)
			parent, err := env.sessions.Create(t.Context(), "Parent")
			require.NoError(t, err)
			started := make(chan context.Context, 1)
			release := make(chan struct{})
			var calls atomic.Int32
			mockAgent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(ctx context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
				calls.Add(1)
				started <- ctx
				select {
				case <-release:
					return agentResultWithText("transferred result"), nil
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			})
			parentCtx, cancelParent := context.WithCancel(t.Context())
			defer cancelParent()
			type result struct {
				response fantasy.ToolResponse
				err      error
			}
			results := make(chan result, 1)
			go func() {
				response, err := coord.runDetachableSubAgent(parentCtx, AgentParams{Prompt: "work"}, presetSubagent{agent: mockAgent, title: "Foreground"}, subAgentParams{
					Agent: mockAgent, SessionID: parent.ID, AgentMessageID: "message", ToolCallID: "call", Prompt: "work", SessionTitle: "Foreground",
				})
				results <- result{response, err}
			}()
			var runCtx context.Context
			select {
			case runCtx = <-started:
			case <-time.After(time.Second):
				t.Fatal("agent did not start")
			}
			require.False(t, permission.IsDetachedAgent(runCtx))
			require.Zero(t, coord.backgroundShells.ForegroundWaits.Detach("other-session"))
			require.Eventually(t, func() bool { return coord.backgroundShells.ForegroundWaits.Detach(parent.ID) == 1 }, time.Second, time.Millisecond)
			var output result
			select {
			case output = <-results:
			case <-time.After(time.Second):
				t.Fatal("foreground did not return")
			}
			require.NoError(t, output.err)
			require.False(t, output.response.IsError, output.response.Content)
			var metadata AgentResponseMetadata
			require.NoError(t, json.Unmarshal([]byte(output.response.Metadata), &metadata))
			require.True(t, metadata.Background)
			require.NotEmpty(t, metadata.TaskID)
			require.True(t, permission.IsDetachedAgent(runCtx))
			_, deadline := runCtx.Deadline()
			require.False(t, deadline)
			cancelParent()
			require.NoError(t, runCtx.Err())
			if stop {
				info, err := coord.backgroundAgents.Stop(t.Context(), metadata.TaskID)
				require.NoError(t, err)
				require.Equal(t, managedtask.StatusKilled, info.State.Status)
			} else {
				close(release)
				finished, err := coord.TaskOutput(t.Context(), metadata.TaskID, true, time.Second)
				require.NoError(t, err)
				require.Equal(t, managedtask.StatusCompleted, finished.Task.State.Status)
				require.Equal(t, "transferred result", finished.Output)
			}
			require.Equal(t, int32(1), calls.Load())
			require.Zero(t, coord.backgroundShells.ForegroundWaits.Count(parent.ID))
		})
	}
}

func TestForegroundTaskOutputWaitTransfer(t *testing.T) {
	manager := NewBackgroundAgentManager(t.TempDir())
	backgroundTask, err := manager.Reserve("work", "task", "wait", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	release := make(chan struct{})
	require.NoError(t, manager.Start(backgroundTask, "child", func(ctx context.Context) backgroundAgentResult {
		select {
		case <-release:
			return backgroundAgentResult{Output: "done"}
		case <-ctx.Done():
			return backgroundAgentResult{Err: ctx.Err()}
		}
	}))
	env := testEnv(t)
	coord := &coordinator{sessions: env.sessions, messages: env.messages, backgroundAgents: manager, backgroundShells: shell.NewBackgroundShellManager(t.TempDir())}
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, "parent")
	type result struct {
		output managedtask.OutputResult
		err    error
	}
	results := make(chan result, 1)
	go func() {
		output, err := coord.TaskOutput(ctx, backgroundTask.ID, true, time.Minute)
		results <- result{output, err}
	}()
	require.Eventually(t, func() bool { return coord.backgroundShells.ForegroundWaits.Detach("parent") == 1 }, time.Second, time.Millisecond)
	select {
	case output := <-results:
		require.NoError(t, output.err)
		require.Equal(t, managedtask.StatusRunning, output.output.Task.State.Status)
	case <-time.After(time.Second):
		t.Fatal("output wait did not return")
	}
	close(release)
	output, err := coord.TaskOutput(ctx, backgroundTask.ID, true, time.Second)
	require.NoError(t, err)
	require.Equal(t, "done", output.Output)
	require.Zero(t, coord.backgroundShells.ForegroundWaits.Count("parent"))
}
