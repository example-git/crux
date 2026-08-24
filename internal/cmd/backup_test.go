package cmd

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBackupCommandsRegistered(t *testing.T) {
	exportCommand, _, err := rootCmd.Find([]string{"export"})
	require.NoError(t, err)
	require.Same(t, exportCmd, exportCommand)

	importCommand, _, err := rootCmd.Find([]string{"import"})
	require.NoError(t, err)
	require.Same(t, importCmd, importCommand)
}

func TestPromptBackupPassword(t *testing.T) {
	original := readBackupPassword
	t.Cleanup(func() {
		readBackupPassword = original
	})
	readBackupPassword = func(int) ([]byte, error) {
		return []byte("secret"), nil
	}
	var output bytes.Buffer
	command := exportCmd
	command.SetErr(&output)
	t.Cleanup(func() {
		command.SetErr(nil)
	})

	password, err := promptBackupPassword(command, "Password: ")
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), password)
	require.Equal(t, "Password: \n", output.String())
}
