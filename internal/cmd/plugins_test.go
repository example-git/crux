package cmd

import (
	"bytes"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestPluginsCommandRegistered(t *testing.T) {
	command, _, err := rootCmd.Find([]string{"plugins", "install"})
	require.NoError(t, err)
	require.Same(t, pluginsInstallCmd, command)
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("ref"))
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("update"))
	require.NotNil(t, pluginsInstallCmd.Flags().Lookup("no-trust"))
	require.NotNil(t, pluginsTrustCmd.Flags().Lookup("digest"))
	require.NotNil(t, pluginsTrustCmd.Flags().Lookup("revoke"))
}

func TestPrintEmptyPluginSnapshot(t *testing.T) {
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	previous := pluginOutputJSON
	pluginOutputJSON = false
	t.Cleanup(func() { pluginOutputJSON = previous })
	require.NoError(t, printPluginSnapshot(command, providerplugin.Snapshot{}))
	require.Contains(t, output.String(), "Core-only mode is available")
}
