package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

type recordingAgentPermissionService struct {
	permission.Service
	granted  bool
	err      error
	requests []permission.CreatePermissionRequest
}

func (s *recordingAgentPermissionService) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	s.requests = append(s.requests, request)
	return s.granted, s.err
}

func TestAgentToolRefreshesDefinitionsWithoutRebuild(t *testing.T) {
	workingDir := t.TempDir()
	userDir := filepath.Join(t.TempDir(), "user")
	projectDir := filepath.Join(workingDir, ".ai-cli", "agents")
	validationCfg := testAgentDefinitionConfig()
	cfg := initTestConfig(t, workingDir)
	cfg.SetupAgents()
	coord := &coordinator{
		cfg: cfg,
		loadAgentDefinitions: func(_ string, _ *config.Config) ([]agentDefinition, error) {
			return loadAgentDefinitionsFromDirs(
				userDir,
				projectDir,
				validationCfg,
			)
		},
	}
	taskCfg := cfg.Config().Agents[config.AgentTask]
	taskAgent := &mockSessionAgent{}
	taskPreset := presetSubagent{
		agent: taskAgent,
		title: "New Agent Session",
	}
	tool := newRefreshingAgentTool(coord, taskCfg, taskPreset)
	selected, err := coord.resolveAgentToolPreset(
		t.Context(),
		taskPreset,
		"",
	)
	require.NoError(t, err)
	require.Same(t, taskAgent, selected.agent)
	writeDefinition := func(dir, description, toolsField string) string {
		t.Helper()
		require.NoError(t, os.MkdirAll(dir, 0o755))
		path := filepath.Join(dir, "reviewer.md")
		content := "---\n" +
			"name: reviewer\n" +
			"description: " + description + "\n" +
			"model: provider/model\n" +
			toolsField + "\n" +
			"---\n\nReview the requested change.\n"
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		return path
	}

	require.NotContains(t, tool.Info().Description, "- reviewer:")
	userPath := writeDefinition(userDir, "User reviewer", "tools: []")
	require.Contains(t, tool.Info().Description, "- reviewer: User reviewer")

	writeDefinition(userDir, "Updated user reviewer", "tools: []")
	require.Contains(t, tool.Info().Description, "- reviewer: Updated user reviewer")
	require.NotContains(t, tool.Info().Description, "- reviewer: User reviewer")

	projectPath := writeDefinition(projectDir, "Project reviewer", "tools: []")
	require.Contains(t, tool.Info().Description, "- reviewer: Project reviewer")
	require.NotContains(t, tool.Info().Description, "Updated user reviewer")

	writeDefinition(projectDir, "Broken project reviewer", "toolz: []")
	require.Contains(
		t,
		tool.Info().Description,
		"- reviewer: invalid definition at "+projectPath,
	)
	toolContext := context.WithValue(
		t.Context(),
		tools.SessionIDContextKey,
		"session",
	)
	toolContext = context.WithValue(
		toolContext,
		tools.MessageIDContextKey,
		"message",
	)
	response, err := tool.Run(toolContext, fantasy.ToolCall{
		ID:    "invalid-definition",
		Input: `{"prompt":"review","subagent_type":"reviewer"}`,
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, "field toolz not found")

	require.NoError(t, os.Remove(projectPath))
	require.Contains(t, tool.Info().Description, "- reviewer: Updated user reviewer")
	require.NoError(t, os.Remove(userPath))
	require.NotContains(t, tool.Info().Description, "- reviewer:")

	response, err = tool.Run(toolContext, fantasy.ToolCall{
		ID:    "deleted-definition",
		Input: `{"prompt":"review","subagent_type":"reviewer"}`,
	})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(
		t,
		response.Content,
		`unknown subagent type "reviewer"; available types: task`,
	)
}

func TestAgentToolDescriptionsAndApprovalUseResolvedSubagentTools(t *testing.T) {
	cfg := initTestConfig(t, t.TempDir())
	definition := agentDefinition{
		Name:        "reviewer",
		Description: "Review changes",
		Model:       config.SelectedModel{Provider: "provider", Model: "model"},
		Tools: []string{
			AgentToolName,
			tools.AgenticFetchToolName,
			tools.CodebaseSearchToolName,
			tools.ImagegenToolName,
			tools.ViewToolName,
		},
	}
	coord := &coordinator{
		cfg: cfg,
		loadAgentDefinitions: func(string, *config.Config) ([]agentDefinition, error) {
			return []agentDefinition{definition}, nil
		},
	}
	taskCfg := config.Agent{
		ID:          config.AgentTask,
		Description: "General research",
		AllowedTools: []string{
			tools.AgenticFetchToolName,
			tools.CodebaseSearchToolName,
			tools.ViewToolName,
		},
	}

	description := coord.currentAgentToolDescription(taskCfg)
	require.Contains(t, description, "- task: General research (model: configured large model; tools: view, fetch)")
	require.Contains(t, description, "- reviewer: Review changes (model: provider/model; tools: view)")
	require.NotContains(t, description, tools.AgenticFetchToolName)
	require.NotContains(t, description, tools.CodebaseSearchToolName)

	resolved := coord.customAgentTools(definition)
	require.Equal(t, []string{tools.ViewToolName}, resolved)
	require.False(t, subagentRequiresApproval(resolved))
}

func TestAgentToolRejectsAllLaunchModesFromSubagent(t *testing.T) {
	coord := &coordinator{}
	ctx := permission.WithSubagent(t.Context())
	for _, test := range []struct {
		name   string
		params AgentParams
	}{
		{name: "foreground", params: AgentParams{Prompt: "work"}},
		{name: "background", params: AgentParams{Prompt: "work", RunInBackground: true}},
		{name: "continuation", params: AgentParams{Prompt: "continue", RunInBackground: true, ContinueTaskID: "a12345678"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			response, err := coord.runAgentTool(ctx, test.params, fantasy.ToolCall{ID: "call"}, presetSubagent{})
			require.NoError(t, err)
			require.True(t, response.IsError)
			require.Equal(t, permission.ErrSubagentLaunch.Error(), response.Content)
		})
	}
}

func TestAgentToolAllowsTopLevelForegroundLaunch(t *testing.T) {
	const providerID = "test-provider"
	env := testEnv(t)
	coord := newTestCoordinator(t, env, providerID, config.ProviderConfig{ID: providerID})
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	subagent := newMockAgent(providerID, 4096, coord.cfg.RuntimeSnapshot(), func(_ context.Context, call SessionAgentCall) (*fantasy.AgentResult, error) {
		require.Equal(t, "work", call.Prompt)
		return agentResultWithText("done"), nil
	})
	ctx := context.WithValue(permission.WithDetachedAgent(t.Context()), tools.SessionIDContextKey, parent.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "message")

	response, err := coord.runAgentTool(ctx, AgentParams{Prompt: "work"}, fantasy.ToolCall{ID: "call"}, presetSubagent{agent: subagent, title: "Task"})
	require.NoError(t, err)
	require.False(t, response.IsError)
	require.Equal(t, "done", response.Content)
}

func TestCoordinatorContinueTaskRejectsSubagent(t *testing.T) {
	_, err := (&coordinator{}).ContinueTask(permission.WithSubagent(t.Context()), "a12345678", "parent", "continue", "call")
	require.ErrorIs(t, err, permission.ErrSubagentBackgroundTask)
}

func TestApproveBackgroundSubagentScopesWritableRuns(t *testing.T) {
	workingDir := t.TempDir()
	service := &recordingAgentPermissionService{granted: true}
	coord := &coordinator{cfg: initTestConfig(t, workingDir), permissions: service}

	approved, err := coord.approveBackgroundSubagent(t.Context(), presetSubagent{
		title:            "Agent: reviewer",
		tools:            []string{"view", "edit"},
		requiresApproval: true,
	}, "parent", "call")
	require.NoError(t, err)
	require.True(t, approved)
	require.Len(t, service.requests, 1)
	require.Equal(t, AgentToolName, service.requests[0].ToolName)
	require.Equal(t, "delegate", service.requests[0].Action)
	require.Equal(t, workingDir, service.requests[0].Path)

	approved, err = coord.approveBackgroundSubagent(t.Context(), presetSubagent{
		title: "Agent: reader",
		tools: []string{"view", "search"},
	}, "parent", "other-call")
	require.NoError(t, err)
	require.False(t, approved)
	require.Len(t, service.requests, 1)

	service.granted = false
	_, err = coord.approveBackgroundSubagent(t.Context(), presetSubagent{
		title:            "Agent: writer",
		tools:            []string{"write"},
		requiresApproval: true,
	}, "parent", "denied-call")
	require.ErrorContains(t, err, "permission denied for Agent: writer")
}

func TestSubagentRequiresApprovalDefaultsUnknownToolsToWritable(t *testing.T) {
	require.False(t, subagentRequiresApproval([]string{"view", "search", "lsp_definition"}))
	require.True(t, subagentRequiresApproval([]string{"view", "edit"}))
	require.True(t, subagentRequiresApproval([]string{"third_party_tool"}))
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
	available := []string{"view", "write", "search", AgentToolName, "agentic_fetch"}

	resolved, err := resolveCustomAgentTools(config.Agent{DefinitionPath: "none.md"}, available, nil)
	require.NoError(t, err)
	require.Empty(t, resolved)

	resolved, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "readonly.md",
		AllowedTools:   []string{"view", "search"},
	}, available, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"view", "search"}, resolved)

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
	require.Equal(t, []string{"search", "view"}, resolved)

	_, err = resolveCustomAgentTools(config.Agent{
		DefinitionPath: "unavailable.md",
		AllowedTools:   []string{"lsp_definition"},
	}, available, nil)
	require.EqualError(t, err, `agent definition "unavailable.md": tool "lsp_definition" is not available to subagents in this workspace`)
}
