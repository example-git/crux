//go:build !windows

package install

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func requireMode(t *testing.T, path string, expected os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, expected, info.Mode().Perm())
}

func createInsecureDirectory(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Mkdir(path, 0o755))
	require.NoError(t, os.Chmod(path, 0o755))
}
