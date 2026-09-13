package backend

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
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

func TestCreateWorkspaceChecksDerivedRemoteDataDirectory(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		for _, outside := range []bool{false, true} {
			name := "default"
			if explicit {
				name = "explicit"
			}
			if outside {
				name += "/outside"
			} else {
				name += "/inside"
			}
			t.Run(name, func(t *testing.T) {
				backend, _ := newTestBackend(t)
				root, err := filepath.EvalSymlinks(t.TempDir())
				require.NoError(t, err)
				workspacePath := filepath.Join(root, "workspace")
				require.NoError(t, os.MkdirAll(workspacePath, 0o700))
				requested := ""
				if explicit {
					requested = filepath.Join(root, "data")
					require.NoError(t, os.MkdirAll(requested, 0o700))
				}
				args := protoWS(workspacePath, requested, newClientID(t))
				args.AuthorityMode = "server"
				args.AuthenticatedPrincipal = strings.Repeat("a", 64)
				args.AllowedWorkspaceRoots = []string{root}
				target := filepath.Join(root, "target")
				if outside {
					target = filepath.Join(t.TempDir(), "target")
				}
				require.NoError(t, os.MkdirAll(target, 0o700))
				derived := remoteWorkspaceDataDir(workspacePath, requested, args.AuthenticatedPrincipal)
				require.NoError(t, os.MkdirAll(filepath.Dir(derived), 0o700))
				require.NoError(t, os.Symlink(target, derived))
				initialized := false
				stop := errors.New("stop before config initialization")
				backend.initConfig = func(string, string, bool) (*config.ConfigStore, error) {
					initialized = true
					return nil, stop
				}
				_, _, err = backend.CreateWorkspace(args)
				if outside {
					require.ErrorContains(t, err, "data directory escaped configured roots")
					require.False(t, initialized)
				} else {
					require.ErrorIs(t, err, stop)
					require.True(t, initialized)
				}
				require.Empty(t, backend.ListWorkspaces())
				entries, err := os.ReadDir(target)
				require.NoError(t, err)
				require.Empty(t, entries)
			})
		}
	}
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
