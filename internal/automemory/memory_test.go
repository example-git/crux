package automemory

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestLoadCreatesManagedProjectMemory(t *testing.T) {
	globalData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", globalData)
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	workingDir := t.TempDir()

	memory, err := Load(t.Context(), workingDir)
	require.NoError(t, err)
	require.True(t, memory.Managed)
	require.DirExists(t, memory.Directory)
	require.Equal(t, filepath.Join(memory.Directory, EntrypointName), memory.Entrypoint)
	require.Empty(t, memory.Content)
	require.Equal(t, filepath.Join(globalData, "memory"), memory.UserDirectory)
	require.Equal(t, filepath.Join(memory.UserDirectory, EntrypointName), memory.UserEntrypoint)
	require.DirExists(t, memory.UserDirectory)
	require.True(t, strings.HasPrefix(memory.Directory, filepath.Join(globalData, "projects")))
}

func TestLoadReadsBoundedEntrypoint(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("CRUX_AUTO_MEMORY_DIR", directory)
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	lines := make([]string, MaxEntrypointLines+1)
	for index := range lines {
		lines[index] = "- [Topic](topic.md) - " + strings.Repeat("界", 50)
	}
	require.NoError(t, os.WriteFile(filepath.Join(directory, EntrypointName), []byte(strings.Join(lines, "\n")), 0o600))

	memory, err := Load(t.Context(), t.TempDir())
	require.NoError(t, err)
	require.False(t, memory.Managed)
	require.Contains(t, memory.Content, "Only part was loaded")
	require.True(t, utf8.ValidString(memory.Content))
	require.LessOrEqual(t, bytes.Count([]byte(memory.Content), []byte("\n")), MaxEntrypointLines+2)
}

func TestLoadCanBeDisabled(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", t.TempDir())

	memory, err := Load(t.Context(), t.TempDir())
	require.NoError(t, err)
	require.Empty(t, memory)
}

func TestDirectoryRejectsRelativeOverride(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", "relative/memory")

	_, _, err := Directory(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "must be a safe absolute path")
}

func TestDirectoryRejectsFilesystemRootOverride(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", filepath.VolumeName(t.TempDir())+string(filepath.Separator))

	_, _, err := Directory(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "cannot be a filesystem root")
}

func TestLoadRejectsInvalidDisableValue(t *testing.T) {
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "sometimes")
	t.Setenv("CRUX_AUTO_MEMORY_DIR", t.TempDir())

	_, err := Load(t.Context(), t.TempDir())
	require.ErrorContains(t, err, "must be a boolean")
}

func TestPromptDescribesIndexAndTopicWorkflow(t *testing.T) {
	memory := Memory{
		Directory:     "/memory/project",
		Entrypoint:    "/memory/project/MEMORY.md",
		Content:       "- [Testing](testing.md) - test preferences",
		Managed:       true,
		UserDirectory: "/memory/user",
		UserContent:   "- [Editor](editor.md) - editor preferences",
	}

	result := Prompt(memory)
	require.Contains(t, result, "<persistent_memory>")
	require.Contains(t, result, "type: {{user|feedback|project|reference}}")
	require.Contains(t, result, "[Testing](testing.md)")
	require.Contains(t, result, "[Editor](editor.md)")
	require.Contains(t, result, "Before every memory mutation, inspect the target scope with memory_list")
	require.Contains(t, result, "update that topic to incorporate the new durable information")
	require.Contains(t, result, "Remove memories that are no longer relevant")
	require.Contains(t, result, "roughly 30 to 50 memories per scope")
	require.Contains(t, result, "soft target, not a hard limit")
	require.NotContains(t, result, "use the view tool")
	require.Contains(t, result, "Trust current evidence over memory")
}

func TestSanitizePathBoundsLongComponents(t *testing.T) {
	path := "/" + strings.Repeat("deep-path/", 40)
	result := sanitizePath(path)
	require.LessOrEqual(t, len(result), 213)
	require.NotContains(t, result, "/")
	require.Equal(t, result, sanitizePath(path))
}
