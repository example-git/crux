package config

import (
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/stretchr/testify/require"
)

type remoteBaselineFile struct {
	Mode fs.FileMode
	Hash [sha256.Size]byte
}

// Include directory/file existence and modes as well as content. No source
// bytes, including synthetic credentials, are printed by failed comparisons.
func remoteBaselineTree(t *testing.T, root string) map[string]remoteBaselineFile {
	t.Helper()
	result := map[string]remoteBaselineFile{}
	require.NoError(t, filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[relative] = remoteBaselineFile{Mode: info.Mode(), Hash: sha256.Sum256(data)}
		return nil
	}))
	return result
}

func TestLegacyForwardingServerStateBaseline(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"CRUX_GLOBAL_DATA", "CRUX_GLOBAL_CONFIG", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		t.Setenv(name, filepath.Join(root, name))
	}
	for _, name := range []string{"accounts.json", "plugin-state/trust.json", "connections.json", "plugins/example.plugin/manifest.json"} {
		path := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(`{"server_fixture":"unchanged"}`), 0o600))
	}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"copilot":     {ID: "copilot", APIKey: "synthetic-server-key"},
		"server-only": {ID: "server-only", APIKey: "synthetic-unselected-key"},
	})}
	cfg.setDefaults(root, filepath.Join(root, "workspace"))
	store := NewTestStore(cfg)
	store.workingDir = root
	store.globalDataPath = filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(store.globalDataPath, mustMarshalConfig(cfg), 0o600))
	before := remoteBaselineTree(t, root)
	// Explicitly empty client credentials currently preserve server bytes via
	// omitempty + merge. Detached client authority must replace this behavior.
	require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{"copilot": {ID: "copilot", APIKey: ""}}, nil))
	provider, _ := store.Config().Providers.Get("copilot")
	require.Equal(t, "synthetic-server-key", provider.APIKey)
	_, found := store.Config().Providers.Get("server-only")
	require.True(t, found)
	require.Equal(t, before, remoteBaselineTree(t, root))
	require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{"copilot": {ID: "copilot", APIKey: "synthetic-client-key"}}, nil))
	require.Equal(t, before, remoteBaselineTree(t, root), "initial forwarding itself is memory-only")
	registration, ok := store.ProviderRegistration("copilot")
	require.True(t, ok)
	require.NoError(t, store.SetProviderAPIKey(ScopeGlobal, "copilot", ProviderAPIKeyCredential{Owner: registration.Owner(), APIKey: "synthetic-replaced-client-key"}))
	after := remoteBaselineTree(t, root)
	require.NotEqual(t, before["crux.json"], after["crux.json"], "baseline: normal setter persists forwarded credentials")
	for name, state := range before {
		if name != "crux.json" {
			require.Equal(t, state, after[name], "unrelated server state: %s", name)
		}
	}
}

// This characterizes the legacy overlay, not the desired detached runtime.
// Exact matches and unselected overlaps must cease blocking client-owned
// snapshots, while duplicate owners inside a received snapshot remain invalid.
func TestLegacyForwardingOwnerOverlapBaseline(t *testing.T) {
	for _, kind := range []string{"plugin", "preset"} {
		for _, scenario := range []string{"identical", "replacement", "unselected"} {
			t.Run(kind+"/"+scenario, func(t *testing.T) {
				persisted := ProviderConfig{ID: "remote-example", APIKey: "server-secret"}
				if kind == "plugin" {
					persisted.Plugin = &ProviderPluginReference{ID: "example.provider", Version: "1.0.0"}
				} else {
					persisted.Preset = &ProviderPresetReference{ID: "example.preset", Version: "1.0.0"}
				}
				forwarded := persisted
				forwarded.APIKey = "client-secret"
				if scenario == "replacement" {
					if kind == "plugin" {
						forwarded.Plugin = &ProviderPluginReference{ID: "example.replacement", Version: "2.0.0"}
					} else {
						forwarded.Preset = &ProviderPresetReference{ID: "example.replacement", Version: "2.0.0"}
					}
				}
				cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{persisted.ID: persisted})}
				selectedProvider := persisted.ID
				if scenario == "unselected" {
					selectedProvider = "unrelated-provider"
				}
				cfg.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: selectedProvider, Model: "example-model"}}
				store := NewTestStore(cfg)
				before := store.Config()
				err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{persisted.ID: forwarded}, nil)
				require.ErrorContains(t, err, "conflicts with its persisted provider owner")
				require.Same(t, before, store.Config())
				require.Empty(t, store.ephemeralProviderSnapshot())
				actual, _ := store.Config().Providers.Get(persisted.ID)
				require.Equal(t, persisted, actual)
				require.NotContains(t, err.Error(), "client-secret")
				require.NotContains(t, err.Error(), "server-secret")
			})
		}
	}
}
