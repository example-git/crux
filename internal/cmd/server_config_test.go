package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestRemoteServerStartupDoesNotLoadClientConfiguration(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "XDG_CACHE_HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(key, filepath.Join(root, key))
		require.NoError(t, os.MkdirAll(os.Getenv(key), 0o700))
	}
	require.NoError(t, os.WriteFile(config.GlobalConfigData(), []byte(`{"providers":`), 0o600))

	remote, err := server.ParseHostURL("tcp://0.0.0.0:9443")
	require.NoError(t, err)
	store, err := loadServerConfig(remote, "", false)
	require.NoError(t, err)
	require.Nil(t, store)

	local, err := server.ParseHostURL("unix://" + filepath.Join(root, "crux.sock"))
	require.NoError(t, err)
	_, err = loadServerConfig(local, "", false)
	require.Error(t, err)
}
