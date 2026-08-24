package cmd

import (
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestServerDaemonInstallCommandRegistered(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"server", "daemon", "install"})
	require.NoError(t, err)
	require.Same(t, daemonInstallCmd, command)
	require.NotNil(t, command.Flags().Lookup("host"))
	require.NotNil(t, command.Flags().Lookup("manager"))
	require.NotNil(t, command.Flags().Lookup("workspace-root"))
}

func TestServerDaemonInstallRejectsNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-Linux contract")
	}
	err := daemonInstallCmd.RunE(daemonInstallCmd, nil)
	require.EqualError(t, err, "server daemon installation is only supported on Linux")
}
