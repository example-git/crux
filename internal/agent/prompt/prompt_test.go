package prompt

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/projects"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestProviderInstructionsAppendMatchingFileAsDynamic(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("provider context\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "gemini.txt"), []byte("unrelated provider context\n"), 0o600))

	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	instructions, err := builder.BuildInstructions(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, instructions.String(), "generated prompt")
	require.Contains(t, instructions.String(), "provider context")
	require.NotContains(t, instructions.String(), "unrelated provider context")
	sections := instructions.Sections()
	require.Equal(t, fantasy.InstructionKindProviderContext, sections[len(sections)-1].Kind)
	require.Equal(t, fantasy.InstructionStabilityDynamic, sections[len(sections)-1].Stability)
	require.Equal(t, "provider context\n", sections[len(sections)-1].Text)
}

func TestProviderInstructionsPreserveRawText(t *testing.T) {
	dir := t.TempDir()
	providerText := "custom instructions\n\n<available_skills>user-owned text</available_skills>\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte(providerText), 0o600))
	active := []*skills.Skill{{Name: "imagegen", Description: "Generate images.", SkillFilePath: "crux://skills/imagegen/SKILL.md", Builtin: true}}
	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(dir), WithSkills(active))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	instructions, err := builder.BuildInstructions(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	sections := instructions.Sections()
	require.Equal(t, providerText, sections[len(sections)-1].Text)
	require.Contains(t, instructions.String(), "user-owned text")
	require.NotContains(t, instructions.String(), "<name>imagegen</name>")
}

func TestProviderInstructionsDoNotCreateMissingFile(t *testing.T) {
	dir := t.TempDir()
	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := builder.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "generated prompt", result)
	_, err = os.Stat(filepath.Join(dir, "codex.txt"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestProviderInstructionsDoNotReplaceGeneratedInstructions(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("provider context"), 0o600))
	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := builder.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "generated prompt\n\nprovider context", result)
	content, err := os.ReadFile(filepath.Join(dir, "codex.txt"))
	require.NoError(t, err)
	require.Equal(t, "provider context", string(content))
}

func TestProviderInstructionsDoNotApplyToSubagentPrompts(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "codex.txt"), []byte("replacement prompt"), 0o600))

	p, err := NewPrompt("task", "task prompt", withProviderInstructionsDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	result, err := p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Equal(t, "task prompt", result)
}

func TestProviderInstructionsRejectUnsafeProviderID(t *testing.T) {
	p, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(t.TempDir()))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	_, err = p.Build(t.Context(), "../codex", "gpt-5.6-sol", store)
	require.ErrorContains(t, err, "invalid provider ID")
}

func TestProviderInstructionsReportUnreadableFile(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(dir, "codex.txt"), 0o700))

	p, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(dir))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	_, err = p.Build(t.Context(), "codex", "gpt-5.6-sol", store)
	require.ErrorContains(t, err, "reading provider instructions")
}

func TestCoderInstructionsPlaceDynamicSectionsAfterCachedStaticSections(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	providerDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(providerDirectory, "openai.txt"), []byte("provider context"), 0o600))
	template := `{{if .Lifecycle}}{{.Lifecycle}}
{{end}}{{.NativeSections}}
<env>
Working directory: {{.WorkingDir}}
Platform: {{.Platform}}
{{if .RenderDate}}Today's date: {{.Date}}
{{end}}</env>`
	builder, err := NewPrompt(
		"coder",
		template,
		withProviderInstructionsDir(providerDirectory),
		WithWorkingDir(t.TempDir()),
		WithPlatform("darwin"),
		WithTimeFunc(func() time.Time { return time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC) }),
	)
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{InstructionMode: "all"}})

	instructions, err := builder.BuildLifecycleInstructions(t.Context(), "openai", "model", store, Lifecycle{Stage: LifecycleDraft})
	require.NoError(t, err)
	sections := instructions.Sections()
	dynamicStarted := false
	for _, section := range sections {
		if section.Stability == fantasy.InstructionStabilityDynamic {
			dynamicStarted = true
		} else {
			require.False(t, dynamicStarted, "static section %q followed a dynamic section", section.Kind)
		}
	}
	require.Equal(t, fantasy.InstructionKindTooling, sections[0].Kind)
	require.Equal(t, fantasy.InstructionStabilityStatic, sections[0].Stability)
	require.Equal(t, fantasy.InstructionKindEnvironment, sections[1].Kind)
	require.Equal(t, fantasy.InstructionStabilityStatic, sections[1].Stability)
	require.NotContains(t, sections[1].Text, "Today's date")
	require.Equal(t, fantasy.InstructionKindProviderContext, sections[2].Kind)
	require.Equal(t, fantasy.InstructionKindLifecycle, sections[3].Kind)
	require.Equal(t, fantasy.InstructionKindRuntime, sections[4].Kind)
	require.Equal(t, "Today's date: 8/29/2026", sections[4].Text)

	message := instructions.Message(fantasy.InstructionPolicyAnthropic)
	for index, part := range message.Content {
		text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
		require.True(t, ok)
		options := fantasy.InstructionPartOptionsFrom(text.ProviderOptions)
		require.NotNil(t, options)
		require.Equal(t, index == 1, options.CacheBoundary)
	}
}

func TestProviderInstructionsPrecedeDynamicRenderedTemplate(t *testing.T) {
	providerDirectory := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(providerDirectory, "openai.txt"), []byte("provider context"), 0o600))
	builder, err := NewPrompt(
		"coder",
		"generated on {{.Date}}",
		withProviderInstructionsDir(providerDirectory),
		WithTimeFunc(func() time.Time { return time.Date(2026, time.August, 29, 0, 0, 0, 0, time.UTC) }),
	)
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	instructions, err := builder.BuildInstructions(t.Context(), "openai", "model", store)
	require.NoError(t, err)
	sections := instructions.Sections()
	require.Len(t, sections, 2)
	require.Equal(t, fantasy.InstructionKindProviderContext, sections[0].Kind)
	require.Equal(t, fantasy.InstructionStabilityDynamic, sections[0].Stability)
	require.Equal(t, fantasy.InstructionKindEnvironment, sections[1].Kind)
	require.Equal(t, fantasy.InstructionStabilityDynamic, sections[1].Stability)
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

func TestProviderInstructionsRemainSeparateFromProjectAndMemory(t *testing.T) {
	providerDirectory := t.TempDir()
	memoryDirectory := t.TempDir()
	workingDir := t.TempDir()
	projectService := projects.NewServiceAt(t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", memoryDirectory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	require.NoError(t, os.WriteFile(filepath.Join(providerDirectory, "codex.txt"), []byte("provider context\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(memoryDirectory, "MEMORY.md"), []byte("- [Current](current.md) — current memory"), 0o600))
	_, err := projectService.Create(projects.Definition{
		Name:            "Instruction Project",
		Slug:            "instruction-project",
		Goal:            "Keep dynamic sections separate",
		SuccessCriteria: []string{"Project appears"},
		Tasks:           []projects.DefinitionTask{{ID: "T1", Content: "Inject project"}},
	}, workingDir)
	require.NoError(t, err)

	builder, err := NewPrompt("coder", "generated prompt", withProviderInstructionsDir(providerDirectory), WithWorkingDir(workingDir), withProjectService(projectService))
	require.NoError(t, err)
	store := config.NewTestStore(&config.Config{Options: &config.Options{}})

	instructions, err := builder.BuildInstructions(t.Context(), "codex", "gpt-5.6-sol", store)
	require.NoError(t, err)
	require.Contains(t, instructions.String(), "generated prompt")
	require.Contains(t, instructions.String(), "provider context")
	require.Contains(t, instructions.String(), "[Current](current.md)")
	require.Contains(t, instructions.String(), "Instruction Project")

	stability := make(map[fantasy.InstructionKind]fantasy.InstructionStability)
	positions := make(map[fantasy.InstructionKind]int)
	sections := instructions.Sections()
	for index, section := range sections {
		stability[section.Kind] = section.Stability
		positions[section.Kind] = index
	}
	require.Equal(t, fantasy.InstructionStabilityDynamic, stability[fantasy.InstructionKindProviderContext])
	for _, section := range sections {
		if section.Stability == fantasy.InstructionStabilityDynamic {
			require.Equal(t, fantasy.InstructionKindProviderContext, section.Kind)
			break
		}
	}
	require.Equal(t, fantasy.InstructionStabilityDynamic, stability[fantasy.InstructionKindProjectState])
	require.Equal(t, fantasy.InstructionStabilityDynamic, stability[fantasy.InstructionKindMemory])
	require.Less(t, positions[fantasy.InstructionKindProviderContext], positions[fantasy.InstructionKindProjectState])
	require.Less(t, positions[fantasy.InstructionKindProviderContext], positions[fantasy.InstructionKindMemory])
	content, err := os.ReadFile(filepath.Join(providerDirectory, "codex.txt"))
	require.NoError(t, err)
	require.Equal(t, "provider context\n", string(content))
}
