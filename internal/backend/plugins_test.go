package backend

import (
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/stretchr/testify/require"
)

func TestPluginSnapshotProtoPreservesBundleType(t *testing.T) {
	t.Parallel()

	scannedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	snapshot := providerplugin.Snapshot{
		Revision:  9,
		ScannedAt: scannedAt,
		Plugins: []providerplugin.Status{{
			BundleName:   "example.preset.plugin",
			PluginType:   "provider-preset",
			ID:           "example.preset",
			Capabilities: []string{"preset"},
		}},
	}

	got := pluginSnapshotProto(snapshot, "plugin-native", []string{"example"})
	require.Equal(t, "plugin-native", got.Profile)
	require.Equal(t, []string{"example"}, got.EnabledProviders)
	require.Equal(t, uint64(9), got.Revision)
	require.Equal(t, scannedAt, got.ScannedAt)
	require.Len(t, got.Plugins, 1)
	require.Equal(t, "provider-preset", got.Plugins[0].PluginType)

	snapshot.Plugins[0].Capabilities[0] = "changed"
	require.Equal(t, []string{"preset"}, got.Plugins[0].Capabilities)
}
