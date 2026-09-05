package config

import (
	"os"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func addOwnerMatrixAvailableProvider(t *testing.T, configPath string) {
	t.Helper()
	value, err := os.ReadFile(configPath)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "providers.available", map[string]any{
		"api_key":  "available-key",
		"base_url": "https://available.example.invalid/v1",
		"type":     "openai-compat",
		"models":   []map[string]any{{"id": "available-model", "name": "Available Model"}},
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, value, 0o600))
}

func requireOwnerMismatchRetention(t *testing.T, store *ConfigStore, providerID string, large, small SelectedModel) {
	t.Helper()
	require.Equal(t, large, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, small, store.Config().Models[SelectedModelTypeSmall])
	require.False(t, store.Config().IsProviderIntegrationAvailable(providerID))
	require.True(t, store.Config().IsProviderAvailable("available"))
	require.False(t, store.Config().CanInitializeAgent())
}

func TestSameIDOwnerTupleMaskingMatrix(t *testing.T) {
	plugin := ownerTestRegistration("owner-test", "plugin.one")
	pluginProvider := func(id, version string) ProviderConfig {
		return ProviderConfig{
			ID:     plugin.ProviderID,
			Owner:  providerOwnerReferenceForRegistration(plugin),
			Plugin: &ProviderPluginReference{ID: id, Version: version},
		}
	}
	activePreset := ProviderPresetReference{ID: "preset.one", Version: "1.0.0", Digest: "sha256:active"}
	presetProvider := func(id, version, digest string) ProviderConfig {
		return ProviderConfig{
			ID:     plugin.ProviderID,
			Owner:  &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
			Preset: &ProviderPresetReference{ID: id, Version: version, Digest: digest},
		}
	}
	customProvider := ProviderConfig{
		ID:    plugin.ProviderID,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}

	var core providerregistry.Registration
	for _, registration := range providerregistry.Integrated() {
		if registration.ProviderID == codex.ID {
			core = registration
			break
		}
	}
	require.Equal(t, codex.ID, core.ProviderID)
	coreProvider := ProviderConfig{
		ID:    core.ProviderID,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: core.Construction},
	}
	pluginCore := ownerTestRegistration(core.ProviderID, "plugin.codex")
	pluginCore.Construction = core.Construction
	pluginCore.CompatibilityAdapter = core.Construction
	pluginCoreProvider := ProviderConfig{
		ID:     core.ProviderID,
		Owner:  providerOwnerReferenceForRegistration(pluginCore),
		Plugin: &ProviderPluginReference{ID: pluginCore.Manifest.ID, Version: pluginCore.Manifest.Version},
	}
	protectedCustom := ProviderConfig{
		ID:    core.ProviderID,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
	}
	disabledPlugin := pluginProvider(plugin.Manifest.ID, plugin.Manifest.Version)
	disabledPlugin.Disable = true
	legacyOAuthCustom := customProvider
	legacyOAuthCustom.Type = catalog.TypeOpenAI
	legacyOAuthCustom.OAuthToken = &oauth.Token{AccessToken: "secret"}
	disabledCustom := customProvider
	disabledCustom.Disable = true

	for _, test := range []struct {
		name                   string
		providerID             string
		provider               ProviderConfig
		registrations          []providerregistry.Registration
		presets                map[string]ProviderPresetReference
		owner                  providerregistry.RegistrationOwner
		owned                  bool
		registered             bool
		integrationAvailable   bool
		available              bool
		constructible          bool
		constructionRegistered bool
	}{
		{
			name: "matching plugin ID and version", providerID: plugin.ProviderID,
			provider: pluginProvider(plugin.Manifest.ID, plugin.Manifest.Version), registrations: []providerregistry.Registration{plugin},
			owner: plugin.Owner(), owned: true, registered: true, integrationAvailable: true, available: true, constructible: true, constructionRegistered: true,
		},
		{
			name: "mismatched plugin ID", providerID: plugin.ProviderID,
			provider: pluginProvider("plugin.other", plugin.Manifest.Version), registrations: []providerregistry.Registration{plugin},
		},
		{
			name: "mismatched plugin version", providerID: plugin.ProviderID,
			provider: pluginProvider(plugin.Manifest.ID, "2.0.0"), registrations: []providerregistry.Registration{plugin},
		},
		{
			name: "inactive plugin", providerID: plugin.ProviderID,
			provider: pluginProvider(plugin.Manifest.ID, plugin.Manifest.Version),
		},
		{
			name: "disabled plugin", providerID: plugin.ProviderID,
			provider: disabledPlugin, registrations: []providerregistry.Registration{plugin},
			owner: plugin.Owner(), owned: true, registered: true, integrationAvailable: true,
		},
		{
			name: "matching preset ID version and digest", providerID: plugin.ProviderID,
			provider: presetProvider(activePreset.ID, activePreset.Version, activePreset.Digest),
			presets:  map[string]ProviderPresetReference{plugin.ProviderID: activePreset},
			owner:    providerregistry.RegistrationOwner{ProviderID: plugin.ProviderID, HasPreset: true, PresetID: activePreset.ID, PresetVersion: activePreset.Version, PresetDigest: activePreset.Digest},
			owned:    true, integrationAvailable: true, available: true, constructible: true,
		},
		{
			name: "mismatched preset ID", providerID: plugin.ProviderID,
			provider: presetProvider("preset.other", activePreset.Version, activePreset.Digest),
			presets:  map[string]ProviderPresetReference{plugin.ProviderID: activePreset},
		},
		{
			name: "mismatched preset version", providerID: plugin.ProviderID,
			provider: presetProvider(activePreset.ID, "2.0.0", activePreset.Digest),
			presets:  map[string]ProviderPresetReference{plugin.ProviderID: activePreset},
		},
		{
			name: "mismatched preset digest", providerID: plugin.ProviderID,
			provider: presetProvider(activePreset.ID, activePreset.Version, "sha256:other"),
			presets:  map[string]ProviderPresetReference{plugin.ProviderID: activePreset},
		},
		{
			name: "inactive preset", providerID: plugin.ProviderID,
			provider: presetProvider(activePreset.ID, activePreset.Version, activePreset.Digest),
		},
		{
			name: "matching core claim", providerID: core.ProviderID,
			provider: coreProvider, registrations: []providerregistry.Registration{core},
			owner: core.Owner(), owned: true, registered: true, integrationAvailable: true, available: true, constructible: true, constructionRegistered: true,
		},
		{
			name: "plugin cannot mask core claim", providerID: core.ProviderID,
			provider: coreProvider, registrations: []providerregistry.Registration{pluginCore},
		},
		{
			name: "core cannot mask plugin claim", providerID: core.ProviderID,
			provider: pluginCoreProvider, registrations: []providerregistry.Registration{core},
		},
		{
			name: "inactive core", providerID: core.ProviderID,
			provider: coreProvider,
		},
		{
			name: "custom claim ignores same ID plugin", providerID: plugin.ProviderID,
			provider: customProvider, registrations: []providerregistry.Registration{plugin},
			owner: providerregistry.RegistrationOwner{ProviderID: plugin.ProviderID}, owned: true, integrationAvailable: true, available: true, constructible: true,
		},
		{
			name: "protected custom claim is rejected", providerID: core.ProviderID,
			provider: protectedCustom, registrations: []providerregistry.Registration{core},
		},
		{
			name: "legacy OAuth custom claim is rejected", providerID: plugin.ProviderID,
			provider: legacyOAuthCustom, registrations: []providerregistry.Registration{plugin},
		},
		{
			name: "disabled custom", providerID: plugin.ProviderID,
			provider: disabledCustom, registrations: []providerregistry.Registration{plugin},
			owner: providerregistry.RegistrationOwner{ProviderID: plugin.ProviderID}, owned: true, integrationAvailable: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := providerregistry.New(test.registrations...)
			require.NoError(t, err)
			cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{test.providerID: test.provider})}
			cfg.bindProviderScan(ProviderScan{Registry: registry, presetReferences: test.presets})
			snapshot := RuntimeSnapshot{config: cfg, registry: registry}

			owner, owned := snapshot.ProviderOwner(test.providerID)
			require.Equal(t, test.owned, owned)
			if test.owned {
				require.Equal(t, test.owner, owner)
			}
			registration, registered := snapshot.ProviderRegistration(test.providerID)
			require.Equal(t, test.registered, registered)
			if test.registered {
				require.Equal(t, test.owner, registration.Owner())
			}
			require.Equal(t, test.integrationAvailable, cfg.IsProviderIntegrationAvailable(test.providerID))
			require.Equal(t, test.available, cfg.IsProviderAvailable(test.providerID))

			resolved, constructionRegistration, constructionRegistered, err := snapshot.ProviderForConstruction(test.providerID, test.provider)
			if !test.constructible {
				require.Error(t, err)
				require.Empty(t, resolved.ID)
				require.Empty(t, constructionRegistration.ProviderID)
				require.False(t, constructionRegistered)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.providerID, resolved.ID)
			require.Equal(t, test.constructionRegistered, constructionRegistered)
			if test.constructionRegistered {
				require.Equal(t, test.owner, constructionRegistration.Owner())
			} else {
				require.Empty(t, constructionRegistration.ProviderID)
			}
		})
	}
}

func TestOwnerMismatchRetainsFullSelectionsAcrossReloadAndStartup(t *testing.T) {
	t.Run("plugin version", func(t *testing.T) {
		store, configPath, paths, status := setupReloadPluginStore(t)
		large := store.Config().Models[SelectedModelTypeLarge]
		small := store.Config().Models[SelectedModelTypeSmall]
		addOwnerMatrixAvailableProvider(t, configPath)
		replaceReloadPluginVersion(t, paths, status, "2.0.0")

		require.NoError(t, store.ReloadFromDisk(t.Context()))
		requireOwnerMismatchRetention(t, store, "example-echo", large, small)
		provider, ok := store.Config().Providers.Get("example-echo")
		require.True(t, ok)
		require.Equal(t, "1.0.0", provider.Plugin.Version)
		_, registered := store.ProviderRegistration("example-echo")
		require.False(t, registered)

		restarted, err := Load(store.WorkingDir(), t.TempDir(), false)
		require.NoError(t, err)
		requireOwnerMismatchRetention(t, restarted, "example-echo", large, small)
		provider, ok = restarted.Config().Providers.Get("example-echo")
		require.True(t, ok)
		require.Equal(t, "1.0.0", provider.Plugin.Version)
	})

	t.Run("preset digest", func(t *testing.T) {
		store, configPath, _, _ := setupReloadPresetStore(t)
		large := store.Config().Models[SelectedModelTypeLarge]
		small := store.Config().Models[SelectedModelTypeSmall]
		addOwnerMatrixAvailableProvider(t, configPath)
		dataPath := GlobalConfigData()
		value, err := os.ReadFile(dataPath)
		require.NoError(t, err)
		value, err = sjson.SetBytes(value, "providers.deepseek.preset.digest", "sha256:t3-12-mismatch")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dataPath, value, 0o600))

		require.NoError(t, store.ReloadFromDisk(t.Context()))
		requireOwnerMismatchRetention(t, store, "deepseek", large, small)
		provider, ok := store.Config().Providers.Get("deepseek")
		require.True(t, ok)
		require.Equal(t, "sha256:t3-12-mismatch", provider.Preset.Digest)

		restarted, err := Load(store.WorkingDir(), t.TempDir(), false)
		require.NoError(t, err)
		requireOwnerMismatchRetention(t, restarted, "deepseek", large, small)
		provider, ok = restarted.Config().Providers.Get("deepseek")
		require.True(t, ok)
		require.Equal(t, "sha256:t3-12-mismatch", provider.Preset.Digest)
	})
}

func TestDefaultSelectionSkipsExactOwnerMismatches(t *testing.T) {
	available := ProviderConfig{
		ID:      "available",
		Type:    catalog.TypeOpenAICompat,
		Owner:   &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
		Models:  []catalog.Model{{ID: "available-model"}},
		BaseURL: "https://available.example.invalid/v1",
	}
	availableCatalog := catalog.Provider{
		ID:                  catalog.ProviderID(available.ID),
		DefaultLargeModelID: "available-model",
		DefaultSmallModelID: "available-model",
		Models:              available.Models,
	}

	t.Run("plugin version", func(t *testing.T) {
		registration := providerregistry.Registration{
			ProviderID:   "plugin-mismatch",
			Construction: providerregistry.ConstructionGenericJSON,
			Manifest:     &manifest.Manifest{ID: "plugin.mismatch", Version: "2.0.0"},
		}
		registry, err := providerregistry.New(registration)
		require.NoError(t, err)
		mismatched := ProviderConfig{
			ID:     registration.ProviderID,
			Owner:  &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: registration.Construction},
			Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: "1.0.0"},
			Models: []catalog.Model{{ID: "plugin-model"}},
		}
		catalog := []catalog.Provider{
			{ID: catalog.ProviderID(mismatched.ID), DefaultLargeModelID: "plugin-model", DefaultSmallModelID: "plugin-model", Models: mismatched.Models},
			availableCatalog,
		}
		cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{mismatched.ID: mismatched, available.ID: available})}
		cfg.bindProviderScan(ProviderScan{Providers: catalog, Registry: registry})

		require.False(t, cfg.IsProviderAvailable(mismatched.ID))
		large, small, err := cfg.defaultModelSelection(catalog)
		require.NoError(t, err)
		require.Equal(t, available.ID, large.Provider)
		require.Equal(t, available.ID, small.Provider)
	})

	t.Run("preset digest", func(t *testing.T) {
		registry, err := providerregistry.New()
		require.NoError(t, err)
		active := ProviderPresetReference{ID: "preset.mismatch", Version: "1.0.0", Digest: "sha256:active"}
		mismatched := ProviderConfig{
			ID:     "preset-mismatch",
			Owner:  &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
			Preset: &ProviderPresetReference{ID: active.ID, Version: active.Version, Digest: "sha256:persisted"},
			Models: []catalog.Model{{ID: "preset-model"}},
		}
		catalog := []catalog.Provider{
			{ID: catalog.ProviderID(mismatched.ID), DefaultLargeModelID: "preset-model", DefaultSmallModelID: "preset-model", Models: mismatched.Models},
			availableCatalog,
		}
		cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{mismatched.ID: mismatched, available.ID: available})}
		cfg.bindProviderScan(ProviderScan{
			Providers:        catalog,
			Registry:         registry,
			presetReferences: map[string]ProviderPresetReference{mismatched.ID: active},
		})

		require.False(t, cfg.IsProviderAvailable(mismatched.ID))
		large, small, err := cfg.defaultModelSelection(catalog)
		require.NoError(t, err)
		require.Equal(t, available.ID, large.Provider)
		require.Equal(t, available.ID, small.Provider)
	})
}
