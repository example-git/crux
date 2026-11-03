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

func TestBuildLifecyclePreservesIdentitySelection(t *testing.T) {
	for _, mode := range []string{"all", "native", "project"} {
		t.Run(mode, func(t *testing.T) {
			for _, disabled := range []bool{false, true} {
				name := "enabled"
				if disabled {
					name = "disabled"
				}
				t.Run(name, func(t *testing.T) {
					options := &config.Options{InstructionMode: mode}
					if disabled {
						options.DisabledInstructionSections = []string{"identity"}
					}
					store := config.NewTestStore(&config.Config{Options: options})
					builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
					require.NoError(t, err)
					for _, lifecycle := range []Lifecycle{
						{Stage: LifecycleDefault},
						{Stage: LifecycleDraft},
						{Stage: LifecycleRevision, Plan: "Revise the plan"},
						{Stage: LifecycleExecution, Plan: "Execute the plan"},
					} {
						t.Run(string(lifecycle.Stage), func(t *testing.T) {
							instructions, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, lifecycle)
							require.NoError(t, err)
							if disabled || mode == "project" {
								require.NotContains(t, instructions, "You are a token engine:")
							} else {
								require.Contains(t, instructions, "You are a token engine:")
								require.Contains(t, instructions, "Your previous output is revisable work")
							}
						})
					}
				})
			}
		})
	}
}

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

func TestBuildLifecycleRendersActiveGoalAsSystemInstructions(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	instructions, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{
		Stage:          LifecycleDefault,
		GoalObjective:  `<ship & "verify">`,
		GoalStatus:     "active",
		GoalGeneration: 4,
	})
	require.NoError(t, err)
	require.Contains(t, instructions, `<goal_lifecycle status="active" generation="4">`)
	require.Contains(t, instructions, `&lt;ship &amp; &#34;verify&#34;&gt;`)
	require.NotContains(t, instructions, `<ship & "verify">`)
	require.Contains(t, instructions, "Continue working autonomously toward the persisted goal")
	require.Contains(t, instructions, "Continue from verified current state instead of replaying uncertain operations")
	require.Contains(t, instructions, `update_goal with status="complete"`)
}

func TestBuildLifecycleDoesNotActivateInactiveGoal(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	for _, status := range []string{"paused", "blocked", "complete"} {
		t.Run(status, func(t *testing.T) {
			instructions, buildErr := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{
				Stage:          LifecycleDefault,
				GoalObjective:  "Finish the integration",
				GoalStatus:     status,
				GoalGeneration: 2,
			})
			require.NoError(t, buildErr)
			require.Contains(t, instructions, `status="`+status+`"`)
			require.NotContains(t, instructions, "Continue working autonomously")
			require.Contains(t, instructions, "Finish the integration")
		})
	}
}

func TestBuildLifecycleSuspendsGoalDuringPlanMode(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	instructions, err := builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{
		Stage:          LifecycleExecution,
		Plan:           "Implement the approved plan",
		GoalObjective:  "Finish the integration",
		GoalStatus:     "active",
		GoalGeneration: 3,
	})
	require.NoError(t, err)
	require.Contains(t, instructions, `stage="execution"`)
	require.Contains(t, instructions, `status="active" generation="3" suspended="plan"`)
	require.Contains(t, instructions, "Follow the plan lifecycle only")
	require.NotContains(t, instructions, "Continue working autonomously")
}

func TestBuildLifecycleRejectsInvalidGoalState(t *testing.T) {
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	builder, err := NewPrompt("coder", lifecycleTestTemplate, WithWorkingDir(t.TempDir()))
	require.NoError(t, err)

	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDefault, GoalStatus: "active", GoalGeneration: 1})
	require.ErrorContains(t, err, "require an objective")
	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDefault, GoalObjective: "goal", GoalStatus: "active"})
	require.ErrorContains(t, err, "requires a positive generation")
	_, err = builder.BuildLifecycle(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDefault, GoalObjective: "goal", GoalStatus: "unknown", GoalGeneration: 1})
	require.ErrorContains(t, err, "invalid goal status")
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
