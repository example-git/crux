package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestInstalledProviderUpdatePreservesOtherOwners(t *testing.T) {
	for _, raw := range []string{
		`{"providers":{"target":{"plugin":{"id":"different","version":"1.0.0"}}}}`,
		`{"providers":{"different":{"plugin":{"id":"plugin.target","version":"1.0.0"}}}}`,
		`{"providers":{"target":{"api_key":"literal"}}}`,
		`{"providers":{"target":{"plugin":{"id":"plugin.target","version":"2.0.0"}}}}`,
	} {
		t.Run(raw, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crux.json")
			require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
			before, err := os.Stat(path)
			require.NoError(t, err)
			require.NoError(t, updateProviderReferenceFile(t.Context(), path, providerplugin.InstalledBundle{ID: "plugin.target", ProviderID: "target", Version: "2.0.0", Digest: "new", PluginType: manifest.PluginTypeProvider}))
			after, err := os.Stat(path)
			require.NoError(t, err)
			require.True(t, os.SameFile(before, after), "unchanged references should not rewrite the file")
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, raw, string(data))
		})
	}
}

func TestInstalledPresetUpdateRefreshesDigestWithoutTakingPluginOwnership(t *testing.T) {
	for _, pluginOwned := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "crux.json")
		raw := `{"providers":{"target":{"preset":{"id":"preset.target","version":"1.0.0","digest":"old"}}}}`
		if pluginOwned {
			raw = `{"providers":{"target":{"plugin":{"id":"full-plugin","version":"1.0.0"},"preset":{"id":"preset.target","version":"1.0.0","digest":"old"}}}}`
		}
		require.NoError(t, os.WriteFile(path, []byte(raw), 0o600))
		require.NoError(t, updateProviderReferenceFile(t.Context(), path, providerplugin.InstalledBundle{ID: "preset.target", ProviderID: "target", Version: "1.0.0", Digest: "new", PluginType: manifest.PluginTypeProviderPreset}))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		if pluginOwned {
			require.Equal(t, raw, string(data))
		} else {
			require.Equal(t, "new", gjson.GetBytes(data, "providers.target.preset.digest").String(), "same-version updates still refresh the digest")
		}
	}
}

func TestInstalledProviderUpdateHonorsDataDirectoryWithoutEvaluatingShell(t *testing.T) {
	for _, flagOverride := range []bool{false, true} {
		root := t.TempDir()
		t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
		t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
		// JSON selects a non-default workspace data directory. --data-dir,
		// when supplied, selects the actual workspace persistence scope.
		require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"options":{"data_directory":"json-data"}}`), 0o600))
		dir, flag := "json-data", ""
		if flagOverride {
			dir, flag = "flag-data", "flag-data"
		}
		path := filepath.Join(root, dir, "crux.json")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(`{"providers":{"target":{"plugin":{"id":"plugin.target","version":"1.0.0"}}}}`), 0o600))
		marker := filepath.Join(root, "must-not-execute")
		require.NoError(t, os.WriteFile(filepath.Join(root, "cruxrc"), []byte("touch '"+marker+"'\n"), 0o600))
		require.NoError(t, UpdateInstalledProviderReferences(t.Context(), root, flag, providerplugin.InstalledBundle{ID: "plugin.target", ProviderID: "target", Version: "2.0.0", Digest: "new", PluginType: manifest.PluginTypeProvider}))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, "2.0.0", gjson.GetBytes(data, "providers.target.plugin.version").String())
		require.NoFileExists(t, marker)
	}
}
