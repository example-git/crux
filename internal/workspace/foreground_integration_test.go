package workspace_test

import (
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestForegroundControlRemoteSessionIsolation(t *testing.T) {
	xdgIsolate(t)
	runtime := newRuntimeServer(t)
	cwd := t.TempDir()
	client := runtime.newClient(t, cwd)
	created, err := client.CreateWorkspace(t.Context(), proto.Workspace{Path: cwd, DataDir: t.TempDir(), AuthorityMode: "server"})
	require.NoError(t, err)
	live, err := runtime.srv.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	first := live.BackgroundShells.ForegroundWaits.Register("first")
	second := live.BackgroundShells.ForegroundWaits.Register("second")
	defer live.BackgroundShells.ForegroundWaits.Remove(first)
	defer live.BackgroundShells.ForegroundWaits.Remove(second)
	remote := workspace.NewClientWorkspace(client, *created)
	count, err := remote.ForegroundTaskControl(t.Context(), "first", false)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	count, err = remote.ForegroundTaskControl(t.Context(), "first", true)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	select {
	case <-first.Detached:
	default:
		t.Fatal("remote detach did not reach first wait")
	}
	select {
	case <-second.Detached:
		t.Fatal("remote detach crossed sessions")
	default:
	}
	count, err = remote.ForegroundTaskControl(t.Context(), "first", false)
	require.NoError(t, err)
	require.Zero(t, count)
	_, err = remote.ForegroundTaskControl(t.Context(), "", true)
	require.Error(t, err)
}
