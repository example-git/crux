package providerplugin

import (
	"os"
	"path/filepath"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestCatalogProviderPreservesManifestOrderAndDefaults(t *testing.T) {
	value := readExampleManifestNamed(t, "responses-oauth.plugin")

	provider, err := catalogProvider(value)
	require.NoError(t, err)
	require.Equal(t, catwalk.InferenceProvider("example-responses"), provider.ID)
	require.Equal(t, "https://api.example.invalid", provider.APIEndpoint)
	require.Equal(t, catwalk.TypeOpenAI, provider.Type)
	require.Equal(t, "example-reasoner", provider.DefaultLargeModelID)
	require.Equal(t, "example-small", provider.DefaultSmallModelID)
	require.Equal(t, []string{"example-reasoner", "example-small"}, []string{provider.Models[0].ID, provider.Models[1].ID})
	require.True(t, provider.Models[0].CanReason)
	require.Equal(t, []string{"low", "medium", "high"}, provider.Models[0].ReasoningLevels)
	require.Equal(t, "medium", provider.Models[0].DefaultReasoningEffort)
	require.True(t, provider.Models[0].SupportsImages)
	require.Equal(t, "auto", provider.Models[0].Options.ProviderOptions["reasoning_summary"])
}

func TestCatalogProviderLeavesGenericJSONWithoutLegacyAdapter(t *testing.T) {
	provider, err := catalogProvider(readExampleManifest(t))
	require.NoError(t, err)
	require.Empty(t, provider.Type)
}

func TestCatalogPresetPreservesCatwalkProviderData(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "deepseek-preset.plugin", "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodePresetStrict(data)
	require.NoError(t, err)

	provider := catalogPreset(value.Preset)
	require.Equal(t, catwalk.InferenceProvider("deepseek"), provider.ID)
	require.Equal(t, catwalk.TypeOpenAICompat, provider.Type)
	require.Equal(t, "$DEEPSEEK_API_KEY", provider.APIKey)
	require.Equal(t, "https://api.deepseek.com/v1", provider.APIEndpoint)
	require.Equal(t, "deepseek-v4-pro", provider.DefaultLargeModelID)
	require.Equal(t, "deepseek-v4-flash", provider.DefaultSmallModelID)
	require.Len(t, provider.Models, 2)
	require.Equal(t, []string{"low", "high", "max"}, provider.Models[0].ReasoningLevels)
}

func TestManagerCatalogSeparatesProviderPresets(t *testing.T) {
	manager := newTestManager(t)
	snapshot, err := manager.Install(t.Context(), InstallRequest{
		Source:           exampleBundle(t, "deepseek-preset.plugin"),
		Trust:            true,
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Equal(t, manifest.PluginTypeProviderPreset, snapshot.Plugins[0].PluginType)
	require.Equal(t, []string{"provider-preset"}, snapshot.Plugins[0].Capabilities)
	require.Empty(t, manager.RegisteredBundles())
	require.Len(t, manager.RegisteredPresetBundles(), 1)
	providers, err := manager.CatalogProviders()
	require.NoError(t, err)
	require.Empty(t, providers)
	presets := manager.CatalogPresets()
	require.Len(t, presets, 1)
	require.Equal(t, catwalk.InferenceProvider("deepseek"), presets[0].ID)
}

func TestManagerCatalogIncludesOnlyRegisteredPlugins(t *testing.T) {
	manager := newTestManager(t)
	snapshot, err := manager.Install(t.Context(), InstallRequest{
		Source:           exampleBundle(t, "minimal.plugin"),
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	providers, err := manager.CatalogProviders()
	require.NoError(t, err)
	require.Empty(t, providers)

	status := snapshot.Plugins[0]
	_, err = manager.SetTrust(t.Context(), status.ID, TrustRequest{
		Digest:           status.Digest,
		Trusted:          true,
		ExpectedRevision: snapshot.Revision,
	})
	require.NoError(t, err)
	providers, err = manager.CatalogProviders()
	require.NoError(t, err)
	require.Len(t, providers, 1)
	require.Equal(t, catwalk.InferenceProvider("example-echo"), providers[0].ID)
}
