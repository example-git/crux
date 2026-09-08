package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestWorkspaceCreationFlightRejectsAnotherPrincipal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		dir := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(dir, 0o700))
		t.Setenv(name, dir)
	}
	b := New(context.Background(), nil, func() {})
	b.SetCreateGrace(time.Minute)
	t.Cleanup(func() { drainBackend(t, b) })
	started, release := make(chan struct{}), make(chan struct{})
	realInit := b.initConfig
	b.initConfig = func(path, data string, debug bool) (*config.ConfigStore, error) {
		close(started)
		<-release
		return realInit(path, data, debug)
	}
	first := proto.Workspace{Path: t.TempDir(), ClientID: uuid.NewString(), AuthorityMode: "server", AuthenticatedPrincipal: strings.Repeat("a", 64)}
	second := first
	second.ClientID = uuid.NewString()
	second.AuthenticatedPrincipal = strings.Repeat("b", 64)
	firstResult, secondResult := make(chan error, 1), make(chan error, 1)
	go func() { _, _, err := b.CreateWorkspace(first); firstResult <- err }()
	<-started
	go func() { _, _, err := b.CreateWorkspace(second); secondResult <- err }()
	require.Eventually(t, func() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.pending == 2 }, 5*time.Second, time.Millisecond)
	close(release)
	require.NoError(t, <-firstResult)
	require.ErrorIs(t, <-secondResult, ErrWorkspaceAuthority)
	workspaces := b.ListWorkspacesForPrincipal(first.AuthenticatedPrincipal)
	require.Len(t, workspaces, 1)
	require.Empty(t, b.ListWorkspacesForPrincipal(second.AuthenticatedPrincipal))
	ws, err := b.GetWorkspace(workspaces[0].ID)
	require.NoError(t, err)
	ws.clientsMu.Lock()
	_, firstClaim := ws.clients[first.ClientID]
	_, secondClaim := ws.clients[second.ClientID]
	ws.clientsMu.Unlock()
	require.True(t, firstClaim)
	require.False(t, secondClaim)
	_, _, err = b.CreateWorkspace(second)
	require.ErrorIs(t, err, ErrWorkspaceAuthority)
	second.AuthenticatedPrincipal = first.AuthenticatedPrincipal
	require.ErrorIs(t, b.BindClientPrincipal(second.ClientID, second.AuthenticatedPrincipal), ErrWorkspaceAuthority)
}
