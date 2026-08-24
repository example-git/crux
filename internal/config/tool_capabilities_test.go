package config

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEveryBuiltinToolHasCapabilityPolicy(t *testing.T) {
	for _, name := range allToolNames() {
		capabilities, ok := ToolCapabilities(name)
		require.Truef(t, ok, "missing capability policy for %s", name)
		require.NotEmptyf(t, capabilities, "empty capability policy for %s", name)
	}
	require.Len(t, builtinToolPolicies, len(allToolNames()))
}

func TestPlanModeKeepsApplicationServicesAndBlocksMutators(t *testing.T) {
	allowed := FilterPlanModeTools(allToolNames())
	for _, name := range []string{"enter_plan", "exit_plan", "memory_list", "memory_upsert", "memory_remove", "skill_list", "skill_load", "todos", "git_inspect", "job_list", "job_output", "task_list", "task_output"} {
		require.Contains(t, allowed, name)
	}
	for _, name := range []string{"bash", "download", "edit", "multiedit", "write", "lsp_rename", "lsp_replace_symbol", "lsp_restart", "job_kill", "task_stop", "task_continue", "agent", "complete_plan"} {
		require.NotContains(t, allowed, name)
	}
	require.False(t, IsToolAllowedInPlanMode("unknown_dynamic_tool"))
}

func TestProjectToolsUseApplicationStateCapabilities(t *testing.T) {
	for _, name := range []string{"project_complete", "project_create", "project_notes", "project_update"} {
		capabilities, ok := ToolCapabilities(name)
		require.True(t, ok)
		require.Equal(t, []string{capabilityApplicationWrite}, capabilities)
	}
	capabilities, ok := ToolCapabilities("project_status")
	require.True(t, ok)
	require.Equal(t, []string{capabilityApplicationRead}, capabilities)
}

func TestToolCapabilitiesReturnsCopy(t *testing.T) {
	capabilities, ok := ToolCapabilities("memory_upsert")
	require.True(t, ok)
	require.True(t, slices.Contains(capabilities, capabilityApplicationWrite))
	capabilities[0] = "changed"
	original, ok := ToolCapabilities("memory_upsert")
	require.True(t, ok)
	require.NotEqual(t, capabilities, original)
}
