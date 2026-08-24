package providerplugin

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDefaultPathsFollowCanonicalOverrides(t *testing.T) {
	data := t.TempDir()
	cache := t.TempDir()
	paths := DefaultPaths(data, cache)
	require.Equal(t, filepath.Join(data, "plugins"), paths.Bundles)
	require.Equal(t, filepath.Join(cache, "plugins"), paths.Cache)
	require.Equal(t, filepath.Join(data, pluginStateDir), paths.State)
	require.Equal(t, filepath.Join(paths.State, trustFilename), paths.TrustFile)
	require.Equal(t, filepath.Join(paths.State, provenanceFilename), paths.ProvenanceFile)
	require.Equal(t, filepath.Join(paths.State, managerLockName), paths.ManagerLock)
}
