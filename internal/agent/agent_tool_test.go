package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestAgentToolSelectsGenericAndCustomPresetsStrictly(t *testing.T) {
	task := &mockSessionAgent{}
	reviewer := &mockSessionAgent{}
	presets := map[string]presetSubagent{
		config.AgentTask: {agent: task, title: "New Agent Session"},
		"reviewer":       {agent: reviewer, title: "Agent: reviewer"},
	}
	available := []string{"reviewer", "task"}

	selected, err := selectPresetSubagent(presets, "", available)
	require.NoError(t, err)
	require.Same(t, task, selected.agent)
	require.Equal(t, "New Agent Session", selected.title)

	selected, err = selectPresetSubagent(presets, "reviewer", available)
	require.NoError(t, err)
	require.Same(t, reviewer, selected.agent)
	require.Equal(t, "Agent: reviewer", selected.title)

	_, err = selectPresetSubagent(presets, "missing", available)
	require.EqualError(t, err, `unknown subagent type "missing"; available types: reviewer, task`)
}

func TestAgentToolReturnsDefinitionErrorOnlyForSelectedPreset(t *testing.T) {
	task := &mockSessionAgent{}
	validationErr := errors.New(`agent definition "reviewer.md": field toolz not found`)
	presets := map[string]presetSubagent{
		config.AgentTask: {agent: task, title: "New Agent Session"},
		"reviewer":       {title: "Agent: reviewer", err: validationErr},
	}
	available := []string{"reviewer", "task"}

	selected, err := selectPresetSubagent(presets, "", available)
	require.NoError(t, err)
	require.Same(t, task, selected.agent)

	_, err = selectPresetSubagent(presets, "reviewer", available)
	require.ErrorIs(t, err, validationErr)
	require.ErrorContains(t, err, "field toolz not found")
}

func TestResolveBackgroundAgentContinuation(t *testing.T) {
	t.Parallel()

	env := testEnv(t)
	coord := &coordinator{sessions: env.sessions, backgroundAgents: NewBackgroundAgentManager(env.workingDir)}
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	ownership := managedtask.Ownership{ParentSessionID: parent.ID}

	_, _, err = coord.resolveBackgroundAgentContinuation(t.Context(), "invalid", "")
	require.ErrorContains(t, err, "invalid task ID")
	_, _, err = coord.resolveBackgroundAgentContinuation(t.Context(), "b12345678", "")
	require.ErrorContains(t, err, "not a background agent")

	running, err := coord.backgroundAgents.Reserve("running", config.AgentTask, "Running", ownership)
	require.NoError(t, err)
	_, _, err = coord.resolveBackgroundAgentContinuation(t.Context(), running.ID, "")
	require.ErrorContains(t, err, "is not terminal")
	coord.backgroundAgents.FailReservation(running, errors.New("failed before session creation"))
	_, _, err = coord.resolveBackgroundAgentContinuation(t.Context(), running.ID, "")
	require.ErrorContains(t, err, "transcript is unavailable")

	child, err := env.sessions.CreateTaskSession(t.Context(), "child", parent.ID, "Background")
	require.NoError(t, err)
	terminal, err := coord.backgroundAgents.Reserve("done", "reviewer", "Done", ownership)
	require.NoError(t, err)
	require.NoError(t, coord.backgroundAgents.Start(terminal, child.ID, func(context.Context) backgroundAgentResult {
		return backgroundAgentResult{Output: "done"}
	}))
	_, _, err = coord.backgroundAgents.Output(t.Context(), terminal.ID, true, time.Second)
	require.NoError(t, err)
	_, _, err = coord.resolveBackgroundAgentContinuation(t.Context(), terminal.ID, config.AgentTask)
	require.EqualError(t, err, `subagent_type "task" does not match continuation agent type "reviewer"`)
	resolved, agentType, err := coord.resolveBackgroundAgentContinuation(t.Context(), terminal.ID, "")
	require.NoError(t, err)
	require.Same(t, terminal, resolved)
	require.Equal(t, "reviewer", agentType)
}

func TestAgentToolResolvesCustomToolAllowlist(t *testing.T) {
	available := []string{"view", "write", "grep", AgentToolName, "agentic_fetch"}

	resolved, err := resolveCustomAgentTools(config.Agent{DefinitionPath: "none.md"}, available, nil)
	require.NoError(t, err)
	require.Empty(t, resolved)

	resolved, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "readonly.md",
		AllowedTools:   []string{"view", "grep"},
	}, available, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"view", "grep"}, resolved)

	resolved, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "script.md",
		AllowedTools:   []string{"script"},
	}, []string{"bash", "script", "view"}, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"script"}, resolved)

	resolved, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "mutating.md",
		AllowedTools:   []string{"write"},
	}, available, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"write"}, resolved)

	resolved, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "wildcard.md",
		AllowAllTools:  true,
	}, available, []string{"write"})
	require.NoError(t, err)
	require.Equal(t, []string{"grep", "view"}, resolved)

	_, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "unavailable.md",
		AllowedTools:   []string{"lsp_definition"},
	}, available, nil)
	require.EqualError(t, err, `agent definition "unavailable.md": tool "lsp_definition" is not available to subagents in this workspace`)
}
