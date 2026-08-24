package prompt

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/projects"
	"github.com/stretchr/testify/require"
)

func TestPromptBuildUsesOnlyMatchingProviderSystemPromptOverride(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("replacement prompt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gemini.txt"), []byte("unrelated provider prompt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.bak"), []byte("backup prompt\n"), 0o600))

	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "replacement prompt\n", result)
}

func TestPromptBuildInitializesMissingProviderOverrideFromGeneratedPrompt(t *testing.T) {
	dir := t.TempDir()
	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "generated prompt", result)
	content, err := os.ReadFile(filepath.Join(dir, "codex.txt"))
	require.NoError(t, err)
	require.Equal(t, "generated prompt", string(content))
}

func TestPromptBuildIgnoresOverrideWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("replacement prompt"), 0o600))

	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "generated prompt", result)
}

func TestPromptBuildDoesNotOverrideSubagentPrompts(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("replacement prompt"), 0o600))

	p, err := NewPrompt("task", "task prompt", withSystemPromptOverrideDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "task prompt", result)
}

func TestPromptBuildRejectsUnsafeProviderOverrideID(t *testing.T) {
	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	_, err = p.Build(t.Context(), "../codex", "gpt-5.6-sol", store)
	require.ErrorContains(t, err, "invalid provider ID")
}

func TestPromptBuildReportsUnreadableProviderOverride(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "codex.txt"), 0o700))

	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	_, err = p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.ErrorContains(t, err, "reading system prompt override")
}

func TestCoderPromptBuildIncludesPersistentMemory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MEMORY.md"), []byte("- [Preferences](preferences.md) — collaboration preferences"), 0o600))

	p, err := NewPrompt("coder", "base prompt", WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, result, "base prompt")
	require.Contains(t, result, "<persistent_memory>")
	require.Contains(t, result, "[Preferences](preferences.md)")
}

func TestTaskPromptBuildOmitsPersistentMemory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")

	p, err := NewPrompt("task", "task prompt", WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "task prompt", result)
}

func TestCoderPromptReloadsMemoryIndex(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	p, err := NewPrompt("coder", "base prompt", WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	projectEmpty := "## Project MEMORY.md\n\nThe memory index is currently empty."
	userEmpty := "## User MEMORY.md\n\nThe memory index is currently empty."

	first, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, first, projectEmpty)
	require.Contains(t, first, userEmpty)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "MEMORY.md"), []byte("- [New](new.md) — newly saved memory"), 0o600))

	second, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, second, "[New](new.md)")
	require.NotContains(t, second, projectEmpty)
	require.Contains(t, second, userEmpty)
}

func TestCoderPromptRejectsInvalidMemoryConfiguration(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "sometimes")
	p, err := NewPrompt("coder", "base prompt", WithWorkingDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	_, err = p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.ErrorContains(t, err, "CRUX_DISABLE_AUTO_MEMORY must be a boolean")
}

func TestCoderPromptIncludesAndReloadsSelectedProjectFiles(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	workingDir := t.TempDir()
	service := projects.NewServiceAt(t.TempDir())
	_, err := service.Create(projects.Definition{
		Name:            "Prompt Project",
		Slug:            "prompt-project",
		Goal:            "Keep durable project context current",
		SuccessCriteria: []string{"Prompt includes current project state"},
		Tasks: []projects.DefinitionTask{
			{ID: "T1", Content: "Build prompt context"},
			{ID: "T1.1", Content: "Include notes", ParentID: "T1"},
		},
	}, workingDir)
	require.NoError(t, err)

	p, err := NewPrompt("coder", "base prompt", WithWorkingDir(workingDir), withProjectService(service))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	first, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, first, "<persistent_project>")
	require.Contains(t, first, "Current goal: `T1` Build prompt context")
	require.Contains(t, first, "- `T1.1` Include notes")
	require.Contains(t, first, "prompt-project.md")
	require.Contains(t, first, "prompt-project.notes.md")
	require.Contains(t, first, "Use the existing `todos` tool")
	require.Contains(t, first, "Keep Projects separate from plans")

	_, err = service.AppendNotes(workingDir, "Fresh note content")
	require.NoError(t, err)
	second, err := p.BuildLifecycle(t.Context(), "codex", "gpt-5.6-sol", store, Lifecycle{Stage: LifecycleDraft})
	require.NoError(t, err)
	require.Contains(t, second, "Fresh note content")
	require.Contains(t, second, "<persistent_project>")

	require.NoError(t, service.Disable(workingDir))
	third, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.NotContains(t, third, "<persistent_project>")
}

func TestTaskPromptOmitsSelectedProject(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	workingDir := t.TempDir()
	service := projects.NewServiceAt(t.TempDir())
	_, err := service.Create(projects.Definition{
		Name:            "Prompt Project",
		Slug:            "prompt-project",
		Goal:            "Stay in coder prompt",
		SuccessCriteria: []string{"Subagent prompt remains unchanged"},
		Tasks:           []projects.DefinitionTask{{ID: "T1", Content: "Test prompt scope"}},
	}, workingDir)
	require.NoError(t, err)

	p, err := NewPrompt("task", "task prompt", WithWorkingDir(workingDir), withProjectService(service))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})
	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "task prompt", result)
}

func TestPromptOverrideKeepsMemoryAndProjectDynamic(t *testing.T) {
	overrideDirectory := t.TempDir()
	memoryDirectory := t.TempDir()
	workingDir := t.TempDir()
	projectService := projects.NewServiceAt(t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", memoryDirectory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	require.NoError(t, os.WriteFile(filepath.Join(overrideDirectory, "codex.txt"), []byte("replacement prompt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(memoryDirectory, "MEMORY.md"), []byte("- [Current](current.md) — current memory"), 0o600))
	_, err := projectService.Create(projects.Definition{
		Name:            "Override Project",
		Slug:            "override-project",
		Goal:            "Remain dynamic with overrides",
		SuccessCriteria: []string{"Project appears"},
		Tasks:           []projects.DefinitionTask{{ID: "T1", Content: "Inject project"}},
	}, workingDir)
	require.NoError(t, err)

	p, err := NewPrompt("coder", "generated prompt", withSystemPromptOverrideDir(overrideDirectory), WithWorkingDir(workingDir), withProjectService(projectService))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{SystemPromptOverride: true}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, result, "replacement prompt")
	require.Contains(t, result, "[Current](current.md)")
	require.Contains(t, result, "Override Project")
	require.Contains(t, result, "override-project.notes.md")
	override, err := os.ReadFile(filepath.Join(overrideDirectory, "codex.txt"))
	require.NoError(t, err)
	require.Equal(t, "replacement prompt\n", string(override))
}
