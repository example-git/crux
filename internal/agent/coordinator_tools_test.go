package agent

import (
	"testing"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestCoordinatorBuildsUnifiedSearchToolPalettes(t *testing.T) {
	coord := newGateTestCoordinator(t, true)
	palettes, err := coord.buildTools(t.Context(), coord.cfg.Config().Agents[config.AgentCoder], false)
	require.NoError(t, err)

	normal := paletteToolNames(palettes, false)
	planMode := paletteToolNames(palettes, true)
	require.Contains(t, normal, tools.SearchToolName)
	require.Contains(t, planMode, tools.SearchToolName)
	require.Contains(t, normal, tools.JQToolName)
	require.NotContains(t, planMode, tools.JQToolName)
	require.Contains(t, normal, tools.ImagegenToolName)
	require.NotContains(t, planMode, tools.ImagegenToolName)
	for _, oldName := range []string{"glob", "grep"} {
		require.NotContains(t, normal, oldName)
		require.NotContains(t, planMode, oldName)
	}
}

func TestCoordinatorSeparatesMainAndSubagentSearchPalettes(t *testing.T) {
	coord := newGateTestCoordinator(t, true)
	enabled := true
	coord.cfg.Config().Tools.CodebaseSearch.Enabled = &enabled

	mainPalettes, err := coord.buildTools(t.Context(), coord.cfg.Config().Agents[config.AgentCoder], false)
	require.NoError(t, err)
	main := paletteToolNames(mainPalettes, false)
	require.Contains(t, main, tools.CodebaseSearchToolName)
	require.Contains(t, main, tools.SearchToolName)

	subagentPalettes, err := coord.buildTools(t.Context(), coord.cfg.Config().Agents[config.AgentTask], true)
	require.NoError(t, err)
	subagent := paletteToolNames(subagentPalettes, false)
	require.NotContains(t, subagent, tools.CodebaseSearchToolName)
	require.Contains(t, subagent, tools.SearchToolName)
	require.Contains(t, subagent, tools.SourcegraphToolName)
	require.Contains(t, subagent, tools.FetchToolName)
	require.Contains(t, subagent, tools.LSToolName)
	require.Contains(t, subagent, tools.ViewToolName)
}

func TestResolveSubagentToolsKeepsDirectToolsAndRemovesLaunchers(t *testing.T) {
	agent := config.Agent{AllowedTools: []string{
		AgentToolName,
		tools.AgenticFetchToolName,
		tools.BashToolName,
		tools.CodebaseSearchToolName,
		tools.FetchToolName,
		tools.ImagegenToolName,
		tools.SearchToolName,
		tools.TaskContinueToolName,
		tools.TaskListToolName,
		tools.TaskOutputToolName,
		tools.TaskStopToolName,
		tools.TrafficCaptureToolName,
	}}
	resolved := resolveSubagentTools(agent, nil)
	require.ElementsMatch(t, []string{
		tools.BashToolName,
		tools.FetchToolName,
		tools.SearchToolName,
		tools.TaskListToolName,
		tools.TaskOutputToolName,
		tools.TaskStopToolName,
	}, resolved)
}

func TestResolveSubagentToolsHonorsDisabledDirectFetch(t *testing.T) {
	resolved := resolveSubagentTools(config.Agent{ID: config.AgentTask}, []string{tools.FetchToolName})
	require.NotContains(t, resolved, tools.FetchToolName)
}

func paletteToolNames(palette toolPalettes, planMode bool) []string {
	selected := palette.normal
	if planMode {
		selected = palette.planMode
	}
	result := make([]string, 0, len(selected))
	for _, tool := range selected {
		result = append(result, tool.Info().Name)
	}
	return result
}
