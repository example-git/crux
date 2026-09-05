package cmd

import (
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func menuTestCommand() *cobra.Command {
	command := &cobra.Command{Use: "test"}
	command.Flags().String("cwd", "", "")
	command.Flags().String("data-dir", "", "")
	command.Flags().String("session", "", "")
	command.Flags().Bool("continue", false, "")
	command.Flags().Bool("yolo", false, "")
	command.Flags().StringSlice("channels", nil, "")
	command.Flags().String("initial-prompt", "", "")
	return command
}

func TestCompatibilityStartupFlagsRemainHidden(t *testing.T) {
	initialPrompt := rootCmd.Flags().Lookup("initial-prompt")
	require.NotNil(t, initialPrompt)
	require.True(t, initialPrompt.Hidden)
	permissionMode := runCmd.Flags().Lookup("compatibility-permission-mode")
	require.NotNil(t, permissionMode)
	require.True(t, permissionMode.Hidden)
	require.Equal(t, string(proto.AgentPermissionBypass), permissionMode.DefValue)
}

func TestParameterlessSavedConnectionOpensServerMenu(t *testing.T) {
	previous := connectionName
	connectionName = "remote"
	t.Cleanup(func() { connectionName = previous })
	require.True(t, shouldOpenServerMenu(menuTestCommand(), nil))
}

func TestWorkspaceFlagsBypassServerMenu(t *testing.T) {
	previous := connectionName
	connectionName = "remote"
	t.Cleanup(func() { connectionName = previous })
	for _, flag := range []string{"cwd", "data-dir", "session", "continue", "yolo", "channels", "initial-prompt"} {
		command := menuTestCommand()
		if command.Flags().Lookup(flag).Value.Type() == "bool" {
			require.NoError(t, command.Flags().Set(flag, "true"))
		} else if flag == "channels" {
			require.NoError(t, command.Flags().Set(flag, "server:test"))
		} else {
			require.NoError(t, command.Flags().Set(flag, "value"))
		}
		require.False(t, shouldOpenServerMenu(command, nil), flag)
	}
}
