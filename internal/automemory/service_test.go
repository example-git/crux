package automemory

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestMemoryServiceManagesProjectAndUserScopes(t *testing.T) {
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	service := NewService(t.TempDir())
	projectContent := strings.TrimSpace(strings.Repeat("project context ", 400))

	project, err := service.Upsert(t.Context(), ScopeProject, Entry{
		File:        "release-decisions",
		Name:        "Release decisions",
		Description: "Use when planning a release",
		Type:        "project",
		Content:     projectContent,
	})
	require.NoError(t, err)
	require.Equal(t, "release-decisions.md", project.File)
	require.Equal(t, projectContent, project.Content)

	user, err := service.Upsert(t.Context(), ScopeUser, Entry{
		File:        "testing-preference.md",
		Name:        "Testing preference",
		Description: "Use when selecting validation commands",
		Type:        "feedback",
		Content:     "Prefer focused tests with explicit timeouts.",
	})
	require.NoError(t, err)
	require.Equal(t, "testing-preference.md", user.File)

	projectEntries, err := service.List(t.Context(), ScopeProject)
	require.NoError(t, err)
	require.Len(t, projectEntries, 1)
	require.Equal(t, project.File, projectEntries[0].File)
	require.Empty(t, projectEntries[0].Content)

	userEntries, err := service.List(t.Context(), ScopeUser)
	require.NoError(t, err)
	require.Len(t, userEntries, 1)
	require.Equal(t, user.File, userEntries[0].File)

	projectMemory, err := service.resolve(t.Context(), ScopeProject, false)
	require.NoError(t, err)
	projectIndex, err := os.ReadFile(projectMemory.Entrypoint)
	require.NoError(t, err)
	require.Contains(t, string(projectIndex), "[Release decisions](release-decisions.md)")

	userIndex, err := os.ReadFile(filepath.Join(UserDirectory(), EntrypointName))
	require.NoError(t, err)
	require.Contains(t, string(userIndex), "[Testing preference](testing-preference.md)")

	require.NoError(t, service.Remove(t.Context(), ScopeProject, project.File))
	projectEntries, err = service.List(t.Context(), ScopeProject)
	require.NoError(t, err)
	require.Empty(t, projectEntries)
	userEntries, err = service.List(t.Context(), ScopeUser)
	require.NoError(t, err)
	require.Len(t, userEntries, 1)
}

func TestMemoryServiceRejectsInvalidMutations(t *testing.T) {
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	service := NewService(t.TempDir())
	valid := Entry{File: "topic", Name: "Topic", Description: "Relevant topic", Type: "project", Content: "Content"}

	_, err := service.Upsert(t.Context(), Scope("system"), valid)
	require.ErrorContains(t, err, "invalid memory scope")
	valid.File = "../escape"
	_, err = service.Upsert(t.Context(), ScopeProject, valid)
	require.ErrorContains(t, err, "invalid memory topic")
	valid.File = "topic"
	valid.Type = "secret"
	_, err = service.Upsert(t.Context(), ScopeProject, valid)
	require.ErrorContains(t, err, "invalid memory type")
	valid.Type = "project"
	valid.Content = strings.Repeat("x", maxMemoryContentBytes+1)
	_, err = service.Upsert(t.Context(), ScopeProject, valid)
	require.ErrorContains(t, err, "invalid memory content")
	err = service.Remove(t.Context(), ScopeProject, "missing")
	require.ErrorContains(t, err, "does not exist")
}

func TestMemoryServiceDoesNotMutateCustomProjectDirectory(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "existing.md"), []byte("---\nname: Existing\ndescription: Existing custom memory\ntype: project\n---\n\nRead only."), 0o600))
	service := NewService(t.TempDir())

	entries, err := service.List(t.Context(), ScopeProject)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	_, err = service.Upsert(t.Context(), ScopeProject, Entry{File: "new", Name: "New", Description: "New memory", Type: "project", Content: "Content"})
	require.ErrorContains(t, err, "unavailable for custom memory directories")
	err = service.Remove(t.Context(), ScopeProject, "existing")
	require.ErrorContains(t, err, "unavailable for custom memory directories")
}
