package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestImageSetupRejectsRemoteTargetBeforeSourceAccess(t *testing.T) {
	for _, flag := range []string{"host", "connection"} {
		command := newImageSetupCommand()
		command.Flags().String(flag, "", "test target")
		require.NoError(t, command.Flags().Set(flag, "remote"))
		err := command.RunE(command, []string{"/nonexistent/source"})
		require.ErrorContains(t, err, "execution host")
	}
}
