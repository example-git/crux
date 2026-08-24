package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

type denyPermissionService struct {
	mockPermissionService
}

func (d *denyPermissionService) Request(context.Context, permission.CreatePermissionRequest) (bool, error) {
	return false, nil
}

func TestSearchToolsRequirePermissionBeforeReadingOutsideWorkspace(t *testing.T) {
	workingDir := t.TempDir()
	outsideDir := t.TempDir()
	secretPath := filepath.Join(outsideDir, "secret-value.txt")
	require.NoError(t, os.WriteFile(secretPath, []byte("private contents"), 0o600))
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")
	permissions := &denyPermissionService{}

	testCases := map[string]fantasy.AgentTool{
		"glob": NewGlobTool(permissions, workingDir, config.ToolGlob{}),
		"grep": NewGrepTool(permissions, workingDir, config.ToolGrep{}),
	}
	inputs := map[string]any{
		"glob": GlobParams{Pattern: "*", Path: outsideDir},
		"grep": GrepParams{Pattern: "private", Path: outsideDir},
	}
	for name, tool := range testCases {
		t.Run(name, func(t *testing.T) {
			input, err := json.Marshal(inputs[name])
			require.NoError(t, err)
			response, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: name, Input: string(input)})
			require.NoError(t, err)
			require.NotContains(t, response.Content, "secret-value")
			require.NotContains(t, response.Content, "private contents")
			require.True(t, strings.Contains(strings.ToLower(response.Content), "permission") || response.IsError)
		})
	}
}

func TestDeniedFileCreationDoesNotCreateParentDirectories(t *testing.T) {
	workingDir := t.TempDir()
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")
	permissions := &denyPermissionService{}
	files := &mockHistoryService{}
	tracker := &mockEditFileTracker{}

	t.Run("edit", func(t *testing.T) {
		target := filepath.Join(workingDir, "edit", "file.txt")
		_, err := createNewFile(editContext{ctx: ctx, permissions: permissions, files: files, filetracker: tracker, workingDir: workingDir}, target, "content", fantasy.ToolCall{ID: "call"})
		require.NoError(t, err)
		require.NoDirExists(t, filepath.Dir(target))
	})

	t.Run("multiedit", func(t *testing.T) {
		target := filepath.Join(workingDir, "multiedit", "file.txt")
		_, err := processMultiEditWithCreation(editContext{ctx: ctx, permissions: permissions, files: files, filetracker: tracker, workingDir: workingDir}, MultiEditParams{
			FilePath: target,
			Edits:    []MultiEditOperation{{NewString: "content"}},
		}, fantasy.ToolCall{ID: "call"})
		require.NoError(t, err)
		require.NoDirExists(t, filepath.Dir(target))
	})

	t.Run("write", func(t *testing.T) {
		target := filepath.Join(workingDir, "write", "file.txt")
		tool := NewWriteTool(nil, permissions, files, mockFileTrackerService{}, workingDir)
		input, err := json.Marshal(WriteParams{FilePath: target, Content: "content"})
		require.NoError(t, err)
		_, err = tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: WriteToolName, Input: string(input)})
		require.NoError(t, err)
		require.NoDirExists(t, filepath.Dir(target))
	})
}
