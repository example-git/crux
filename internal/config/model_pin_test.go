package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// twoProviderConfig is a config file naming large as the given
// provider/model, with both providers available so either choice resolves.
func twoProviderConfig(provider, model string) string {
	return `{
		"models": {
			"large": {"provider": "` + provider + `", "model": "` + model + `"}
		},
		"providers": {
			"alpha": {
				"type": "openai-compat",
				"base_url": "https://alpha.example.test/v1",
				"api_key": "test-key",
				"models": [{"id": "alpha-model", "name": "Alpha"}]
			},
			"beta": {
				"type": "openai-compat",
				"base_url": "https://beta.example.test/v1",
				"api_key": "test-key-2",
				"models": [{"id": "beta-model", "name": "Beta"}]
			}
		}
	}`
}

// TestModelSelectionSurvivesPeerWrite is a regression test for the model
// switching out from under the user when several Crux instances share the
// global config file. A sibling instance selecting a different model must
// not change ours, even though an unrelated config write (a token refresh,
// for example) reloads the file we both write to.
func TestModelSelectionSurvivesPeerWrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("alpha", "alpha-model")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// The user picks Beta in this instance.
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, SelectedModel{
		Provider: "beta",
		Model:    "beta-model",
	}))

	// A sibling instance then picks Alpha and writes it to the shared file.
	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("alpha", "alpha-model")), 0o600))

	// Any reload (here explicit; in practice triggered by an unrelated
	// write such as an OAuth token refresh) must leave our choice alone.
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	large := store.Config().Models[SelectedModelTypeLarge]
	require.Equal(t, "beta", large.Provider)
	require.Equal(t, "beta-model", large.Model)
}

// TestModelSelectionYieldsToDiskWhenUnchosen verifies the other half of the
// rule: a model type this instance never selected still follows the config
// file, so external edits and `crux login` defaults keep working.
func TestModelSelectionYieldsToDiskWhenUnchosen(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("alpha", "alpha-model")), 0o600))

	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	require.NoError(t, os.WriteFile(configPath, []byte(twoProviderConfig("beta", "beta-model")), 0o600))
	require.NoError(t, store.ReloadFromDisk(context.Background()))

	large := store.Config().Models[SelectedModelTypeLarge]
	require.Equal(t, "beta", large.Provider)
	require.Equal(t, "beta-model", large.Model)
}
