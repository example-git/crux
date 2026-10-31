package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestSearchWorkspaceFiles(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Join(root, "needle-dir"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "src"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "needle-dir", "value.txt"), []byte("value"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "needle-file.txt"), []byte("needle"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "src", "other.txt"), []byte("other"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored-needle.txt\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "ignored-needle.txt"), []byte("ignored"), 0o600))

	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)
	response, err := backend.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "needle",
		Roots: []string{"."},
		Limit: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{realRoot}, response.Roots)
	require.NotEmpty(t, response.Files)

	paths := make([]string, 0, len(response.Files))
	for index, file := range response.Files {
		require.Equal(t, realRoot, file.Root)
		require.Contains(t, []string{"file", "directory"}, file.MatchType)
		require.NotEmpty(t, file.FileName)
		paths = append(paths, file.Path)
		if index > 0 {
			previous := response.Files[index-1]
			require.True(t, previous.Score > file.Score || previous.Score == file.Score && previous.Path < file.Path)
		}
	}
	require.Contains(t, paths, "needle-dir")
	require.Contains(t, paths, "src/needle-file.txt")
	require.NotContains(t, paths, "ignored-needle.txt")

	limited, err := backend.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "needle",
		Roots: []string{"."},
		Limit: 1,
	})
	require.NoError(t, err)
	require.Len(t, limited.Files, 1)
	require.Equal(t, response.Files[0], limited.Files[0])
}

func TestSearchWorkspaceFilesEmptyQuery(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)

	response, err := backend.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "",
		Roots: []string{"."},
		Limit: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{realRoot}, response.Roots)
	require.Empty(t, response.Files)
}

func TestSearchWorkspaceFilesRejectsInvalidRequests(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)
	outside := t.TempDir()

	for _, test := range []struct {
		name    string
		request proto.WorkspaceFileSearchRequest
		message string
	}{
		{name: "zero limit", request: proto.WorkspaceFileSearchRequest{Query: "x", Roots: []string{"."}}, message: "limit must be between 1 and 1000"},
		{name: "large limit", request: proto.WorkspaceFileSearchRequest{Query: "x", Roots: []string{"."}, Limit: 1001}, message: "limit must be between 1 and 1000"},
		{name: "empty root", request: proto.WorkspaceFileSearchRequest{Query: "x", Roots: []string{""}, Limit: 50}, message: "fuzzy file search root must not be empty"},
		{name: "outside root", request: proto.WorkspaceFileSearchRequest{Query: "x", Roots: []string{outside}, Limit: 50}, message: "fuzzy file search root is outside the workspace"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := backend.SearchWorkspaceFiles(t.Context(), workspace.ID, test.request)
			require.ErrorIs(t, err, ErrInvalidFileSearch)
			require.ErrorContains(t, err, test.message)
		})
	}
}

func TestSearchWorkspaceFilesRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	outside := t.TempDir()
	link := filepath.Join(root, "outside")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)

	_, err = backend.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "x",
		Roots: []string{"outside"},
		Limit: 50,
	})
	require.ErrorIs(t, err, ErrInvalidFileSearch)
	require.ErrorContains(t, err, "fuzzy file search root is outside the workspace")
}

func TestSearchWorkspaceFilesCancellation(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "needle.txt"), []byte("needle"), 0o600))
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = backend.SearchWorkspaceFiles(ctx, workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "needle",
		Roots: []string{"."},
		Limit: 50,
	})
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, errors.Is(err, context.Canceled))
}

func TestWorkspaceFileSearchSessionLifecycle(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "needle.txt"), []byte("needle"), 0o600))
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)

	err = backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: "owner-a", SessionID: "empty"})
	require.ErrorIs(t, err, ErrInvalidFileSearch)
	require.ErrorContains(t, err, "at least one search root is required")

	_, err = backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: "owner-a", SessionID: "missing", Query: "needle", Limit: 50})
	require.ErrorIs(t, err, ErrInvalidFileSearch)
	require.ErrorContains(t, err, "fuzzy file search session not found: missing")

	for _, owner := range []string{"owner-a", "owner-b"} {
		require.NoError(t, backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: owner, SessionID: "shared", Roots: []string{"."}}))
	}
	first, err := backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: "owner-a", SessionID: "shared", Query: "old", Limit: 50})
	require.NoError(t, err)
	second, err := backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: "owner-a", SessionID: "shared", Query: "needle", Limit: 50})
	require.NoError(t, err)
	require.NotEqual(t, first.UpdateID, second.UpdateID)

	stale, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: "owner-a", SessionID: "shared", UpdateID: first.UpdateID})
	require.NoError(t, err)
	require.False(t, stale.Accepted)

	accepted, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: "owner-a", SessionID: "shared", UpdateID: second.UpdateID})
	require.NoError(t, err)
	require.True(t, accepted.Accepted)
	require.Equal(t, "needle", accepted.Query)
	require.Len(t, accepted.Files, 1)
	require.Equal(t, "needle.txt", accepted.Files[0].Path)

	duplicate, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: "owner-a", SessionID: "shared", UpdateID: second.UpdateID})
	require.NoError(t, err)
	require.False(t, duplicate.Accepted)

	pending, err := backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: "owner-b", SessionID: "shared", Query: "needle", Limit: 50})
	require.NoError(t, err)
	require.NoError(t, backend.StopWorkspaceFileSearchSession(workspace.ID, proto.WorkspaceFileSearchSessionRequest{Owner: "owner-a", SessionID: "unknown"}))
	require.NoError(t, backend.CloseWorkspaceFileSearchOwner("owner-a"))
	otherOwner, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: "owner-b", SessionID: "shared", UpdateID: pending.UpdateID})
	require.NoError(t, err)
	require.True(t, otherOwner.Accepted)

	require.NoError(t, backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: "owner-b", SessionID: "stopped", Roots: []string{"."}}))
	stopped, err := backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: "owner-b", SessionID: "stopped", Query: "needle", Limit: 50})
	require.NoError(t, err)
	require.NoError(t, backend.StopWorkspaceFileSearchSession(workspace.ID, proto.WorkspaceFileSearchSessionRequest{Owner: "owner-b", SessionID: "stopped"}))
	stoppedResult, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: "owner-b", SessionID: "stopped", UpdateID: stopped.UpdateID})
	require.NoError(t, err)
	require.False(t, stoppedResult.Accepted)
}

func TestWorkspaceFileSearchSessionRetainsCandidates(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "existing.txt"), []byte("existing"), 0o600))
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)

	require.NoError(t, backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{
		Owner: "owner", SessionID: "session", Roots: []string{"."},
	}))
	require.NoError(t, os.WriteFile(filepath.Join(root, "added-later.txt"), []byte("later"), 0o600))

	update, err := backend.BeginWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{
		Owner: "owner", SessionID: "session", Query: "addedlater", Limit: 50,
	})
	require.NoError(t, err)
	result, err := backend.CompleteWorkspaceFileSearchSessionUpdate(workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{
		Owner: "owner", SessionID: "session", UpdateID: update.UpdateID,
	})
	require.NoError(t, err)
	require.True(t, result.Accepted)
	require.Empty(t, result.Files)

	oneShot, err := backend.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "addedlater", Roots: []string{"."}, Limit: 50,
	})
	require.NoError(t, err)
	require.Len(t, oneShot.Files, 1)
	require.Equal(t, "added-later.txt", oneShot.Files[0].Path)
}

func TestWorkspaceFileSearchSessionCleanup(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	backend, _ := newTestBackend(t)
	workspace, _ := insertTestWorkspace(t, backend, realRoot)

	require.NoError(t, backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: "owner", SessionID: "session", Roots: []string{"."}}))
	backend.closeWorkspaceFileSearchSessions(workspace.ID)
	require.Empty(t, backend.fileSearch)

	require.NoError(t, backend.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: "owner", SessionID: "session", Roots: []string{"."}}))
	backend.Shutdown()
	require.Empty(t, backend.fileSearch)
}
