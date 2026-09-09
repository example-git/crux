package cmd

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/connection"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestConnectionsRevokeReportsStoredAuthorizationOnly(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	server, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, certificate, err := connection.Add(t.Context(), "fixture", "tcp://127.0.0.1:9443", server)
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "fixture", certificate))
	beforeForce := connectionsRevokeForce
	connectionsRevokeForce = true
	t.Cleanup(func() { connectionsRevokeForce = beforeForce })
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetContext(t.Context())
	command.SetOut(&output)
	require.NoError(t, connectionsRevokeCmd.RunE(command, []string{"fixture"}))
	clients, err := connection.ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Empty(t, clients)
	require.Contains(t, output.String(), "Revoked stored authorization for client fixture")
	require.Contains(t, output.String(), "No live daemon was registered; no live cancellation acknowledgement was received.")
	require.NotContains(t, output.String(), "acknowledged cancellation and joined work")
	require.NotContains(t, output.String(), "Restart")
}
