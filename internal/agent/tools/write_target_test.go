package tools

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

type replacingWritePermissionService struct {
	*mockPermissionService
	replace func() error
}

func (service *replacingWritePermissionService) Request(_ context.Context, _ permission.CreatePermissionRequest) (bool, error) {
	if service.replace != nil {
		err := service.replace()
		service.replace = nil
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

type recordingCheckpointHistory struct {
	*mockHistoryService
	calls int
}

func (history *recordingCheckpointHistory) Checkpoint(_ context.Context, _, _, _, _ string, _ bool, _ fs.FileMode) error {
	history.calls++
	return nil
}

func TestEditRejectsLeafReplacementDuringPermission(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "target.txt")
	moved := filepath.Join(directory, "authorized.txt")
	require.NoError(t, os.WriteFile(path, []byte("authorized"), 0o644))

	permissions := &replacingWritePermissionService{
		mockPermissionService: &mockPermissionService{},
		replace: func() error {
			if err := os.Rename(path, moved); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o644)
		},
	}
	history := &recordingCheckpointHistory{mockHistoryService: &mockHistoryService{}}
	edit := editContext{
		ctx:         context.WithValue(t.Context(), SessionIDContextKey, "session"),
		permissions: permissions,
		files:       history,
		filetracker: &mockEditFileTracker{lastRead: time.Now().Add(time.Second)},
		workingDir:  directory,
	}

	_, err := replaceContent(edit, path, "authorized", "changed", false, fantasy.ToolCall{ID: "call"})
	require.ErrorContains(t, err, "changed after capture")
	require.Zero(t, history.calls)
	requireFileContent(t, moved, "authorized")
	requireFileContent(t, path, "replacement")
}

func TestWriteRejectsParentReplacementDuringPermission(t *testing.T) {
	workingDirectory := t.TempDir()
	directory := filepath.Join(workingDirectory, "parent")
	moved := filepath.Join(workingDirectory, "authorized-parent")
	path := filepath.Join(directory, "target.txt")
	require.NoError(t, os.Mkdir(directory, 0o755))
	require.NoError(t, os.WriteFile(path, []byte("authorized"), 0o644))

	permissions := &replacingWritePermissionService{
		mockPermissionService: &mockPermissionService{},
		replace: func() error {
			if err := os.Rename(directory, moved); err != nil {
				return err
			}
			if err := os.Mkdir(directory, 0o755); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0o644)
		},
	}
	history := &recordingCheckpointHistory{mockHistoryService: &mockHistoryService{}}
	tool := NewWriteTool(nil, permissions, history, mockFileTrackerService{}, workingDirectory)
	input := `{"file_path":"parent/target.txt","content":"changed"}`
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "session")

	_, err := tool.Run(ctx, fantasy.ToolCall{ID: "call", Name: WriteToolName, Input: input})
	require.ErrorContains(t, err, "changed after capture")
	require.Zero(t, history.calls)
	requireFileContent(t, filepath.Join(moved, "target.txt"), "authorized")
	requireFileContent(t, path, "replacement")
}

func requireFileContent(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, string(content))
}
