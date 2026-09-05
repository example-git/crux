package config

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/stretchr/testify/require"
)

// readConfigJSON reads and unmarshals the JSON config file at path.
func readConfigJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	baseDir := filepath.Dir(path)
	fileName := filepath.Base(path)
	b, err := fs.ReadFile(os.DirFS(baseDir), fileName)
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(b, &out))
	return out
}

// readRecentModels reads the recent_models section from the config file.
func readRecentModels(t *testing.T, path string) map[string]any {
	t.Helper()
	out := readConfigJSON(t, path)
	rm, ok := out["recent_models"].(map[string]any)
	require.True(t, ok)
	return rm
}

// testStoreWithPath creates a ConfigStore backed by a Config for recent model tests.
func testStoreWithPath(cfg *Config, dir string) *ConfigStore {
	return &ConfigStore{
		config:         cfg,
		globalDataPath: filepath.Join(dir, "config.json"),
	}
}

// configWithRecents builds a Config seeded with the given recent models for
// the large type, for exercising the pure nextRecentModels helper.
func configWithRecents(recents ...SelectedModel) *Config {
	return &Config{
		RecentModels: map[SelectedModelType][]SelectedModel{
			SelectedModelTypeLarge: recents,
		},
	}
}

func TestNextRecentModels_AddsToFront(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents()
	updated, changed := nextRecentModels(cfg, SelectedModelTypeLarge, SelectedModel{Provider: "openai", Model: "gpt-4o"})
	require.True(t, changed)
	require.Equal(t, []SelectedModel{{Provider: "openai", Model: "gpt-4o"}}, updated)
}

func TestNextRecentModels_DedupeAndMoveToFront(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents(
		SelectedModel{Provider: "anthropic", Model: "claude"},
		SelectedModel{Provider: "openai", Model: "gpt-4o"},
	)
	updated, changed := nextRecentModels(cfg, SelectedModelTypeLarge, SelectedModel{Provider: "openai", Model: "gpt-4o"})
	require.True(t, changed)
	require.Equal(t, []SelectedModel{
		{Provider: "openai", Model: "gpt-4o"},
		{Provider: "anthropic", Model: "claude"},
	}, updated)
}

func TestNextRecentModels_TrimsToMax(t *testing.T) {
	t.Parallel()

	var seed []SelectedModel
	for _, id := range []string{"m5", "m4", "m3", "m2", "m1"} {
		seed = append(seed, SelectedModel{Provider: "p", Model: id})
	}
	cfg := configWithRecents(seed...)

	updated, changed := nextRecentModels(cfg, SelectedModelTypeLarge, SelectedModel{Provider: "p", Model: "m6"})
	require.True(t, changed)
	require.Len(t, updated, maxRecentModelsPerType)
	require.Equal(t, SelectedModel{Provider: "p", Model: "m6"}, updated[0])
	require.Equal(t, SelectedModel{Provider: "p", Model: "m2"}, updated[maxRecentModelsPerType-1])
}

func TestNextRecentModels_SkipsEmptyValues(t *testing.T) {
	t.Parallel()

	cfg := configWithRecents()
	_, changed := nextRecentModels(cfg, SelectedModelTypeLarge, SelectedModel{Provider: "", Model: "m"})
	require.False(t, changed)
	_, changed = nextRecentModels(cfg, SelectedModelTypeLarge, SelectedModel{Provider: "p", Model: ""})
	require.False(t, changed)
}

func TestNextRecentModels_NoChangeWhenAlreadyFront(t *testing.T) {
	t.Parallel()

	entry := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	cfg := configWithRecents(entry)
	_, changed := nextRecentModels(cfg, SelectedModelTypeLarge, entry)
	require.False(t, changed)
}

func TestUpdatePreferredModel_PersistsModelAndRecents(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	cfg.Providers.Set("openai", ProviderConfig{ID: "openai", Models: []catalog.Model{{ID: "gpt-4o"}}})
	store := testStoreWithPath(cfg, dir)

	sel := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, sel))

	// in-memory state (read through the store; copy-on-write publishes a
	// new Config, so the seed cfg pointer is intentionally unchanged).
	require.Equal(t, sel, store.Config().Models[SelectedModelTypeLarge])
	require.Len(t, store.Config().RecentModels[SelectedModelTypeLarge], 1)

	// persisted state
	rm := readRecentModels(t, store.globalDataPath)
	large, ok := rm[string(SelectedModelTypeLarge)].([]any)
	require.True(t, ok)
	require.Len(t, large, 1)
	item := large[0].(map[string]any)
	require.Equal(t, "openai", item["provider"])
	require.Equal(t, "gpt-4o", item["model"])
}

func TestUpdatePreferredModel_ImplicitSmallTracksLargeProvider(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	alphaLarge := SelectedModel{Provider: "alpha", Model: "alpha-large"}
	alphaSmall := SelectedModel{Provider: "alpha", Model: "alpha-small"}
	betaLarge := SelectedModel{Provider: "beta", Model: "beta-large"}
	betaSmall := SelectedModel{Provider: "beta", Model: "beta-small", MaxTokens: 512}
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	cfg.Providers.Set("alpha", ProviderConfig{ID: "alpha", Models: []catalog.Model{{ID: "alpha-large"}, {ID: "alpha-small"}}})
	cfg.Providers.Set("beta", ProviderConfig{ID: "beta", Models: []catalog.Model{{ID: "beta-large"}, {ID: "beta-small", DefaultMaxTokens: 512}}})
	cfg.Models[SelectedModelTypeLarge] = alphaLarge
	cfg.captureExplicitModels()
	cfg.Models[SelectedModelTypeSmall] = alphaSmall
	store := testStoreWithPath(cfg, dir)
	store.knownProviders = []catalog.Provider{
		{ID: "alpha", DefaultSmallModelID: "alpha-small"},
		{ID: "beta", DefaultSmallModelID: "beta-small"},
	}

	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, betaLarge))
	require.Equal(t, betaLarge, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, betaSmall, store.Config().Models[SelectedModelTypeSmall])
	require.False(t, store.Config().modelExplicit(SelectedModelTypeSmall))
	require.Equal(t, betaLarge, store.overrides.Models[SelectedModelTypeLarge])
	_, pinnedSmall := store.overrides.Models[SelectedModelTypeSmall]
	require.False(t, pinnedSmall)

	persisted := readConfigJSON(t, store.globalDataPath)
	models := persisted["models"].(map[string]any)
	require.NotContains(t, models, string(SelectedModelTypeSmall))
}

func TestOverridePreferredModel_ImplicitSmallTracksLargeProvider(t *testing.T) {
	t.Parallel()

	cfg := &Config{}
	cfg.setDefaults(t.TempDir(), "")
	cfg.Providers.Set("alpha", ProviderConfig{ID: "alpha", Models: []catalog.Model{{ID: "alpha-large"}, {ID: "alpha-small"}}})
	cfg.Providers.Set("beta", ProviderConfig{ID: "beta", Models: []catalog.Model{{ID: "beta-small"}, {ID: "beta-large"}}})
	cfg.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "alpha", Model: "alpha-large"}
	cfg.captureExplicitModels()
	cfg.Models[SelectedModelTypeSmall] = SelectedModel{Provider: "alpha", Model: "alpha-small"}
	store := NewTestStore(cfg)

	betaLarge := SelectedModel{Provider: "beta", Model: "beta-large"}
	require.NoError(t, store.OverridePreferredModel(SelectedModelTypeLarge, betaLarge))
	require.Equal(t, SelectedModel{Provider: "beta", Model: "beta-small"}, store.Config().Models[SelectedModelTypeSmall])
	require.False(t, store.Config().modelExplicit(SelectedModelTypeSmall))
	_, pinnedSmall := store.overrides.Models[SelectedModelTypeSmall]
	require.False(t, pinnedSmall)
}

func TestUpdatePreferredModel_TypeIsolation(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	cfg.Providers.Set("openai", ProviderConfig{ID: "openai", Models: []catalog.Model{{ID: "gpt-4o"}}})
	cfg.Providers.Set("anthropic", ProviderConfig{ID: "anthropic", Models: []catalog.Model{{ID: "claude"}}})
	cfg.Providers.Set("google", ProviderConfig{ID: "google", Models: []catalog.Model{{ID: "gemini"}}})
	cfg.captureExplicitModels()
	store := testStoreWithPath(cfg, dir)

	largeModel := SelectedModel{Provider: "openai", Model: "gpt-4o"}
	smallModel := SelectedModel{Provider: "anthropic", Model: "claude"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, largeModel))
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeSmall, smallModel))

	// Adding to large leaves small untouched.
	anotherLarge := SelectedModel{Provider: "google", Model: "gemini"}
	require.NoError(t, store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, anotherLarge))

	require.Len(t, store.Config().RecentModels[SelectedModelTypeLarge], 2)
	require.Equal(t, anotherLarge, store.Config().RecentModels[SelectedModelTypeLarge][0])
	require.Len(t, store.Config().RecentModels[SelectedModelTypeSmall], 1)
	require.Equal(t, smallModel, store.Config().RecentModels[SelectedModelTypeSmall][0])
}

func TestPreferredModelMutationsRejectUnavailableSelections(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{}
	cfg.setDefaults(dir, "")
	available := SelectedModel{Provider: "available", Model: "available-model"}
	cfg.Providers.Set("available", ProviderConfig{ID: "available", Models: []catalog.Model{{ID: "available-model"}}})
	cfg.Providers.Set("unavailable", ProviderConfig{
		ID: "unavailable", Models: []catalog.Model{{ID: "unavailable-model"}},
		Plugin: &ProviderPluginReference{ID: "missing.plugin", Version: "1"},
	})
	cfg.Models[SelectedModelTypeLarge] = available
	cfg.Models[SelectedModelTypeSmall] = available
	cfg.RecentModels[SelectedModelTypeLarge] = []SelectedModel{available}
	cfg.RecentModels[SelectedModelTypeSmall] = []SelectedModel{available}
	store := testStoreWithPath(cfg, dir)
	originalDisk := []byte(`{"models":{"large":{"provider":"available","model":"available-model"}}}`)
	require.NoError(t, os.WriteFile(store.globalDataPath, originalDisk, 0o600))

	for _, test := range []struct {
		name      string
		modelType SelectedModelType
		model     SelectedModel
	}{
		{name: "large unavailable owner", modelType: SelectedModelTypeLarge, model: SelectedModel{Provider: "unavailable", Model: "unavailable-model"}},
		{name: "small missing model", modelType: SelectedModelTypeSmall, model: SelectedModel{Provider: "available", Model: "missing-model"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			require.ErrorContains(t, store.UpdatePreferredModel(ScopeGlobal, test.modelType, test.model), "is not available")
			require.Equal(t, available, store.Config().Models[test.modelType])
			require.Equal(t, []SelectedModel{available}, store.Config().RecentModels[test.modelType])
			require.Empty(t, store.overrides.Models)
			disk, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			require.Equal(t, originalDisk, disk)
		})
	}

	require.ErrorContains(t, store.OverridePreferredModel(SelectedModelTypeLarge, SelectedModel{
		Provider: "unavailable",
		Model:    "unavailable-model",
	}), "is not available")
	require.Equal(t, available, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, []SelectedModel{available}, store.Config().RecentModels[SelectedModelTypeLarge])
	require.Empty(t, store.overrides.Models)
	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, originalDisk, disk)
}
