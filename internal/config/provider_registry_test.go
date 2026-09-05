package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestComposeProviderCatalogPreservesSlotsAndRegistrationOrder(t *testing.T) {
	base := []catalog.Provider{{ID: "first", Name: "First"}, {ID: "second", Name: "Second"}}
	integrated := []catalog.Provider{{ID: "second", Name: "Ignored duplicate"}, {ID: "integrated", Name: "Integrated"}}
	plugins := []catalog.Provider{{ID: "plugin-a", Name: "Plugin A"}, {ID: "plugin-b", Name: "Plugin B"}}

	providers, err := composeProviderCatalog(base, integrated, plugins, nil)
	require.NoError(t, err)
	require.Equal(t, []catalog.ProviderID{"first", "second", "integrated", "plugin-a", "plugin-b"}, providerIDs(providers))
	require.Equal(t, "Second", providers[1].Name)
}

func TestComposeProviderPresetsPreservesCoreEntriesAndAppendsNew(t *testing.T) {
	base := []catalog.Provider{{ID: "copilot", Name: "Core Copilot"}}
	presets := []catalog.Provider{{ID: "copilot", Name: "Preset Copilot"}, {ID: "new", Name: "New"}}

	providers := composeProviderPresets(base, presets)
	require.Equal(t, []catalog.ProviderID{"copilot", "new"}, providerIDs(providers))
	require.Equal(t, "Core Copilot", providers[0].Name)
	require.Equal(t, "Core Copilot", base[0].Name)
}

func TestComposeProviderCatalogRejectsPluginOwnerConflict(t *testing.T) {
	base := []catalog.Provider{{ID: "reserved", Name: "Existing"}}
	plugins := []catalog.Provider{{ID: "reserved", Name: "Plugin"}, {ID: "new", Name: "New"}}

	providers, err := composeProviderCatalog(base, nil, plugins, nil)
	require.ErrorContains(t, err, `provider plugin claim "reserved" conflicts`)
	require.Equal(t, []catalog.ProviderID{"reserved", "new"}, providerIDs(providers))
	require.Equal(t, "Existing", providers[0].Name)
}

func TestComposeProviderCatalogRemovesDisabledCachedAndIntegratedTargets(t *testing.T) {
	base := []catalog.Provider{{ID: "codex", Name: "Cached Codex"}, {ID: "custom", Name: "Custom"}}
	integrated := []catalog.Provider{{ID: "gemini-ag", Name: "Integrated Gemini"}}
	modes := map[string]providerregistry.OwnerMode{
		"codex":     providerregistry.OwnerDisabled,
		"gemini-ag": providerregistry.OwnerDisabled,
	}

	providers, err := composeProviderCatalog(base, integrated, nil, modes)
	require.NoError(t, err)
	require.Equal(t, []catalog.ProviderID{"custom"}, providerIDs(providers))
	require.Len(t, base, 2, "catalog filtering must not mutate cached input")
}

func TestComposeProviderCatalogExplicitPluginCompatPreservesSlot(t *testing.T) {
	base := []catalog.Provider{{ID: "first", Name: "First"}}
	integrated := []catalog.Provider{{ID: "target", Name: "Integrated"}}
	plugins := []catalog.Provider{{ID: "target", Name: "Plugin declarations"}}

	providers, err := composeProviderCatalog(base, integrated, plugins, map[string]providerregistry.OwnerMode{"target": providerregistry.OwnerPluginCompat})
	require.NoError(t, err)
	require.Equal(t, []catalog.ProviderID{"first", "target"}, providerIDs(providers))
	require.Equal(t, "Plugin declarations", providers[1].Name)
}

func TestCloneProviderCatalogIsDeepEnoughForUserMerges(t *testing.T) {
	original := []catalog.Provider{{
		ID:             "provider",
		DefaultHeaders: map[string]string{"X-Test": "original"},
		Models: []catalog.Model{{
			ID:              "model",
			ReasoningLevels: []string{"low", "high"},
			Options: catalog.ModelOptions{
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
	providers, err := Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	require.Equal(t, catalog.ProviderID("example-echo"), providers[len(providers)-1].ID)
	require.Equal(t, "echo-1", providers[len(providers)-1].Models[0].ID)
}

func TestProvidersIncludePresetOnlyWhenTrusted(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)

	source, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin"))
	require.NoError(t, err)
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
		Source:           source,
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	status := snapshot.Plugins[0]
	manager.Close()

	resetProviderState()
	providers, err := Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	require.NotContains(t, providerIDs(providers), catalog.ProviderID("deepseek"))
	_, active := ActiveProviderPreset("deepseek")
	require.False(t, active)

	manager, err = providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	_, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{
		Digest:           status.Digest,
		Trusted:          true,
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	manager.Close()

	resetProviderState()
	t.Cleanup(resetProviderState)
	providers, err = Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	require.Contains(t, providerIDs(providers), catalog.ProviderID("deepseek"))
	reference, active := ActiveProviderPreset("deepseek")
	require.True(t, active)
	require.Equal(t, "crux.catwalk.deepseek", reference.ID)
	require.Equal(t, "0.51.23", reference.Version)
}

func TestFreshProviderScanRejectsPresetClaimsForCoreProviders(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileIntegrated))

	sourcePath, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin", "manifest.json"))
	require.NoError(t, err)
	source, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	coreIDs := []string{
		string(catalog.ProviderCopilot),
		string(codex.CatalogProvider().ID),
		string(gemini.CatalogProvider().ID),
	}
	for index, providerID := range coreIDs {
		bundle := filepath.Join(root, "claims", providerID+".plugin")
		require.NoError(t, os.MkdirAll(bundle, 0o755))
		manifest := source
		manifest, err = sjson.SetBytes(manifest, "id", "test.core-claim."+providerID)
		require.NoError(t, err)
		manifest, err = sjson.SetBytes(manifest, "preset.id", providerID)
		require.NoError(t, err)
		manifest, err = sjson.SetBytes(manifest, "preset.name", "Core Claim "+providerID)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), manifest, 0o600))
		_, err = manager.Install(t.Context(), providerplugin.InstallRequest{
			Source:           bundle,
			Trust:            true,
			ExpectedRevision: manager.Snapshot().Revision,
		})
		require.NoError(t, err, "install core claim %d", index)
	}
	manager.Close()

	resetProviderState()
	t.Cleanup(resetProviderState)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.Error(t, err)
	providers := scan.Providers
	for _, providerID := range coreIDs {
		require.ErrorContains(t, err, `provider preset claim "`+providerID+`" conflicts with a core provider`)
		_, active := scan.presetReferences[providerID]
		require.False(t, active)
	}

	copilotCatalog, ok := lookupProvider(providers, string(catalog.ProviderCopilot))
	require.True(t, ok)
	require.Equal(t, copilot.CatalogProvider().Name, copilotCatalog.Name)
	codexCatalog, ok := lookupProvider(providers, string(codex.CatalogProvider().ID))
	require.True(t, ok)
	require.Equal(t, codex.CatalogProvider().Name, codexCatalog.Name)
	geminiCatalog, ok := lookupProvider(providers, string(gemini.CatalogProvider().ID))
	require.True(t, ok)
	require.Equal(t, gemini.CatalogProvider().Name, geminiCatalog.Name)

	registration, ok := scan.Registry.Lookup(string(catalog.ProviderCopilot))
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCopilot, registration.Construction)
	registration, ok = scan.Registry.Lookup(string(codex.CatalogProvider().ID))
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCodex, registration.Construction)
	registration, ok = scan.Registry.Lookup(string(gemini.CatalogProvider().ID))
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionGeminiAntigravity, registration.Construction)
}

func TestFreshProviderScanRejectsNoncanonicalMigratedPresetOwner(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)

	sourcePath, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin", "manifest.json"))
	require.NoError(t, err)
	manifest, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	manifest, err = sjson.SetBytes(manifest, "id", "example.deepseek")
	require.NoError(t, err)
	bundle := filepath.Join(root, "example.deepseek.plugin")
	require.NoError(t, os.MkdirAll(bundle, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), manifest, 0o600))
	installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)

	resetProviderState()
	t.Cleanup(resetProviderState)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.ErrorContains(t, err, "must use canonical preset crux.catwalk.deepseek version 0.51.23")
	require.NotContains(t, providerIDs(scan.Providers), catalog.ProviderID("deepseek"))
	_, active := scan.presetReferences["deepseek"]
	require.False(t, active)
}

func TestFreshProviderScanRejectsExplicitlyTrustedModifiedMigratedPreset(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)

	sourcePath, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin", "manifest.json"))
	require.NoError(t, err)
	data, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "description", "Explicitly trusted local modification")
	require.NoError(t, err)
	bundle := filepath.Join(root, "crux.catwalk.deepseek.plugin")
	require.NoError(t, os.MkdirAll(bundle, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0o600))
	installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)

	resetProviderState()
	t.Cleanup(resetProviderState)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.ErrorContains(t, err, "must use canonical preset crux.catwalk.deepseek version 0.51.23 with its canonical digest")
	require.NotContains(t, providerIDs(scan.Providers), catalog.ProviderID("deepseek"))
	_, active := scan.presetReferences["deepseek"]
	require.False(t, active)
	status, found := scan.pluginStatuses["crux.catwalk.deepseek"]
	require.True(t, found)
	require.Equal(t, providerplugin.StateQuarantined, status.State)
	require.Equal(t, providerplugin.TrustTrusted, status.Trust)
	require.Len(t, status.Diagnostics, 1)
	require.Equal(t, "migrated-preset-canonical-mismatch", status.Diagnostics[0].Code)
}

func TestProvidersSelectProtectedPluginsByProfile(t *testing.T) {
	for _, owner := range []struct {
		name         string
		providerID   string
		namespace    string
		construction providerregistry.Construction
	}{
		{name: "codex", providerID: codex.ID, namespace: accounts.ProviderCodex, construction: providerregistry.ConstructionCodex},
		{name: "gemini", providerID: gemini.ID, namespace: accounts.ProviderGemini, construction: providerregistry.ConstructionGeminiAntigravity},
	} {
		for _, profile := range []struct {
			name              string
			profile           ProviderProfile
			compatibility     bool
			wantMode          providerregistry.OwnerMode
			wantConstruction  providerregistry.Construction
			wantCompatibility providerregistry.Construction
		}{
			{
				name:              "compatibility",
				profile:           ProviderProfilePluginCompat,
				compatibility:     true,
				wantMode:          providerregistry.OwnerPluginCompat,
				wantConstruction:  owner.construction,
				wantCompatibility: owner.construction,
			},
			{
				name:             "native",
				profile:          ProviderProfilePluginNative,
				wantMode:         providerregistry.OwnerPluginNative,
				wantConstruction: providerregistry.ConstructionGenericJSON,
			},
		} {
			t.Run(owner.name+"/"+profile.name, func(t *testing.T) {
				root := t.TempDir()
				dataRoot := filepath.Join(root, "data")
				cacheRoot := filepath.Join(root, "cache")
				t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
				t.Setenv("CRUX_CACHE_DIR", cacheRoot)
				t.Setenv("CRUX_PROVIDER_PROFILE", string(profile.profile))

				bundle := providerClaimBundle(t, root, owner.providerID)
				if profile.compatibility {
					bundle = providerCompatibilityClaimBundle(t, bundle, owner.namespace, owner.construction)
				}
				installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)

				scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
				require.NoError(t, err)
				provider, ok := lookupProvider(scan.Providers, owner.providerID)
				require.True(t, ok)
				require.Equal(t, "Example Echo", provider.Name)
				require.Equal(t, profile.wantMode, scan.ownerModes[owner.providerID])
				registration, ok := scan.Registry.Lookup(owner.providerID)
				require.True(t, ok)
				require.NotNil(t, registration.Manifest)
				require.Equal(t, "test.claim."+owner.providerID, registration.Manifest.ID)
				require.Equal(t, profile.wantConstruction, registration.Construction)
				require.Equal(t, profile.wantCompatibility, registration.CompatibilityAdapter)
			})
		}
	}
}

func TestFreshProviderScanRejectsReservedFullPluginsAndIgnoresIntegratedAlternates(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileIntegrated))

	for _, providerID := range []string{"deepseek", string(catalog.ProviderCopilot), codex.ID, gemini.ID} {
		bundle := providerClaimBundle(t, root, providerID)
		installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)
	}

	resetProviderState()
	t.Cleanup(resetProviderState)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.ErrorContains(t, err, `provider plugin claim "deepseek" conflicts with its reserved migrated preset`)
	providers := scan.Providers
	require.ErrorContains(t, err, `provider plugin claim "copilot" conflicts with a core provider`)
	require.NotContains(t, err.Error(), `provider plugin claim "codex" conflicts with a core provider`)
	require.NotContains(t, err.Error(), `provider plugin claim "gemini-ag" conflicts with a core provider`)
	require.NotContains(t, providerIDs(providers), catalog.ProviderID("deepseek"))

	copilotCatalog, ok := lookupProvider(providers, string(catalog.ProviderCopilot))
	require.True(t, ok)
	require.Equal(t, copilot.CatalogProvider().Name, copilotCatalog.Name)
	codexCatalog, ok := lookupProvider(providers, codex.ID)
	require.True(t, ok)
	require.Equal(t, codex.CatalogProvider().Name, codexCatalog.Name)
	geminiCatalog, ok := lookupProvider(providers, gemini.ID)
	require.True(t, ok)
	require.Equal(t, gemini.CatalogProvider().Name, geminiCatalog.Name)

	registration, ok := scan.Registry.Lookup(string(catalog.ProviderCopilot))
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCopilot, registration.Construction)
	registration, ok = scan.Registry.Lookup(codex.ID)
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCodex, registration.Construction)
	registration, ok = scan.Registry.Lookup(gemini.ID)
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionGeminiAntigravity, registration.Construction)
}

func TestFreshProviderScanPreservesExplicitCustomOwnerAgainstSameIDPlugin(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
	require.NoError(t, err)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, source)
	cfg := &Config{
		Options: &Options{},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"example-echo": {
				ID:      "example-echo",
				Owner:   &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
				Type:    catalog.TypeOpenAICompat,
				BaseURL: "https://custom.example.invalid/v1",
				Models:  []catalog.Model{{ID: "custom-model"}},
			},
		}),
	}

	scan, err := FreshProviderScan(t.Context(), cfg)
	require.NoError(t, err)
	require.NotContains(t, providerIDs(scan.Providers), catalog.ProviderID("example-echo"))
	_, registered := scan.Registry.Lookup("example-echo")
	require.False(t, registered)
}

func TestFreshProviderScanObservesNewTrustedBundleWithoutReplacingPublishedState(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	cfg := &Config{Options: &Options{}}

	resetProviderState()
	t.Cleanup(resetProviderState)
	published, err := Providers(cfg)
	require.NoError(t, err)
	require.NotContains(t, providerIDs(published), catalog.ProviderID("example-echo"))

	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
	require.NoError(t, err)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, source)

	scan, err := FreshProviderScan(t.Context(), cfg)
	require.NoError(t, err)
	require.Contains(t, providerIDs(scan.Providers), catalog.ProviderID("example-echo"))
	registration, ok := scan.Registry.Lookup("example-echo")
	require.True(t, ok)
	require.NotNil(t, registration.Manifest)

	published, err = Providers(cfg)
	require.NoError(t, err)
	require.NotContains(t, providerIDs(published), catalog.ProviderID("example-echo"))
}

func TestFreshProviderScanRejectsGenericCoreClaimsWhenCoreIsDisabled(t *testing.T) {
	for _, test := range []struct {
		name            string
		providerID      string
		disableDefaults bool
		profile         ProviderProfile
	}{
		{name: "copilot custom-only", providerID: string(catalog.ProviderCopilot), disableDefaults: true},
		{name: "codex core-only", providerID: codex.ID, profile: ProviderProfileCoreOnly},
		{name: "gemini plugin-native", providerID: gemini.ID, profile: ProviderProfilePluginNative},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(t.TempDir(), "data"))
			t.Setenv("CRUX_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))
			if test.profile != "" {
				t.Setenv("CRUX_PROVIDER_PROFILE", string(test.profile))
			}
			cfg := &Config{
				Options: &Options{DisableDefaultProviders: test.disableDefaults},
				Providers: csync.NewMapFrom(map[string]ProviderConfig{
					test.providerID: {ID: test.providerID, Type: catalog.TypeOpenAICompat, BaseURL: "https://example.invalid", Models: []catalog.Model{{ID: "custom"}}},
				}),
			}
			scan, err := FreshProviderScan(t.Context(), cfg)
			require.ErrorContains(t, err, `provider "`+test.providerID+`" is reserved for its core catalog and registration`)
			require.Empty(t, scan.Providers)
			require.Nil(t, scan.Registry)
		})
	}
}

func installTrustedProviderBundle(t *testing.T, dataRoot, cacheRoot, source string) {
	t.Helper()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{
		Source:           source,
		Trust:            true,
		ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
}

func providerClaimBundle(t *testing.T, root, providerID string) string {
	t.Helper()
	sourcePath, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	require.NoError(t, err)
	value, err := os.ReadFile(sourcePath)
	require.NoError(t, err)
	pluginID := "test.claim." + providerID
	value, err = sjson.SetBytes(value, "id", pluginID)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "provider.id", providerID)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "provider.account_namespace", pluginID)
	require.NoError(t, err)
	bundle := filepath.Join(root, pluginID+".plugin")
	require.NoError(t, os.MkdirAll(bundle, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), value, 0o600))
	return bundle
}

func providerCompatibilityClaimBundle(t *testing.T, bundle, namespace string, construction providerregistry.Construction) string {
	t.Helper()
	manifestPath := filepath.Join(bundle, "manifest.json")
	value, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "provider.account_namespace", namespace)
	require.NoError(t, err)
	protocol, transport, operationPath := "", "", ""
	switch construction {
	case providerregistry.ConstructionCodex:
		protocol, transport, operationPath = string(providerregistry.ConstructionOpenAIResponses), "websocket-json", "/"
	case providerregistry.ConstructionGeminiAntigravity:
		protocol, transport = string(providerregistry.ConstructionGeminiContent), "sse"
	default:
		t.Fatalf("unsupported compatibility construction %q", construction)
	}
	value, err = sjson.SetBytes(value, "capabilities.operations.0.protocol", protocol)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "capabilities.operations.0.transport", transport)
	require.NoError(t, err)
	if operationPath != "" {
		value, err = sjson.SetBytes(value, "capabilities.operations.0.path", operationPath)
		require.NoError(t, err)
	}
	value, err = sjson.SetBytes(value, "capabilities.compatibility_adapter", map[string]any{
		"id":        construction,
		"delegates": []string{"construction"},
		"inventory": []map[string]any{{
			"delegate":       "construction",
			"classification": "private-stateful",
			"behavior":       "Synthetic compatibility construction",
		}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, value, 0o600))
	return bundle
}

func lookupProvider(providers []catalog.Provider, id string) (catalog.Provider, bool) {
	for _, provider := range providers {
		if string(provider.ID) == id {
			return provider, true
		}
	}
	return catalog.Provider{}, false
}

func providerIDs(providers []catalog.Provider) []catalog.ProviderID {
	result := make([]catalog.ProviderID, len(providers))
	for i, provider := range providers {
		result[i] = provider.ID
	}
	return result
}
