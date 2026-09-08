package config

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestCollectRemoteRuntimePreservesClientControls(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	require.NoError(t, store.SetConfigFields(ScopeGlobal, map[string]any{
		"options.disable_auto_summarize":        true,
		"options.summarization_context_cap":     49152,
		"options.summarization_max_tokens":      2048,
		"options.summarization_fast_mode":       true,
		"options.codex_compaction_v2":           true,
		"options.instruction_mode":              "native",
		"options.disabled_instruction_sections": []string{"identity", "memory"},
		"options.response_verbosity":            "high",
		"options.analysis_effort":               "max",
	}))
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	want := RemoteRuntimeControls{
		DisableAutoSummarize: true, SummarizationContextCap: 49152, SummarizationMaxTokens: 2048,
		SummarizationFastMode: true, CodexCompactionV2: true, InstructionMode: "native",
		DisabledInstructionSections: []string{"identity", "memory"}, ResponseVerbosity: "high", AnalysisEffort: "max",
	}
	require.Equal(t, want, proposal.Controls)
	root := t.TempDir()
	receiver, err := CompileRemoteRuntime(root, filepath.Join(root, "scoped-data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	options := receiver.Config().Options
	require.True(t, options.DisableAutoSummarize)
	require.EqualValues(t, 49152, options.SummarizationContextCap)
	require.EqualValues(t, 2048, options.SummarizationMaxTokens)
	require.True(t, options.SummarizationFastMode)
	require.True(t, options.CodexCompactionV2)
	require.Equal(t, "native", options.InstructionMode)
	require.Equal(t, []string{"identity", "memory"}, options.DisabledInstructionSections)
	require.Equal(t, "high", options.ResponseVerbosity)
	require.Equal(t, "max", options.AnalysisEffort)
	require.Equal(t, filepath.Join(root, "scoped-data"), options.DataDirectory)
	proposal.Controls.DisabledInstructionSections[0] = "mutated"
	require.Equal(t, []string{"identity", "memory"}, options.DisabledInstructionSections)
	again, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, want, again.Controls)
}

func TestRemoteRuntimeControlsRejectInvalidReplacement(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	proposal.Controls = RemoteRuntimeControls{InstructionMode: "project", AnalysisEffort: "high"}
	proposal = sealRemoteRuntime(t, proposal)
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	for _, invalid := range []RemoteRuntimeControls{
		{InstructionMode: "hidden"}, {AnalysisEffort: "unlimited"}, {ResponseVerbosity: "verbose"},
		{SummarizationMaxTokens: -1}, {SummarizationContextCap: -1},
	} {
		candidate := proposal
		candidate.Revision, candidate.Controls = 2, invalid
		candidate = sealRemoteRuntime(t, candidate)
		_, err := store.ReplaceRemoteRuntime(t.Context(), candidate, principal, 1)
		require.Error(t, err)
		require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
		require.Same(t, before.Config(), store.Config())
	}
	proposal.Revision, proposal.Controls = 2, RemoteRuntimeControls{InstructionMode: "native", AnalysisEffort: "low"}
	proposal = sealRemoteRuntime(t, proposal)
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	require.Equal(t, "high", before.Config().Options.AnalysisEffort)
	require.Equal(t, "project", before.Config().Options.InstructionMode)
	require.Equal(t, "low", store.Config().Options.AnalysisEffort)
	require.Equal(t, "native", store.Config().Options.InstructionMode)
}

func TestClientRuntimeSettingValidation(t *testing.T) {
	for key, value := range map[string]any{
		"options.analysis_effort":               "unlimited",
		"options.response_verbosity":            1,
		"options.instruction_mode":              "hidden",
		"options.disable_auto_summarize":        "true",
		"options.summarization_context_cap":     -1,
		"options.summarization_max_tokens":      1.5,
		"options.disabled_instruction_sections": []int{1},
	} {
		require.Error(t, ValidateClientRuntimeSetting(key, value), key)
	}
	require.NoError(t, ValidateClientRuntimeSetting("options.analysis_effort", "none"))
	require.NoError(t, ValidateClientRuntimeSetting("options.summarization_context_cap", 0))
	require.NoError(t, ValidateClientRuntimeSetting("options.disable_auto_summarize", false))
	require.Error(t, ValidateClientRuntimeSetting("authorization", true))
}
