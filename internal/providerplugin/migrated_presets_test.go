package providerplugin

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCanonicalMigratedProviderPresetRequiresExactOwner(t *testing.T) {
	t.Parallel()

	id, version, digest, migrated := CanonicalMigratedProviderPreset("deepseek")
	require.True(t, migrated)
	require.Len(t, digest, 64)
	require.True(t, IsCanonicalMigratedProviderPreset("deepseek", id, version))
	require.True(t, IsCanonicalMigratedProviderPresetBundle("deepseek", id, version, digest))
	require.False(t, IsCanonicalMigratedProviderPreset("deepseek", "example.deepseek", version))
	require.False(t, IsCanonicalMigratedProviderPreset("deepseek", id, "0.51.24"))
	require.False(t, IsCanonicalMigratedProviderPreset("example", id, version))
	require.False(t, IsCanonicalMigratedProviderPresetBundle("deepseek", id, version, strings.Repeat("0", 64)))
}

func TestCanonicalMigratedProviderPresetDigestsMatchGeneratedBundles(t *testing.T) {
	t.Parallel()

	root := generatedPresetRoot(t)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	count := 0
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), bundleSuffix) {
			continue
		}
		providerID := strings.TrimSuffix(entry.Name(), bundleSuffix)
		presetID, version, digest, migrated := CanonicalMigratedProviderPreset(providerID)
		require.True(t, migrated, providerID)
		require.Equal(t, "crux.catwalk."+providerID, presetID)
		require.Equal(t, "0.51.23", version)
		snapshot, err := snapshotDirectory(filepath.Join(root, entry.Name()), filepath.Join(t.TempDir(), "snapshot"))
		require.NoError(t, err, providerID)
		require.Equal(t, 1, snapshot.FileCount, providerID)
		require.Equal(t, digest, snapshot.Digest, providerID)
		count++
	}
	require.Equal(t, 26, count)
}

func generatedPresetRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(file), "..", "..", "plugins", "provider-presets")
}
