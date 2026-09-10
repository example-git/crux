package providerplugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/lock"
	"github.com/stretchr/testify/require"
)

func TestInstallAfterCommitUsesCommittedBytesUnderLock(t *testing.T) {
	manager := newTestManager(t)
	source := filepath.Join(t.TempDir(), "source.plugin")
	value := readExampleManifest(t)
	writeBundleManifest(t, source, value)
	var committed InstalledBundle
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true, AfterCommit: func(installed InstalledBundle) error {
		committed = installed
		unlock, err := lock.TryFile(manager.paths.ManagerLock)
		if unlock != nil {
			unlock()
		}
		require.Error(t, err, "another updater cannot replace the bundle while references are written")
		data, err := os.ReadFile(filepath.Join(manager.paths.Bundles, installed.ID+bundleSuffix, manifestFilename))
		require.NoError(t, err, "callback must run after commit")
		var stored map[string]any
		require.NoError(t, json.Unmarshal(data, &stored))
		require.Equal(t, installed.Version, stored["version"])
		// A source edit after commit cannot change the identity delivered to
		// the config writer, the registered bundle, or exact-digest trust.
		value.Version = "9.0.0"
		writeBundleManifest(t, source, value)
		return nil
	}})
	require.NoError(t, err)
	require.Equal(t, "1.0.0", committed.Version)
	require.Equal(t, committed.Digest, snapshot.Plugins[0].Digest)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	require.Equal(t, committed.Version, snapshot.Plugins[0].Version)
}
