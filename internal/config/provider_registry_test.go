package config

import (
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestComposeProviderCatalogPreservesSlotsAndRegistrationOrder(t *testing.T) {
	base := []catwalk.Provider{{ID: "first", Name: "First"}, {ID: "second", Name: "Second"}}
	integrated := []catwalk.Provider{{ID: "second", Name: "Ignored duplicate"}, {ID: "integrated", Name: "Integrated"}}
	plugins := []catwalk.Provider{{ID: "plugin-a", Name: "Plugin A"}, {ID: "plugin-b", Name: "Plugin B"}}

	catalog, err := composeProviderCatalog(base, integrated, plugins, nil)
	require.NoError(t, err)
	require.Equal(t, []catwalk.InferenceProvider{"first", "second", "integrated", "plugin-a", "plugin-b"}, providerIDs(catalog))
	require.Equal(t, "Second", catalog[1].Name)
}

func TestComposeProviderPresetsReplacesExistingSlotAndAppendsNew(t *testing.T) {
	base := []catwalk.Provider{{ID: "first", Name: "First"}, {ID: "deepseek", Name: "Cached DeepSeek"}}
	presets := []catwalk.Provider{{ID: "deepseek", Name: "Preset DeepSeek"}, {ID: "new", Name: "New"}}

	catalog := composeProviderPresets(base, presets)
	require.Equal(t, []catwalk.InferenceProvider{"first", "deepseek", "new"}, providerIDs(catalog))
	require.Equal(t, "Preset DeepSeek", catalog[1].Name)
	require.Equal(t, "Cached DeepSeek", base[1].Name)
}

func TestFilterExcludedIntegratedCatalogRemovesStaleTargets(t *testing.T) {
	catalog := []catwalk.Provider{
		{ID: "custom", Name: "Custom"},
		{ID: "codex", Name: "Stale Codex"},
		{ID: "gemini-ag", Name: "Stale Gemini"},
		{ID: "copilot", Name: "GitHub Copilot"},
	}

	filtered := filterExcludedIntegratedCatalog(catalog, providerregistry.Integrated())
	require.Equal(t, []catwalk.InferenceProvider{"custom", "copilot"}, providerIDs(filtered))
	require.Len(t, catalog, 4, "filtering must not mutate the cached source catalog")
}

func TestComposeProviderCatalogRejectsPluginOwnerConflict(t *testing.T) {
	base := []catwalk.Provider{{ID: "reserved", Name: "Existing"}}
	plugins := []catwalk.Provider{{ID: "reserved", Name: "Plugin"}, {ID: "new", Name: "New"}}

	catalog, err := composeProviderCatalog(base, nil, plugins, nil)
	require.ErrorContains(t, err, `provider plugin claim "reserved" conflicts`)
	require.Equal(t, []catwalk.InferenceProvider{"reserved", "new"}, providerIDs(catalog))
	require.Equal(t, "Existing", catalog[0].Name)
}

func TestComposeProviderCatalogRemovesDisabledCachedAndIntegratedTargets(t *testing.T) {
	base := []catwalk.Provider{{ID: "codex", Name: "Cached Codex"}, {ID: "custom", Name: "Custom"}}
	integrated := []catwalk.Provider{{ID: "gemini-ag", Name: "Integrated Gemini"}}
	modes := map[string]providerregistry.OwnerMode{
		"codex":     providerregistry.OwnerDisabled,
		"gemini-ag": providerregistry.OwnerDisabled,
	}

	catalog, err := composeProviderCatalog(base, integrated, nil, modes)
	require.NoError(t, err)
	require.Equal(t, []catwalk.InferenceProvider{"custom"}, providerIDs(catalog))
	require.Len(t, base, 2, "catalog filtering must not mutate cached input")
}

func TestComposeProviderCatalogExplicitPluginCompatPreservesSlot(t *testing.T) {
	base := []catwalk.Provider{{ID: "first", Name: "First"}}
	integrated := []catwalk.Provider{{ID: "target", Name: "Integrated"}}
	plugins := []catwalk.Provider{{ID: "target", Name: "Plugin declarations"}}

	catalog, err := composeProviderCatalog(base, integrated, plugins, map[string]providerregistry.OwnerMode{"target": providerregistry.OwnerPluginCompat})
	require.NoError(t, err)
	require.Equal(t, []catwalk.InferenceProvider{"first", "target"}, providerIDs(catalog))
	require.Equal(t, "Plugin declarations", catalog[1].Name)
}

func TestCloneProviderCatalogIsDeepEnoughForUserMerges(t *testing.T) {
	original := []catwalk.Provider{{
		ID:             "provider",
		DefaultHeaders: map[string]string{"X-Test": "original"},
		Models: []catwalk.Model{{
			ID:              "model",
			ReasoningLevels: []string{"low", "high"},
			Options: catwalk.ModelOptions{
				ProviderOptions: map[string]any{"mode": "original"},
			},
		}},
	}}

	clone := cloneProviderCatalog(original)
	clone[0].DefaultHeaders["X-Test"] = "changed"
	clone[0].Models[0].ReasoningLevels[0] = "changed"
	clone[0].Models[0].Options.ProviderOptions["mode"] = "changed"
	require.Equal(t, "original", original[0].DefaultHeaders["X-Test"])
	require.Equal(t, "low", original[0].Models[0].ReasoningLevels[0])
	require.Equal(t, "original", original[0].Models[0].Options.ProviderOptions["mode"])
}

func TestProvidersRegistersTrustedManifestCatalog(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)

	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
		Source:           source,
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	_, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{
		Digest:           status.Digest,
		Trusted:          true,
		ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	manager.Close()

	resetProviderState()
	t.Cleanup(resetProviderState)
	providers, err := Providers(&Config{Options: &Options{DisableProviderAutoUpdate: true}})
	require.NoError(t, err)
	require.Equal(t, catwalk.InferenceProvider("example-echo"), providers[len(providers)-1].ID)
	require.Equal(t, "echo-1", providers[len(providers)-1].Models[0].ID)
}

func providerIDs(providers []catwalk.Provider) []catwalk.InferenceProvider {
	result := make([]catwalk.InferenceProvider, len(providers))
	for i, provider := range providers {
		result[i] = provider.ID
	}
	return result
}
