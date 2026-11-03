package agent

import (
	"context"
	"testing"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/subagentscript"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestSubagentScriptServiceEnforcesSessionOwnership(t *testing.T) {
	env := testEnv(t)
	parent, err := env.sessions.Create(t.Context(), "parent")
	require.NoError(t, err)
	other, err := env.sessions.Create(t.Context(), "other")
	require.NoError(t, err)
	child, err := env.sessions.CreateTaskSession(t.Context(), "child", parent.ID, "child")
	require.NoError(t, err)
	coord := &coordinator{sessions: env.sessions, backgroundAgents: &BackgroundAgentManager{
		tasks: map[string]*BackgroundAgentTask{
			"a12345678": {ID: "a12345678", Ownership: managedtask.Ownership{ParentSessionID: parent.ID}, childSessionID: child.ID, state: managedtask.State{Status: managedtask.StatusCompleted}},
			"a87654321": {ID: "a87654321", Ownership: managedtask.Ownership{ParentSessionID: other.ID}, childSessionID: "other-child", state: managedtask.State{Status: managedtask.StatusCompleted}},
			"a11111111": {ID: "a11111111", Ownership: managedtask.Ownership{ParentSessionID: parent.ID}, childSessionID: "sibling", state: managedtask.State{Status: managedtask.StatusCompleted}},
		},
	}}
	for _, sessionID := range []string{parent.ID, child.ID} {
		ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, sessionID)
		service, err := coord.scriptService(ctx)
		require.NoError(t, err)
		listed, err := service.Call(t.Context(), subagentscript.Request{Action: "list"})
		require.NoError(t, err)
		if sessionID == parent.ID {
			require.Len(t, listed, 2)
		} else {
			require.Len(t, listed, 1)
			_, err = service.Call(t.Context(), subagentscript.Request{Action: "start", Prompt: "nested"})
			require.Error(t, err)
			_, err = service.Call(t.Context(), subagentscript.Request{Action: "output", TaskID: "a11111111"})
			require.ErrorContains(t, err, "not owned")
		}
		for _, action := range []string{"output", "wait", "stop", "continue"} {
			request := subagentscript.Request{Action: action, TaskID: "a87654321"}
			if action == "continue" {
				request.Prompt = "continue"
			}
			_, err := service.Call(t.Context(), request)
			require.ErrorContains(t, err, "not owned")
		}
	}
}

func TestSubagentScriptToolIsAvailableOnlyToMainAgent(t *testing.T) {
	coord := newGateTestCoordinator(t, true)
	main, err := coord.buildTools(t.Context(), coord.cfg.Config().Agents["coder"], false)
	require.NoError(t, err)
	require.Contains(t, paletteToolNames(main, false), SubagentScriptToolName)
	require.NotContains(t, paletteToolNames(main, true), SubagentScriptToolName)
	child, err := coord.buildTools(t.Context(), coord.cfg.Config().Agents["task"], true)
	require.NoError(t, err)
	require.NotContains(t, paletteToolNames(child, false), SubagentScriptToolName)
}
