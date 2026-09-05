package tools

import (
	"context"
	"io/fs"
	"testing"

	"github.com/example-git/crux/internal/history"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

type externalCheckpointHistory struct {
	*mockHistoryService
	calls int
}

func (h *externalCheckpointHistory) Checkpoint(ctx context.Context, sessionID, messageID, path, content string, exists bool, mode fs.FileMode) error {
	h.calls++
	if h.calls == 1 {
		return history.ErrSnapshotOutsideWorkspace
	}
	return nil
}

type snapshotPermissionService struct {
	*mockPermissionService
	granted bool
	request permission.CreatePermissionRequest
}

func (s *snapshotPermissionService) Request(_ context.Context, request permission.CreatePermissionRequest) (bool, error) {
	s.request = request
	return s.granted, nil
}

func TestCheckpointFilePromptsAndIncludesExternalSnapshot(t *testing.T) {
	files := &externalCheckpointHistory{mockHistoryService: &mockHistoryService{}}
	permissions := &snapshotPermissionService{
		mockPermissionService: &mockPermissionService{},
		granted:               true,
	}
	ctx := context.WithValue(t.Context(), MessageIDContextKey, "message")

	err := checkpointFile(ctx, files, permissions, "session", "tool-call", "/outside/file.txt", "", false, 0)
	require.NoError(t, err)
	require.Equal(t, 2, files.calls)
	require.Equal(t, "snapshot", permissions.request.ToolName)
	require.Equal(t, "include", permissions.request.Action)
	require.True(t, permissions.request.RequireExplicitApproval)
	require.True(t, permissions.request.AllowDetachedPrompt)
}

func TestCheckpointFileSkipsExternalSnapshotWhenDenied(t *testing.T) {
	files := &externalCheckpointHistory{mockHistoryService: &mockHistoryService{}}
	permissions := &snapshotPermissionService{
		mockPermissionService: &mockPermissionService{},
		granted:               false,
	}
	ctx := context.WithValue(t.Context(), MessageIDContextKey, "message")

	err := checkpointFile(ctx, files, permissions, "session", "tool-call", "/outside/file.txt", "", false, 0)
	require.NoError(t, err)
	require.Equal(t, 1, files.calls)
}
