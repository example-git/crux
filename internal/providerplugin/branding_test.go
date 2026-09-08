package providerplugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestBrandingFileOverridesInlineAndBindsTrust(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	value := readExampleManifest(t)
	value.Provider.Brand = &manifest.Brand{ShortName: "OLD", Color: "#FFFFFF"}
	writeBundleManifest(t, source, value)
	require.NoError(t, os.WriteFile(filepath.Join(source, "branding.json"), []byte(`{"short_name":"NEW","color":"#123456"}`), 0o600))
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	bundles := manager.RegisteredBundles()
	require.Len(t, bundles, 1)
	require.Equal(t, "NEW", bundles[0].Manifest.Provider.Brand.ShortName)
	registration, err := providerregistry.FromManifest(bundles[0].Manifest, bundles[0].StaticText)
	require.NoError(t, err)
	require.Equal(t, "NEW", registration.Brand.ShortName)
	require.Equal(t, "#123456", registration.Brand.Color)
	status := snapshot.Plugins[0]
	require.NoError(t, os.WriteFile(filepath.Join(manager.paths.Bundles, status.BundleName, "branding.json"), []byte(`{"short_name":"EDIT","color":"#123456"}`), 0o600))
	snapshot, err = manager.Rescan(t.Context(), snapshot.Revision)
	require.NoError(t, err)
	require.NotEqual(t, status.Digest, snapshot.Plugins[0].Digest)
	require.Equal(t, StateUntrusted, snapshot.Plugins[0].State)
	require.Empty(t, manager.RegisteredBundles())
}

func TestPresetBrandingReachesCatalog(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	data, err := os.ReadFile(filepath.Join(exampleBundle(t, "deepseek-preset.plugin"), manifestFilename))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, manifestFilename), data, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(source, "branding.json"), []byte(`{"short_name":"DEEP","gradient_a":"#123456","gradient_b":"#ABCDEF"}`), 0o600))
	snapshot, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
	providers := manager.CatalogPresets()
	require.Len(t, providers, 1)
	require.Equal(t, "DEEP", providers[0].Brand.ShortName)
	require.Equal(t, "#123456", providers[0].Brand.GradientA)
	providers[0].Brand.ShortName = "MUTATED"
	require.Equal(t, "DEEP", manager.CatalogPresets()[0].Brand.ShortName)
	require.Empty(t, manager.RegisteredBundles())
}

func TestBundledProviderPresetBrandingRegisters(t *testing.T) {
	root := generatedPresetRoot(t)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		t.Run(entry.Name(), func(t *testing.T) {
			source := filepath.Join(root, entry.Name())
			data, err := os.ReadFile(filepath.Join(source, "branding.json"))
			require.NoError(t, err)
			branding, err := manifest.DecodeBrandingStrict(data)
			require.NoError(t, err)
			require.NotEmpty(t, branding.Label)
			require.NotEmpty(t, branding.ShortName)
			require.NotEmpty(t, branding.Color)
			require.NotEmpty(t, branding.GradientA)
			require.NotEmpty(t, branding.GradientB)
			manager := newTestManager(t)
			snapshot, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true})
			require.NoError(t, err)
			require.Len(t, snapshot.Plugins, 1)
			require.Equal(t, StateRegistered, snapshot.Plugins[0].State)
			providers := manager.CatalogPresets()
			require.Len(t, providers, 1)
			require.Equal(t, providers[0].Name, branding.Label)
			require.Equal(t, &branding, providers[0].Brand)
		})
	}
}

func TestInvalidBrandingDoesNotFallBackToInline(t *testing.T) {
	manager := newTestManager(t)
	source := t.TempDir()
	value := readExampleManifest(t)
	value.Provider.Brand = &manifest.Brand{ShortName: "OLD", Color: "#FFFFFF"}
	writeBundleManifest(t, source, value)
	require.NoError(t, os.WriteFile(filepath.Join(source, "branding.json"), []byte(`{"color":"invalid"}`), 0o600))
	_, err := manager.Install(t.Context(), InstallRequest{Source: source, Trust: true})
	require.Error(t, err)
	require.Empty(t, manager.RegisteredBundles())
}
