package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestSavedConnectionEnablesClientServerAndRemoteHostRequiresAuthentication(t *testing.T) {
	originalConnection := connectionName
	originalHost := clientHost
	t.Cleanup(func() {
		connectionName = originalConnection
		clientHost = originalHost
	})

	connectionName = "remote"
	require.True(t, useClientServer())

	connectionName = ""
	clientHost = "tcp://192.0.2.10:9090"
	_, _, _, err := connectToServer(&cobra.Command{})
	require.EqualError(t, err, "remote TCP servers require a saved authenticated connection")
}

func TestRemoteConnectionCwdDoesNotRequireServerPathLocally(t *testing.T) {
	localCwd := t.TempDir()
	t.Chdir(localCwd)
	serverCwd := "/srv/crux/projects/server-only"
	command := &cobra.Command{}
	command.Flags().String("cwd", "", "")
	require.NoError(t, command.Flags().Set("cwd", serverCwd))

	local, workspace, err := resolveConnectionCwds(command, true)
	require.NoError(t, err)
	require.Equal(t, localCwd, local)
	require.Equal(t, serverCwd, workspace)
}

func TestConnectionsCommandsRegistered(t *testing.T) {
	for _, test := range []struct {
		args []string
		want *cobra.Command
	}{
		{args: []string{"connections"}, want: connectionsCmd},
		{args: []string{"connections", "server-init"}, want: connectionsServerInitCmd},
		{args: []string{"connections", "add"}, want: connectionsAddCmd},
		{args: []string{"connections", "authorize"}, want: connectionsAuthorizeCmd},
	} {
		command, _, err := rootCmd.Find(test.args)
		require.NoError(t, err)
		require.Same(t, test.want, command)
	}
	require.NotNil(t, rootCmd.PersistentFlags().Lookup("connection"))
}
