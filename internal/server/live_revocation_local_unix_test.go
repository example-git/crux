//go:build unix

package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestLiveAuthorizationPreservesLocalUnixSocket(t *testing.T) {
	// Keep the socket shorter than Darwin's sockaddr_un path limit.
	root, err := os.MkdirTemp("", "crx-auth-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	storeRoot := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", storeRoot)
	path := filepath.Join(storeRoot, "connections.json")
	malformed := []byte(`{"invalid-network-authorization"`)
	require.NoError(t, os.WriteFile(path, malformed, 0o600))
	socket := filepath.Join(root, "server.sock")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	srv := NewServer(nil, "unix", socket)
	finished := make(chan error, 1)
	go func() { finished <- srv.Serve(listener) }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = listener.Close()
		<-finished
		srv.backend.Shutdown()
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	for _, route := range []string{"/v1/health", "/v1/workspaces"} {
		response, err := client.Get("http://local" + route)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, response.StatusCode)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
	}
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, malformed, actual)
	_, err = os.Stat(path + ".lock")
	require.ErrorIs(t, err, os.ErrNotExist, "local socket requests must not inspect or lock network authorization")
}
