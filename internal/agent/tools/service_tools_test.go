package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/require"
)

func TestMemoryToolsManageBothScopes(t *testing.T) {
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	service := automemory.NewService(t.TempDir())
	upsert := NewMemoryUpsertTool(service)
	list := NewMemoryListTool(service)
	remove := NewMemoryRemoveTool(service)

	response := runServiceTool(t, upsert, MemoryUpsertToolName, MemoryUpsertParams{
		Scope:       "user",
		Topic:       "validation",
		Name:        "Validation preference",
		Description: "Use when selecting tests",
		Type:        "feedback",
		Content:     "Use focused tests with timeouts.",
	})
	require.Contains(t, response.Content, "Saved user memory validation.md")

	response = runServiceTool(t, list, MemoryListToolName, MemoryListParams{Scope: "user"})
	require.Contains(t, response.Content, `"file": "validation.md"`)
	response = runServiceTool(t, list, MemoryListToolName, MemoryListParams{Scope: "user", Topic: "validation"})
	require.Contains(t, response.Content, "Use focused tests with timeouts.")

	response = runServiceTool(t, remove, MemoryRemoveToolName, MemoryRemoveParams{Scope: "user", Topic: "validation"})
	require.Contains(t, response.Content, "Removed user memory validation")
	response = runServiceTool(t, list, MemoryListToolName, MemoryListParams{Scope: "user"})
	require.Equal(t, "[]", response.Content)
}

func TestMemoryToolDescriptionsRequireScopeReviewAndNaturalPruning(t *testing.T) {
	require.Contains(t, memoryListDescription, "Always inspect the target scope before an upsert or removal")
	require.Contains(t, memoryUpsertDescription, "use `memory_list` to inspect the target scope")
	require.Contains(t, memoryUpsertDescription, "update that topic with the new durable information")
	require.Contains(t, memoryUpsertDescription, "roughly 30 to 50 memories")
	require.Contains(t, memoryUpsertDescription, "soft target rather than a hard limit")
	require.Contains(t, memoryRemoveDescription, "use `memory_list` to inspect the target scope")
	require.Contains(t, memoryRemoveDescription, "Do not remove useful memory solely to meet a target count")
}

func TestMemoryToolsRejectInvalidScope(t *testing.T) {
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	response := runServiceTool(t, NewMemoryListTool(automemory.NewService(t.TempDir())), MemoryListToolName, MemoryListParams{Scope: "global"})
	require.Contains(t, response.Content, "expected project or user")
}

func TestSkillToolsListAndLoadActiveSkill(t *testing.T) {
	workingDir := t.TempDir()
	skillDirectory := filepath.Join(workingDir, ".agents", "skills", "testing")
	require.NoError(t, os.MkdirAll(skillDirectory, 0o700))
	skillPath := filepath.Join(skillDirectory, skills.SkillFileName)
	content := "---\nname: testing\ndescription: Use for focused validation\n---\n\nRun focused tests."
	require.NoError(t, os.WriteFile(skillPath, []byte(content), 0o600))
	active := []*skills.Skill{{Name: "testing", Description: "Use for focused validation", Instructions: "Run focused tests.", Path: skillDirectory, SkillFilePath: skillPath}}
	tracker := skills.NewTracker(active)

	response := runServiceTool(t, NewSkillListTool(active, []string{filepath.Dir(skillDirectory)}, workingDir, tracker), SkillListToolName, struct{}{})
	require.Contains(t, response.Content, `"name": "testing"`)
	require.Contains(t, response.Content, `"loaded": false`)

	response = runServiceTool(t, NewSkillLoadTool(active, []string{filepath.Dir(skillDirectory)}, workingDir, tracker), SkillLoadToolName, SkillLoadParams{Name: "testing"})
	require.Equal(t, content, response.Content)
	require.True(t, tracker.IsLoaded("testing"))

	response = runServiceTool(t, NewSkillListTool(active, []string{filepath.Dir(skillDirectory)}, workingDir, tracker), SkillListToolName, struct{}{})
	require.Contains(t, response.Content, `"loaded": true`)
}

func TestGitInspectDescriptionDiscouragesRoutineRepositoryScans(t *testing.T) {
	description := string(gitInspectDescription)
	require.Contains(t, description, "Do not use it for routine repository discovery")
	require.Contains(t, description, "prefer a relevant path-scoped status or diff")
}

func TestGitInspectArgsAreBoundedAndReadOnly(t *testing.T) {
	args, err := gitInspectArgs(GitInspectParams{Action: "status", Path: "internal/agent"})
	require.NoError(t, err)
	require.Contains(t, args, "status")
	require.Contains(t, args, "--untracked-files=normal")
	require.NotContains(t, args, "--untracked-files=all")
	require.Contains(t, args, "--")

	args, err = gitInspectArgs(GitInspectParams{Action: "diff", Staged: true})
	require.NoError(t, err)
	require.Contains(t, args, "--cached")
	require.Contains(t, args, "--no-ext-diff")

	args, err = gitInspectArgs(GitInspectParams{Action: "log", Limit: 100})
	require.NoError(t, err)
	require.Contains(t, args, "-100")

	args, err = gitInspectArgs(GitInspectParams{Action: "show", Revision: "HEAD~2"})
	require.NoError(t, err)
	require.Contains(t, args, "HEAD~2")

	_, err = gitInspectArgs(GitInspectParams{Action: "diff", Path: "../outside"})
	require.ErrorContains(t, err, "within the current workspace")
	_, err = gitInspectArgs(GitInspectParams{Action: "show", Revision: "--help"})
	require.ErrorContains(t, err, "invalid Git revision")
	_, err = gitInspectArgs(GitInspectParams{Action: "log", Limit: 101})
	require.ErrorContains(t, err, "between 1 and 100")
	_, err = gitInspectArgs(GitInspectParams{Action: "reset"})
	require.ErrorContains(t, err, "invalid Git action")
}

func runServiceTool(t *testing.T, tool fantasy.AgentTool, name string, params any) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	response, err := tool.Run(context.Background(), fantasy.ToolCall{ID: "test", Name: name, Input: string(input)})
	require.NoError(t, err)
	return response
}
