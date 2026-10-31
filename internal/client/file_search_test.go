package client

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestSearchWorkspaceFilesThroughRegisteredServerRoute(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "needle.txt"), []byte("needle"), 0o600))

	srv := server.NewServer(nil, "tcp", "127.0.0.1:0")
	workspace := &backend.Workspace{ID: "workspace-file-search", Path: realRoot}
	backend.SetWorkspaceShutdownFnForTest(workspace, func() {})
	backend.InsertWorkspaceForTest(srv.Backend(), workspace)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	client := captureClient(t, hs)

	response, err := client.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "needle",
		Roots: []string{"."},
		Limit: 50,
	})
	require.NoError(t, err)
	require.Equal(t, []string{realRoot}, response.Roots)
	require.Len(t, response.Files, 1)
	require.Equal(t, "needle.txt", response.Files[0].Path)
	require.Equal(t, "file", response.Files[0].MatchType)

	_, err = client.SearchWorkspaceFiles(t.Context(), workspace.ID, proto.WorkspaceFileSearchRequest{
		Query: "needle",
		Roots: []string{t.TempDir()},
		Limit: 50,
	})
	require.ErrorContains(t, err, "status code 400")
	require.ErrorContains(t, err, "fuzzy file search root is outside the workspace")

	owner := "route-owner"
	err = client.StartWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionStartRequest{Owner: owner, SessionID: "session", Roots: []string{"."}})
	require.NoError(t, err)
	begin, err := client.BeginWorkspaceFileSearchSessionUpdate(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: owner, SessionID: "session", Query: "needle", Limit: 50})
	require.NoError(t, err)
	require.NotEmpty(t, begin.UpdateID)
	completed, err := client.CompleteWorkspaceFileSearchSessionUpdate(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: owner, SessionID: "session", UpdateID: begin.UpdateID})
	require.NoError(t, err)
	require.True(t, completed.Accepted)
	require.Equal(t, "needle", completed.Query)
	require.Len(t, completed.Files, 1)

	_, err = client.BeginWorkspaceFileSearchSessionUpdate(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: owner, SessionID: "missing", Query: "needle", Limit: 50})
	require.ErrorContains(t, err, "status code 400")
	require.ErrorContains(t, err, "fuzzy file search session not found: missing")

	begin, err = client.BeginWorkspaceFileSearchSessionUpdate(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionBeginRequest{Owner: owner, SessionID: "session", Query: "needle", Limit: 50})
	require.NoError(t, err)
	require.NoError(t, client.StopWorkspaceFileSearchSession(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionRequest{Owner: owner, SessionID: "session"}))
	stale, err := client.CompleteWorkspaceFileSearchSessionUpdate(t.Context(), workspace.ID, proto.WorkspaceFileSearchSessionCompleteRequest{Owner: owner, SessionID: "session", UpdateID: begin.UpdateID})
	require.NoError(t, err)
	require.False(t, stale.Accepted)
	require.NoError(t, client.CloseWorkspaceFileSearchOwner(t.Context(), proto.WorkspaceFileSearchOwnerRequest{Owner: owner}))
}
