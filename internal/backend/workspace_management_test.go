package backend

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCloseIdleWorkspaceAndRejectConnectedWorkspace(t *testing.T) {
	backend, shutdowns := newTestBackend(t)
	backend.SetPersistent(true)
	workspace, workspaceShutdowns := insertTestWorkspace(t, backend, t.TempDir())
	clientID := newClientID(t)
	workspace.clients[clientID] = &clientState{streams: 1}

	require.ErrorIs(t, backend.CloseIdleWorkspace(workspace.ID), ErrWorkspaceInUse)
	require.Equal(t, 1, workspace.ConnectedClients())
	workspace.clients[clientID].streams = 0
	require.ErrorIs(t, backend.CloseIdleWorkspace(workspace.ID), ErrWorkspaceInUse)
	delete(workspace.clients, clientID)
	require.NoError(t, backend.CloseIdleWorkspace(workspace.ID))
	require.Equal(t, int32(1), workspaceShutdowns.Load())
	require.Equal(t, int32(0), shutdowns.Load())
	_, err := backend.GetWorkspace(workspace.ID)
	require.ErrorIs(t, err, ErrWorkspaceNotFound)
}

func TestCreateWorkspaceRevalidatesConfiguredRoots(t *testing.T) {
	backend, _ := newTestBackend(t)
	root := t.TempDir()
	args := protoWS(t.TempDir(), t.TempDir(), newClientID(t))
	args.AllowedWorkspaceRoots = []string{root}

	_, _, err := backend.CreateWorkspace(args)
	require.ErrorContains(t, err, "workspace path escaped configured roots")
	require.Empty(t, backend.ListWorkspaces())
}

func TestPersistentBackendDoesNotShutdownWhenIdle(t *testing.T) {
	backend, shutdowns := newTestBackend(t)
	backend.SetIdleShutdownDelay(time.Millisecond)
	backend.SetPersistent(true)
	workspace, _ := insertTestWorkspace(t, backend, t.TempDir())
	require.NoError(t, backend.CloseIdleWorkspace(workspace.ID))
	time.Sleep(5 * time.Millisecond)
	require.Equal(t, int32(0), shutdowns.Load())
}
