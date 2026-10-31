package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompletionDataDirectory(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "global"))
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
	path, err := CompletionDataDirectory(t.Context(), root, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, ".crux"), path)
	require.NoDirExists(t, path)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"options":{"data_directory":"json-state"}}`), 0o600))
	path, err = CompletionDataDirectory(t.Context(), root, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "json-state"), path)
	require.NoError(t, os.WriteFile(filepath.Join(root, "cruxrc"), []byte("option data-directory shell-state\n"), 0o600))
	path, err = CompletionDataDirectory(t.Context(), root, "")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "shell-state"), path)
	path, err = CompletionDataDirectory(t.Context(), root, "explicit")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "explicit"), path)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{broken`), 0o600))
	_, err = CompletionDataDirectory(t.Context(), root, "")
	require.Error(t, err)
}
