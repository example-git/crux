package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

const lifecycleTestTemplate = `{{if .Lifecycle}}{{.Lifecycle}}
{{end}}{{.NativeSections}}
{{range .ContextFiles}}{{.Content}}{{end}}`

func TestBuildLifecycleSelectsCentralInstructionProfile(t *testing.T) {
	contextFile := filepath.Join(t.TempDir(), "PROJECT.md")
	require.NoError(t, os.WriteFile(contextFile, []byte("project-specific guidance"), 0o600))
	store := config.NewTestStore(&config.Config{Options: &config.Options{
		ContextPaths:    []string{contextFile},
		InstructionMode: "all",
	}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	draft, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDraft})
	require.NoError(t, err)
	require.Contains(t, draft, `stage="draft"`)
	require.Contains(t, draft, "project-specific guidance")
	require.Contains(t, draft, "<critical_rules>")
	require.Contains(t, draft, "<memory_instructions>")
	require.NotContains(t, draft, "<editing_files>")
	require.NotContains(t, draft, "<task_completion>")

	revision, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleRevision, Plan: "Persisted revised plan"})
	require.NoError(t, err)
	require.Contains(t, revision, `stage="revision"`)
	require.Contains(t, revision, "<persisted_plan>\nPersisted revised plan")
	require.NotContains(t, revision, "<editing_files>")

	execution, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleExecution, Plan: "Approved implementation plan"})
	require.NoError(t, err)
	require.Contains(t, execution, `stage="execution"`)
	require.Contains(t, execution, "<approved_plan>\nApproved implementation plan")
	require.Contains(t, execution, "<editing_files>")
	require.Contains(t, execution, "<task_completion>")
	require.Contains(t, execution, "ordinary permission requests are automatically approved")
	require.Contains(t, execution, "Automatic tool approval does not bypass hooks or completion review")
	require.Contains(t, execution, "project-specific guidance")
}

func TestBuildLifecycleRebuildsFromCurrentState(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	revision, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleRevision, Plan: "First plan"})
	require.NoError(t, err)
	execution, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleExecution, Plan: "Approved replacement plan"})
	require.NoError(t, err)

	require.Contains(t, revision, "First plan")
	require.NotContains(t, revision, "Approved replacement plan")
	require.Contains(t, execution, "Approved replacement plan")
	require.NotContains(t, execution, "First plan")
}

func TestBuildLifecycleAppendsProviderInstructionsWithoutReplacingLifecycle(t *testing.T) {
	providerDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(providerDirectory, "codex.txt"), []byte("provider context"), 0o600))
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, withProviderInstructionsDir(providerDirectory), WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	normal, err := builder.Build(t.Context(), "codex", "model", store)
	require.NoError(t, err)
	require.Contains(t, normal, "<critical_rules>")
	require.Contains(t, normal, "provider context")

	draft, err := builder.BuildLifecycle(t.Context(), "codex", "model", store, Lifecycle{Stage: LifecycleDraft})
	require.NoError(t, err)
	require.Contains(t, draft, `stage="draft"`)
	require.Contains(t, draft, "<critical_rules>")
	require.Contains(t, draft, "provider context")
}

func TestBuildLifecycleRejectsInvalidState(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleRevision})
	require.ErrorContains(t, err, "requires a persisted plan")
	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDefault, Plan: "unexpected"})
	require.ErrorContains(t, err, "cannot include a persisted plan")
	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleStage("unknown")})
	require.ErrorContains(t, err, "invalid lifecycle stage")
}
