package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/agent/prompt"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/session"
	"github.com/stretchr/testify/require"
)

func TestCoderPromptIncludesPersistentMemory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MEMORY.md"), []byte("- [Preferences](preferences.md) — collaboration preferences"), 0o600))
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "native"}})
	coder, err := coderPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	result, err := coder.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, result, "<persistent_memory>")
	require.Contains(t, result, "[Preferences](preferences.md)")
}

func TestCoderPromptBuildsLifecycleProfilesCentrally(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	coder, err := coderPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	draft, err := coder.BuildLifecycle(t.Context(), "openai", "model", store, prompt.Lifecycle{Stage: prompt.LifecycleDraft})
	require.NoError(t, err)
	require.Contains(t, draft, `<plan_lifecycle stage="draft">`)
	require.Contains(t, draft, "<env>")
	require.Contains(t, draft, "<critical_rules>")
	require.NotContains(t, draft, "<editing_files>")

	execution, err := coder.BuildLifecycle(t.Context(), "openai", "model", store, prompt.Lifecycle{Stage: prompt.LifecycleExecution, Plan: "Approved plan"})
	require.NoError(t, err)
	require.Contains(t, execution, `<plan_lifecycle stage="execution">`)
	require.Contains(t, execution, "<approved_plan>\nApproved plan")
	require.Contains(t, execution, "<editing_files>")
}

func TestCoderPromptUsesEvidenceBasedCorrectionGuidance(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "native"}})
	coder, err := coderPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	for _, lifecycle := range []prompt.Lifecycle{
		{Stage: prompt.LifecycleDefault},
		{Stage: prompt.LifecycleDraft},
		{Stage: prompt.LifecycleRevision, Plan: "Revise the implementation plan"},
		{Stage: prompt.LifecycleExecution, Plan: "Implement the requested outcome"},
	} {
		t.Run(string(lifecycle.Stage), func(t *testing.T) {
			result, err := coder.BuildLifecycle(t.Context(), "openai", "model", store, lifecycle)
			require.NoError(t, err)
			if lifecycle.Stage == prompt.LifecycleDefault {
				require.Contains(t, result, "You have no position, reputation, or prior answer to defend.")
				require.Contains(t, result, "The user owns the requirements and reports their observed behavior")
			}
			for _, expected := range []string{
				"When corrected, stop acting on the disputed interpretation.",
				"Treat reported observations as evidence requiring investigation.",
				"Distinguish the requested outcome from a proposed diagnosis of its cause.",
				"Verify disputed diagnoses with evidence rather than defending them or agreeing automatically.",
				"Acknowledging feedback while retaining the rejected behavior is a failure.",
				"unrequested gates, fallbacks, substitutions, or narrower success conditions",
				"Change conclusions when evidence warrants it",
			} {
				require.Contains(t, result, expected)
			}
			if lifecycle.Stage == prompt.LifecycleDefault || lifecycle.Stage == prompt.LifecycleExecution {
				require.Contains(t, result, "Distinguish source inspection, automated tests, and live verification")
				require.Contains(t, result, "Avoid formulaic validation, repeated apologies, flattery")
			}
			for _, rejected := range []string{
				"The user is always better at diagnosing",
				"When your output contradicts the user's observation, your output is wrong.",
				"no intelligence to protect",
				"no intelligence to apply",
				"no right answers to defend",
				"no right answers to protect",
			} {
				require.NotContains(t, result, rejected)
			}
		})
	}
}

func TestPromptLifecycleMapsPersistedSessionState(t *testing.T) {
	tests := []struct {
		mode  session.Mode
		plan  string
		stage prompt.LifecycleStage
	}{
		{mode: session.ModeDefault, stage: prompt.LifecycleDefault},
		{mode: session.ModePlan, stage: prompt.LifecycleDraft},
		{mode: session.ModePlanRevision, plan: "revision", stage: prompt.LifecycleRevision},
		{mode: session.ModePlanExecution, plan: "approved", stage: prompt.LifecycleExecution},
	}
	for _, test := range tests {
		lifecycle, err := promptLifecycle(session.Session{Mode: test.mode, Plan: test.plan})
		require.NoError(t, err)
		require.Equal(t, test.stage, lifecycle.Stage)
		require.Equal(t, test.plan, lifecycle.Plan)
	}

	_, err := promptLifecycle(session.Session{Mode: session.Mode("unknown")})
	require.ErrorContains(t, err, "invalid session mode")
}

func TestCustomAgentPromptKeepsInstructionsLiteralAndIsolated(t *testing.T) {
	workingDir := t.TempDir()
	store := config.NewTestStore(&config.Config{
		Options: &config.Options{
			InstructionMode: "all",
			ContextPaths:    []string{"AGENTS.md"},
		},
	})
	require.NoError(t, os.WriteFile(filepath.Join(workingDir, "AGENTS.md"), []byte("coder-only project context"), 0o600))
	instructions := "# Independent reviewer\n\nKeep `{{.Literal}}`, punctuation?!, and **Markdown** unchanged."
	custom, err := customAgentPrompt(instructions, prompt.WithWorkingDir(workingDir))
	require.NoError(t, err)

	result, err := custom.Build(t.Context(), "provider", "model", store)
	require.NoError(t, err)
	require.Contains(t, result, instructions)
	require.Contains(t, result, "Working directory: "+filepath.ToSlash(workingDir))
	require.NotContains(t, result, "coder-only project context")
	require.NotContains(t, result, "<plan_lifecycle")
	require.NotContains(t, result, "<persistent_memory>")
	require.NotContains(t, result, "<available_skills>")
}

func TestCoderPromptDoesNotRunOrInjectGitStatus(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	workingDir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(workingDir, ".git"), 0o700))
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "native"}})
	coder, err := coderPrompt(prompt.WithWorkingDir(workingDir))
	require.NoError(t, err)

	result, err := coder.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, result, "Is directory a git repo: yes")
	require.NotContains(t, result, "Git status (snapshot")
	require.NotContains(t, result, "Recent commits:")
}

func TestCoderPromptDoesNotInjectCodexRuntimeControls(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	store := config.NewTestStore(&config.Config{Options: &config.Options{
		ResponseVerbosity: "high",
		AnalysisEffort:    "max",
	}})
	coder, err := coderPrompt(prompt.WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	result, err := coder.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.False(t, strings.HasPrefix(result, "<runtime_metadata>"))
	require.NotContains(t, result, "Analysis-budget metadata")
}
