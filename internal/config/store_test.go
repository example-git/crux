package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestApplyEphemeralProviderStateDoesNotWriteCredentials(t *testing.T) {
	root := t.TempDir()
	providers := csync.NewMap[string, ProviderConfig]()
	cfg := &Config{Providers: providers}
	cfg.setDefaults(root, filepath.Join(root, "state"))
	store := NewTestStore(cfg)
	store.workingDir = root

	registration, ok := store.ProviderRegistration("codex")
	require.True(t, ok)
	owner := registration.Owner()
	forwardedProvider := ProviderConfig{ID: "remote", APIKey: "provider-secret"}
	forwardedAccount := accounts.Entry{ID: "account", AccessToken: "account-secret", Raw: []byte(`{"account_id":"forwarded"}`)}
	require.NoError(t, store.ApplyEphemeralProviderState(
		map[string]ProviderConfig{"remote": forwardedProvider},
		map[string]ForwardedAccount{owner.AccountNamespace: {Owner: owner, Entry: forwardedAccount}},
	))

	actualProvider, ok := store.Config().Providers.Get("remote")
	require.True(t, ok)
	require.Equal(t, "provider-secret", actualProvider.APIKey)
	actualAccount, ok := store.EphemeralAccount(owner)
	require.True(t, ok)
	require.Equal(t, "account-secret", actualAccount.AccessToken)
	actualAccount.Raw[0] = 'x'
	unchangedAccount, ok := store.EphemeralAccount(owner)
	require.True(t, ok)
	require.JSONEq(t, `{"account_id":"forwarded"}`, string(unchangedAccount.Raw))

	_, err := os.Stat(filepath.Join(root, "state", "crux.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestApplyEphemeralProviderStateRejectsUnownedMigratedProviders(t *testing.T) {
	providerIDs := []string{
		"aihubmix", "alibaba-singapore", "alibaba-us", "atlascloud", "avian", "baseten", "cerebras", "chutes", "deepseek", "fireworks", "groq", "huggingface", "ionet", "moonshot", "nebius", "neuralwatt", "opencode-go", "opencode-zen", "qiniucloud", "scaleway", "synthetic", "venice", "xai", "zai", "zhipu", "zhipu-coding",
	}
	for _, providerID := range providerIDs {
		t.Run(providerID, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			store := NewTestStore(cfg)

			err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{
				providerID: {ID: providerID, Type: catalog.TypeOpenAICompat},
			}, nil)
			require.ErrorContains(t, err, "must use canonical preset")
			_, found := store.Config().Providers.Get(providerID)
			require.False(t, found)
		})
	}
}

func TestApplyEphemeralProviderStatePreservesCanonicalUnavailableMigratedProvider(t *testing.T) {
	registry, err := providerregistry.New()
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(t.TempDir(), t.TempDir())
	cfg.bindProviderScan(ProviderScan{Registry: registry})
	store := NewTestStore(cfg)
	presetID, version, migrated := providerplugin.MigratedProviderPreset("deepseek")
	require.True(t, migrated)
	forwarded := ProviderConfig{
		ID:     "deepseek",
		Type:   catalog.TypeOpenAICompat,
		Preset: &ProviderPresetReference{ID: presetID, Version: version},
	}

	require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{"deepseek": forwarded}, nil))
	actual, found := store.Config().Providers.Get("deepseek")
	require.True(t, found)
	require.Equal(t, forwarded.Preset, actual.Preset)
}

func TestApplyEphemeralProviderStateRejectsNoncanonicalActiveMigratedPreset(t *testing.T) {
	registry, err := providerregistry.New()
	require.NoError(t, err)
	presetID, version, _, migrated := providerplugin.CanonicalMigratedProviderPreset("deepseek")
	require.True(t, migrated)
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(t.TempDir(), t.TempDir())
	cfg.bindProviderScan(ProviderScan{
		Registry: registry,
		presetReferences: map[string]ProviderPresetReference{
			"deepseek": {ID: presetID, Version: version, Digest: "noncanonical"},
		},
	})
	store := NewTestStore(cfg)

	err = store.ApplyEphemeralProviderState(map[string]ProviderConfig{
		"deepseek": {ID: "deepseek", Preset: &ProviderPresetReference{ID: presetID, Version: version}},
	}, nil)
	require.ErrorContains(t, err, "canonical active preset digest")
	_, found := store.Config().Providers.Get("deepseek")
	require.False(t, found)
}

func TestApplyEphemeralProviderStateRejectsInvalidMigratedOwnership(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider ProviderConfig
	}{
		{name: "plugin", provider: ProviderConfig{Plugin: &ProviderPluginReference{ID: "plugin.deepseek", Version: "1.0.0"}}},
		{name: "wrong preset ID", provider: ProviderConfig{Preset: &ProviderPresetReference{ID: "crux.catwalk.other", Version: "0.51.23"}}},
		{name: "wrong preset version", provider: ProviderConfig{Preset: &ProviderPresetReference{ID: "crux.catwalk.deepseek", Version: "0.51.22"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			store := NewTestStore(cfg)
			test.provider.ID = "deepseek"

			err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{"deepseek": test.provider}, nil)
			require.ErrorContains(t, err, "must use canonical preset crux.catwalk.deepseek version 0.51.23")
		})
	}
}

func TestApplyEphemeralProviderStateRejectsAmbiguousOwnership(t *testing.T) {
	for _, test := range []struct {
		name       string
		providerID string
		provider   ProviderConfig
		errorText  string
	}{
		{
			name:       "conflicting provider ID",
			providerID: "custom",
			provider:   ProviderConfig{ID: "other"},
			errorText:  `declares conflicting ID "other"`,
		},
		{
			name:       "plugin and preset markers",
			providerID: "custom",
			provider: ProviderConfig{
				ID:     "custom",
				Plugin: &ProviderPluginReference{ID: "example.plugin", Version: "1.0.0"},
				Preset: &ProviderPresetReference{ID: "example.preset", Version: "1.0.0"},
			},
			errorText: "declares both plugin and preset ownership",
		},
		{
			name:       "core plugin marker",
			providerID: "copilot",
			provider: ProviderConfig{
				ID:     "copilot",
				Plugin: &ProviderPluginReference{ID: "example.plugin", Version: "1.0.0"},
			},
			errorText: "is reserved for its core catalog and registration",
		},
		{
			name:       "core preset marker",
			providerID: "codex",
			provider: ProviderConfig{
				ID:     "codex",
				Preset: &ProviderPresetReference{ID: "example.preset", Version: "1.0.0"},
			},
			errorText: "cannot use preset ownership",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			store := NewTestStore(cfg)

			err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{test.providerID: test.provider}, nil)
			require.ErrorContains(t, err, test.errorText)
			_, found := store.Config().Providers.Get(test.providerID)
			require.False(t, found)
		})
	}
}

func TestApplyEphemeralProviderStateRequiresActiveCoreOwner(t *testing.T) {
	registry, err := providerregistry.New()
	require.NoError(t, err)
	for _, providerID := range []string{"copilot", "codex", "gemini-ag"} {
		t.Run(providerID, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			cfg.bindProviderScan(ProviderScan{Registry: registry})
			store := NewTestStore(cfg)

			err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{
				providerID: {ID: providerID, Type: catalog.TypeOpenAICompat},
			}, nil)
			require.ErrorContains(t, err, "requires its active core registration")
			_, found := store.Config().Providers.Get(providerID)
			require.False(t, found)
		})
	}
}

func TestApplyEphemeralProviderStateAcceptsExactActiveCoreOwner(t *testing.T) {
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	for _, providerID := range []string{"copilot", "codex", "gemini-ag"} {
		t.Run(providerID, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			cfg.setDefaults(t.TempDir(), t.TempDir())
			cfg.bindProviderScan(ProviderScan{Registry: registry})
			store := NewTestStore(cfg)

			require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{
				providerID: {ID: providerID},
			}, nil))
			_, found := store.Config().Providers.Get(providerID)
			require.True(t, found)
		})
	}
}

func TestApplyEphemeralProviderStateAcceptsExactProfileSelectedPluginOwners(t *testing.T) {
	for _, test := range []struct {
		providerID           string
		construction         providerregistry.Construction
		compatibilityAdapter providerregistry.Construction
	}{
		{providerID: "codex", construction: providerregistry.ConstructionCodex, compatibilityAdapter: providerregistry.ConstructionCodex},
		{providerID: "gemini-ag", construction: providerregistry.ConstructionGenericJSON},
	} {
		t.Run(test.providerID, func(t *testing.T) {
			pluginID := "plugin." + test.providerID
			registration := ownerTestRegistration(test.providerID, pluginID)
			registration.Construction = test.construction
			registration.CompatibilityAdapter = test.compatibilityAdapter
			registry, err := providerregistry.New(registration)
			require.NoError(t, err)
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			cfg.setDefaults(t.TempDir(), t.TempDir())
			cfg.bindProviderScan(ProviderScan{Registry: registry})
			store := NewTestStore(cfg)
			forwarded := ProviderConfig{
				ID:     test.providerID,
				Plugin: &ProviderPluginReference{ID: pluginID, Version: "1.0.0"},
			}

			require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{test.providerID: forwarded}, nil))
			actual, found := store.Config().Providers.Get(test.providerID)
			require.True(t, found)
			require.Equal(t, forwarded.Plugin, actual.Plugin)
		})
	}
}

func TestApplyEphemeralProviderStateRejectsProfileSelectedPluginOwnerMismatch(t *testing.T) {
	registration := ownerTestRegistration("codex", "plugin.codex")
	registration.Construction = providerregistry.ConstructionCodex
	registration.CompatibilityAdapter = providerregistry.ConstructionCodex
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	for _, test := range []struct {
		name      string
		provider  ProviderConfig
		errorText string
	}{
		{name: "missing marker", provider: ProviderConfig{ID: "codex"}, errorText: "active provider plugin"},
		{name: "wrong plugin", provider: ProviderConfig{ID: "codex", Plugin: &ProviderPluginReference{ID: "plugin.other", Version: "1.0.0"}}, errorText: "active provider plugin"},
		{name: "wrong version", provider: ProviderConfig{ID: "codex", Plugin: &ProviderPluginReference{ID: "plugin.codex", Version: "2.0.0"}}, errorText: "active provider plugin"},
		{name: "preset marker", provider: ProviderConfig{ID: "codex", Preset: &ProviderPresetReference{ID: "preset.codex", Version: "1.0.0"}}, errorText: "cannot use preset ownership"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
			cfg.setDefaults(t.TempDir(), t.TempDir())
			cfg.bindProviderScan(ProviderScan{Registry: registry})
			store := NewTestStore(cfg)

			err := store.ApplyEphemeralProviderState(map[string]ProviderConfig{"codex": test.provider}, nil)
			require.ErrorContains(t, err, test.errorText)
			_, found := store.Config().Providers.Get("codex")
			require.False(t, found)
		})
	}
}

func TestApplyEphemeralProviderStateRejectsPersistedOwnerCollisions(t *testing.T) {
	for _, test := range []struct {
		name     string
		provider ProviderConfig
	}{
		{name: "plugin", provider: ProviderConfig{Plugin: &ProviderPluginReference{ID: "persisted.plugin", Version: "1.2.3"}}},
		{name: "preset", provider: ProviderConfig{Preset: &ProviderPresetReference{ID: "persisted.preset", Version: "4.5.6"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owned": test.provider})}
			store := NewTestStore(cfg)
			original := store.Config()

			err := store.ApplyEphemeralProviderState(
				map[string]ProviderConfig{"owned": {ID: "owned", APIKey: "forwarded"}},
				nil,
			)
			require.ErrorContains(t, err, `forwarded provider "owned" conflicts with its persisted provider owner`)
			require.Same(t, original, store.Config())
			require.Empty(t, store.ephemeralAccounts)
			require.Empty(t, store.ephemeralProviderSnapshot())
		})
	}
}

func TestApplyEphemeralProviderStatePreservesNonreservedProviders(t *testing.T) {
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(t.TempDir(), t.TempDir())
	store := NewTestStore(cfg)
	providers := map[string]ProviderConfig{
		"custom": {ID: "custom", Type: catalog.TypeOpenAICompat},
		"plugin-owned": {
			ID:     "plugin-owned",
			Plugin: &ProviderPluginReference{ID: "example.plugin", Version: "1.2.3"},
		},
		"preset-owned": {
			ID:     "preset-owned",
			Preset: &ProviderPresetReference{ID: "example.preset", Version: "4.5.6"},
		},
	}

	require.NoError(t, store.ApplyEphemeralProviderState(providers, nil))
	for providerID, expected := range providers {
		actual, found := store.Config().Providers.Get(providerID)
		require.True(t, found)
		require.Equal(t, expected.Plugin, actual.Plugin)
		require.Equal(t, expected.Preset, actual.Preset)
	}
}

func TestApplyEphemeralProviderStateValidationFailurePublishesNothing(t *testing.T) {
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"existing": {ID: "existing", APIKey: "unchanged"},
	})}
	store := NewTestStore(cfg)
	original := store.Config()

	err := store.ApplyEphemeralProviderState(
		map[string]ProviderConfig{
			"custom":   {ID: "custom", Type: catalog.TypeOpenAICompat},
			"deepseek": {ID: "deepseek", Type: catalog.TypeOpenAICompat},
		},
		nil,
	)
	require.ErrorContains(t, err, "must use canonical preset")
	require.Same(t, original, store.Config())
	_, found := store.Config().Providers.Get("custom")
	require.False(t, found)
	require.Empty(t, store.ephemeralAccounts)
	require.Empty(t, store.ephemeralProviderSnapshot())
	require.Empty(t, store.ephemeralProviders)
}

func TestApplyEphemeralProviderStateRebuildsAgents(t *testing.T) {
	root := t.TempDir()
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig]()}
	cfg.setDefaults(root, filepath.Join(root, "state"))
	cfg.SetupAgents()
	store := NewTestStore(cfg)
	store.workingDir = root

	require.NoError(t, store.ApplyEphemeralProviderState(
		map[string]ProviderConfig{"remote": {ID: "remote", Type: "openai-compat"}},
		nil,
	))

	require.Equal(t, AgentCoder, store.Config().Agents[AgentCoder].ID)
	require.Equal(t, AgentTask, store.Config().Agents[AgentTask].ID)
}

func TestEphemeralOAuthRefreshSurvivesConfigurationReload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", root)
	t.Setenv("CRUX_GLOBAL_DATA", root)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath := filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"remote":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"disk","models":[{"id":"model","name":"Model"}]}}}`), 0o600))
	store, err := Load(root, root, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	registration := providerregistry.Registration{
		ProviderID:   "remote",
		Construction: providerregistry.ConstructionGenericJSON,
		Manifest: &manifest.Manifest{
			ID:      "plugin.remote",
			Version: "1.0.0",
			Configuration: manifest.Configuration{Schema: map[string]any{
				"type":                 "object",
				"properties":           map[string]any{},
				"additionalProperties": false,
			}},
		},
		OAuth: &providerregistry.OAuthCapability{},
	}
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	store.providerRegistry = registry

	oldToken := &oauth.Token{AccessToken: "old-access", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
	forwarded := ProviderConfig{
		ID: "remote", Type: "openai-compat", BaseURL: "https://example.invalid/v1",
		APIKey: oldToken.AccessToken, OAuthToken: oldToken,
		Owner:  providerOwnerReferenceForRegistration(registration),
		Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	}
	require.NoError(t, store.ApplyEphemeralProviderState(map[string]ProviderConfig{"remote": forwarded}, nil))
	newToken := &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(2 * time.Hour).Unix()}
	require.NoError(t, store.applyEphemeralToken(newToken, registration.Owner()))

	require.NoError(t, store.ReloadFromDisk(context.Background()))
	actual, ok := store.Config().Providers.Get("remote")
	require.True(t, ok)
	require.Equal(t, "new-access", actual.APIKey)
	require.Equal(t, "new-refresh", actual.OAuthToken.RefreshToken)
}

func TestConfigStore_ConfigPath_GlobalAlwaysWorks(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		globalDataPath: "/some/global/crux.json",
	}

	path, err := store.configPath(ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, "/some/global/crux.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceReturnsPath(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		workspacePath: "/some/workspace/.crux/crux.json",
	}

	path, err := store.configPath(ScopeWorkspace)
	require.NoError(t, err)
	require.Equal(t, "/some/workspace/.crux/crux.json", path)
}

func TestConfigStore_ConfigPath_WorkspaceErrorsWhenEmpty(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		globalDataPath: "/some/global/crux.json",
		workspacePath:  "",
	}

	_, err := store.configPath(ScopeWorkspace)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	err := store.SetConfigField(ScopeWorkspace, "foo", "bar")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_SetConfigField_GlobalScopeAlwaysWorks(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)
	globalPath := filepath.Join(dir, "crux.json")
	require.NoError(t, os.WriteFile(globalPath, []byte("{}"), 0o600))
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	err = store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.NoError(t, err)

	data, err := os.ReadFile(globalPath)
	require.NoError(t, err)
	require.Contains(t, string(data), `"foo"`)
}

func TestConfigStore_RemoveConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	err := store.RemoveConfigField(ScopeWorkspace, "foo")
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrNoWorkspaceConfig))
}

func TestConfigStore_HasConfigField_WorkspaceScopeGuard(t *testing.T) {
	t.Parallel()

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: filepath.Join(t.TempDir(), "global.json"),
		workspacePath:  "",
	}

	has := store.HasConfigField(ScopeWorkspace, "foo")
	require.False(t, has)
}

func TestConfigStore_RuntimeOverrides_Independent(t *testing.T) {
	t.Parallel()

	store1 := &ConfigStore{config: &Config{}}
	store2 := &ConfigStore{config: &Config{}}

	require.False(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)

	store1.SetRuntimeOverrides(true, nil)

	require.True(t, store1.Overrides().SkipPermissionRequests)
	require.False(t, store2.Overrides().SkipPermissionRequests)
}

func TestConfigStore_RuntimeOverrides_ReturnsDetachedSnapshot(t *testing.T) {
	t.Parallel()

	temperature := 0.25
	store := &ConfigStore{
		config: &Config{},
		overrides: RuntimeOverrides{Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {
				Provider:    "example",
				Model:       "large",
				Temperature: &temperature,
				ProviderOptions: map[string]any{
					"nested": map[string]any{"enabled": true},
					"items":  []any{"first"},
				},
			},
		}},
	}
	store.SetRuntimeOverrides(true, []string{"one"})
	overrides := store.Overrides()
	overrides.SkipPermissionRequests = false
	overrides.EnabledChannels[0] = "changed"
	model := overrides.Models[SelectedModelTypeLarge]
	*model.Temperature = 0.75
	model.ProviderOptions["nested"].(map[string]any)["enabled"] = false
	model.ProviderOptions["items"].([]any)[0] = "changed"

	published := store.Overrides()
	require.True(t, published.SkipPermissionRequests)
	require.Equal(t, []string{"one"}, published.EnabledChannels)
	model = published.Models[SelectedModelTypeLarge]
	require.Equal(t, 0.25, *model.Temperature)
	require.Equal(t, true, model.ProviderOptions["nested"].(map[string]any)["enabled"])
	require.Equal(t, []any{"first"}, model.ProviderOptions["items"])
}

func TestConfigStore_ModelAdmissionDetachesCallerValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		apply func(*ConfigStore, SelectedModel) error
	}{
		{
			name: "persistent",
			apply: func(store *ConfigStore, model SelectedModel) error {
				return store.UpdatePreferredModel(ScopeGlobal, SelectedModelTypeLarge, model)
			},
		},
		{
			name: "runtime override",
			apply: func(store *ConfigStore, model SelectedModel) error {
				return store.OverridePreferredModel(SelectedModelTypeLarge, model)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &Config{}
			cfg.setDefaults(dir, "")
			cfg.Providers.Set("example", ProviderConfig{ID: "example", Models: []catalog.Model{{ID: "large"}}})
			store := testStoreWithPath(cfg, dir)
			temperature := 0.25
			model := SelectedModel{
				Provider:    "example",
				Model:       "large",
				Temperature: &temperature,
				ProviderOptions: map[string]any{
					"nested": map[string]any{"enabled": true},
					"items":  []any{"first"},
				},
			}

			require.NoError(t, test.apply(store, model))
			*model.Temperature = 0.75
			model.ProviderOptions["nested"].(map[string]any)["enabled"] = false
			model.ProviderOptions["items"].([]any)[0] = "changed"

			for _, published := range []SelectedModel{
				store.Config().Models[SelectedModelTypeLarge],
				store.Overrides().Models[SelectedModelTypeLarge],
			} {
				require.Equal(t, 0.25, *published.Temperature)
				require.Equal(t, true, published.ProviderOptions["nested"].(map[string]any)["enabled"])
				require.Equal(t, []any{"first"}, published.ProviderOptions["items"])
			}
		})
	}
}

func TestConfigStore_SetupAgentsPublishesNewConfig(t *testing.T) {
	t.Parallel()

	oldAgents := map[string]Agent{"existing": {ID: "existing"}}
	store := &ConfigStore{config: &Config{Options: &Options{}, Agents: oldAgents}}
	before := store.Config()

	store.SetupAgents()

	after := store.Config()
	require.NotSame(t, before, after)
	require.Equal(t, oldAgents, before.Agents)
	require.Contains(t, after.Agents, AgentCoder)
	require.Contains(t, after.Agents, AgentTask)
}

func TestGlobalWorkspaceDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", dir)

	wsDir := GlobalWorkspaceDir()
	globalData := GlobalConfigData()

	require.Equal(t, filepath.Dir(globalData), wsDir)
	require.Equal(t, dir, wsDir)
}

func TestScope_String(t *testing.T) {
	t.Parallel()

	require.Equal(t, "global", ScopeGlobal.String())
	require.Equal(t, "workspace", ScopeWorkspace.String())
	require.Contains(t, Scope(99).String(), "Scope(99)")
}

func TestConfigStaleness_CleanImmediatelyAfterSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create a config file
	content := []byte(`{"options": {"debug": true}}`)
	require.NoError(t, os.WriteFile(configPath, content, 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	result := store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileContentChange(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond) // Ensure different mtime
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_DetectsFileDeletion(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Delete the file
	require.NoError(t, os.Remove(configPath))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Contains(t, result.Missing, configPath)
}

func TestConfigStaleness_DetectsNewFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Don't create file initially
	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Now create the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	require.Contains(t, result.Changed, configPath)
	require.Empty(t, result.Missing)
}

func TestConfigStaleness_SortedOutput(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.json")
	pathB := filepath.Join(dir, "b.json")
	pathC := filepath.Join(dir, "c.json")

	// Create all files
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{}`), 0o600))
	}

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: pathA,
	}
	// Add in reverse order to test sorting
	store.captureStalenessSnapshot([]string{pathC, pathA, pathB})

	// Modify all files
	time.Sleep(10 * time.Millisecond)
	for _, p := range []string{pathA, pathB, pathC} {
		require.NoError(t, os.WriteFile(p, []byte(`{"changed": true}`), 0o600))
	}

	result := store.ConfigStaleness()
	require.True(t, result.Dirty)
	// Should be sorted alphabetically
	require.Equal(t, []string{pathA, pathB, pathC}, result.Changed)
}

func TestConfigStaleness_RefreshClearsDirtyState(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": false}`), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}
	store.captureStalenessSnapshot([]string{configPath})

	// Modify the file
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"debug": true}`), 0o600))

	// Verify dirty
	result := store.ConfigStaleness()
	require.True(t, result.Dirty)

	// Refresh snapshot
	require.NoError(t, store.RefreshStalenessSnapshot())

	// Verify clean now
	result = store.ConfigStaleness()
	require.False(t, result.Dirty)
	require.Empty(t, result.Changed)
	require.Empty(t, result.Missing)
}

// TestReloadFromDisk_UsesNewConfigValues is a regression test ensuring that
// ReloadFromDisk updates store state BEFORE running model/agent setup,
// so the new config values are used rather than stale pre-reload values.
func TestReloadFromDisk_UsesNewConfigValues(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Isolate from the host's global config so only test-provided
	// providers are visible.
	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)

	// Create initial config with one model preference
	initialConfig := `{
		"models": {
			"large": {"provider": "openai", "model": "gpt-4"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config properly
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath for the test (Load doesn't set this directly)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify initial model
	require.Equal(t, "openai", store.config.Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "gpt-4", store.config.Models[SelectedModelTypeLarge].Model)

	// Modify config on disk to change model
	updatedConfig := `{
		"models": {
			"large": {"provider": "anthropic", "model": "claude-3"}
		},
		"providers": {
			"openai": {
				"api_key": "test-key",
				"models": [{"id": "gpt-4", "name": "GPT-4"}]
			},
			"anthropic": {
				"api_key": "test-key-2",
				"models": [{"id": "claude-3", "name": "Claude 3"}]
			}
		}
	}`
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(updatedConfig), 0o600))

	// Reload from disk
	ctx := context.Background()
	err = store.ReloadFromDisk(ctx)
	require.NoError(t, err)

	// Verify the NEW config values are now in effect (regression check)
	require.Equal(t, "anthropic", store.config.Models[SelectedModelTypeLarge].Provider)
	require.Equal(t, "claude-3", store.config.Models[SelectedModelTypeLarge].Model)
}

func isolateReloadProviderScan(t *testing.T) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	resetProviderState()
	t.Cleanup(resetProviderState)
}

// TestSetConfigField_AutoReloads verifies that SetConfigField automatically
// reloads config into memory after writing, so subsequent reads see the new value.
func TestSetConfigField_AutoReloads(t *testing.T) {
	isolateReloadProviderScan(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file with debug = false
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Verify initial state
	require.False(t, store.config.Options.Debug)

	// Set globalDataPath and capture snapshot for staleness tracking
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Use SetConfigField to change debug to true
	err = store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.NoError(t, err)

	// Verify in-memory state was automatically reloaded and reflects the change
	require.True(t, store.config.Options.Debug, "Expected config to auto-reload and show debug = true")

	// Verify staleness is clean after the reload
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload")
}

// TestRemoveConfigField_AutoReloads verifies that RemoveConfigField automatically
// reloads config into memory after writing.
func TestRemoveConfigField_AutoReloads(t *testing.T) {
	isolateReloadProviderScan(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file with a custom option
	initialConfig := `{"options": {"debug": true, "custom_field": "value"}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Verify the field exists initially (indirectly - store loaded successfully)
	require.True(t, store.config.Options.Debug)

	// Remove the debug field
	err = store.RemoveConfigField(ScopeGlobal, "options.debug")
	require.NoError(t, err)

	// Verify auto-reload occurred and stale state is clean
	staleness := store.ConfigStaleness()
	require.False(t, staleness.Dirty, "Expected staleness to be clean after auto-reload from RemoveConfigField")
}

func TestSetConfigFieldFailsWithoutWorkingDirBeforeDiskWrite(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")
	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	err := store.SetConfigField(ScopeGlobal, "foo", "bar")
	require.ErrorContains(t, err, "cannot publish config fields without a working directory")
	_, statErr := os.Stat(configPath)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestGenericConfigMutationsWaitForPublicationLock(t *testing.T) {
	for _, test := range []struct {
		name    string
		initial string
		apply   func(*ConfigStore) error
		verify  func(*testing.T, *Config)
	}{
		{
			name:    "set",
			initial: `{"options":{"debug":false}}`,
			apply: func(store *ConfigStore) error {
				return store.SetConfigField(ScopeGlobal, "options.debug", true)
			},
			verify: func(t *testing.T, cfg *Config) {
				require.True(t, cfg.Options.Debug)
			},
		},
		{
			name:    "remove",
			initial: `{"options":{"debug":true}}`,
			apply: func(store *ConfigStore) error {
				return store.RemoveConfigField(ScopeGlobal, "options.debug")
			},
			verify: func(t *testing.T, cfg *Config) {
				require.False(t, cfg.Options.Debug)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolateReloadProviderScan(t)
			dir := t.TempDir()
			configPath := filepath.Join(dir, "crux.json")
			require.NoError(t, os.WriteFile(configPath, []byte(test.initial), 0o600))
			store, err := Load(dir, dir, false)
			require.NoError(t, err)
			store.globalDataPath = configPath
			store.CaptureStalenessSnapshot([]string{configPath})

			started := make(chan struct{})
			result := make(chan error, 1)
			store.writeMu.Lock()
			locked := true
			defer func() {
				if locked {
					store.writeMu.Unlock()
				}
			}()
			go func() {
				close(started)
				result <- test.apply(store)
			}()
			<-started
			select {
			case err := <-result:
				require.Failf(t, "mutation returned before publication lock was released", "error: %v", err)
			case <-time.After(25 * time.Millisecond):
			}
			store.writeMu.Unlock()
			locked = false
			require.NoError(t, <-result)
			test.verify(t, store.Config())
			require.False(t, store.ConfigStaleness().Dirty)
		})
	}
}

func TestGenericConfigMutationReportsReloadFailure(t *testing.T) {
	isolateReloadProviderScan(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options":{"debug":false}}`), 0o600))
	store, err := Load(dir, dir, false)
	require.NoError(t, err)
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})
	previous := store.Config()
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{}, errors.New("runtime publication blocked")
	})

	err = store.SetConfigField(ScopeGlobal, "options.debug", true)
	require.ErrorContains(t, err, "config file updated but failed to publish in-memory state")
	require.ErrorContains(t, err, "runtime publication blocked")
	require.Same(t, previous, store.Config())
	require.False(t, store.Config().Options.Debug)
	data, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.True(t, gjson.GetBytes(data, "options.debug").Bool())
}

func TestReloadDoesNotReenterGenericConfigMutation(t *testing.T) {
	isolateReloadProviderScan(t)

	dir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config with a provider that will trigger config modification during reload
	// (simulating the anthropic OAuth token removal case)
	initialConfig := `{
		"providers": {
			"anthropic": {
				"api_key": "test-key",
				"oauth": {"access_token": "token", "refresh_token": "refresh"}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load will trigger configureProviders which removes anthropic OAuth config.
	// This should NOT cause infinite recursion — writeMu prevents re-entrant reloads.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Capture snapshot and verify reload also works without recursion
	store.CaptureStalenessSnapshot([]string{configPath})

	// Modify file and reload — this should work without re-entrancy issues
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options": {"debug": true}}`), 0o600))

	err = store.ReloadFromDisk(context.Background())
	require.NoError(t, err)
}

// TestSetConfigFields_AutoReloadsAtomically verifies that SetConfigFields writes
// multiple fields in a single disk write and triggers only one auto-reload,
// avoiding intermediate states where only some fields are persisted.
func TestSetConfigFields_AutoReloadsAtomically(t *testing.T) {
	isolateReloadProviderScan(t)

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create initial config file.
	initialConfig := `{"options": {"debug": false}}`
	require.NoError(t, os.WriteFile(configPath, []byte(initialConfig), 0o600))

	// Load initial config.
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	// Set globalDataPath and capture snapshot.
	store.globalDataPath = configPath
	store.CaptureStalenessSnapshot([]string{configPath})

	// Write multiple fields atomically.
	err = store.SetConfigFields(ScopeGlobal, map[string]any{
		"options.debug":  true,
		"options.custom": "hello",
	})
	require.NoError(t, err)

	// Verify both fields are reflected in memory.
	require.True(t, store.config.Options.Debug)
}

func TestLoadTokenFromDisk_ReturnsNewerToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"codex": {
				"oauth": {
					"access_token": "newer-token-from-disk",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "codex")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "newer-token-from-disk", token.AccessToken)
	require.Equal(t, "refresh-abc", token.RefreshToken)
	require.Equal(t, 3600, token.ExpiresIn)
	require.Equal(t, int64(9999999999), token.ExpiresAt)
}

func TestLoadTokenFromDisk_ReturnsNilWhenSameToken(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create config file with the same token
	configContent := `{
		"providers": {
			"codex": {
				"oauth": {
					"access_token": "same-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "codex")
	require.NoError(t, err)
	require.NotNil(t, token)
	require.Equal(t, "same-token", token.AccessToken)
}

func TestLoadTokenFromDisk_ReturnsNilWhenFileMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "nonexistent.json")

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "codex")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenProviderMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create config file without the requested provider.
	configContent := `{"providers": {"openai": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "codex")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestLoadTokenFromDisk_ReturnsNilWhenOAuthMissing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create config file with provider but no OAuth token
	configContent := `{"providers": {"codex": {"api_key": "test-key"}}}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	store := &ConfigStore{
		config:         &Config{},
		globalDataPath: configPath,
	}

	token, err := store.loadTokenFromDisk(ScopeGlobal, "codex")
	require.NoError(t, err)
	require.Nil(t, token)
}

func TestRefreshOAuthToken_UsesDiskTokenWhenDifferent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "crux.json")

	// Create config file with a newer token on disk
	configContent := `{
		"providers": {
			"codex": {
				"api_key": "newer-access-token",
				"oauth": {
					"access_token": "newer-access-token",
					"refresh_token": "refresh-abc",
					"expires_in": 3600,
					"expires_at": 9999999999
				}
			}
		}
	}`
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	// Set up store with an older in-memory token
	oldToken := &oauth.Token{
		AccessToken:  "older-access-token",
		RefreshToken: "refresh-abc",
		ExpiresIn:    3600,
		ExpiresAt:    time.Now().Add(-time.Hour).Unix(), // Expired
	}

	providers := csync.NewMap[string, ProviderConfig]()
	providers.Set("codex", ProviderConfig{
		ID:         "codex",
		Name:       "Codex",
		APIKey:     oldToken.AccessToken,
		OAuthToken: oldToken,
		Owner: &ProviderOwnerReference{
			Type:         ProviderOwnerCore,
			Construction: providerregistry.ConstructionCodex,
		},
	})

	registry, registryErr := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, registryErr)
	store := &ConfigStore{
		config: &Config{
			Providers: providers,
		},
		globalDataPath:   configPath,
		providerRegistry: registry,
	}

	// Refresh should use the disk token without making an external call
	owner := refreshTestOwner(t, store)
	err := refreshOAuthTokenForTest(context.Background(), store, ScopeGlobal, owner)
	require.NoError(t, err)

	// Verify the in-memory token was updated to the disk token
	updatedConfig, ok := store.config.Providers.Get("codex")
	require.True(t, ok)
	require.Equal(t, "newer-access-token", updatedConfig.APIKey)
	require.Equal(t, "newer-access-token", updatedConfig.OAuthToken.AccessToken)
	require.Equal(t, "refresh-abc", updatedConfig.OAuthToken.RefreshToken)
}

// TestConfigStore_SetConfigFields_concurrentInProcess verifies that
// concurrent in-process writes do not lose data when serialized by the
// s.mu mutex. This does not exercise the cross-process flock; testing
// that would require spawning a separate OS process.
func TestConfigStore_SetConfigFields_concurrentInProcess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", dir)
	t.Setenv("CRUX_GLOBAL_DATA", dir)
	resetProviderState()
	t.Cleanup(resetProviderState)
	configPath := filepath.Join(dir, "crux.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(configPath), 0o755))
	require.NoError(t, os.WriteFile(configPath, []byte("{}"), 0o600))
	store, err := Load(dir, dir, false)
	require.NoError(t, err)

	const (
		numGoroutines    = 20
		fieldsPerRoutine = 5
	)

	errs := make(chan error, numGoroutines)
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			kv := make(map[string]any, fieldsPerRoutine)
			for j := 0; j < fieldsPerRoutine; j++ {
				key := fmt.Sprintf("goroutine_%d_field_%d", id, j)
				kv[key] = fmt.Sprintf("value_%d_%d", id, j)
			}
			errs <- store.SetConfigFields(ScopeGlobal, kv)
		}(i)
	}

	for i := 0; i < numGoroutines; i++ {
		require.NoError(t, <-errs)
	}

	// Verify all fields are present in the config file.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)

	for i := 0; i < numGoroutines; i++ {
		for j := 0; j < fieldsPerRoutine; j++ {
			key := fmt.Sprintf("goroutine_%d_field_%d", i, j)
			expectedValue := fmt.Sprintf("value_%d_%d", i, j)
			result := gjson.Get(string(data), key)
			require.True(t, result.Exists(), "key %s should exist", key)
			require.Equal(t, expectedValue, result.String(), "key %s should have the correct value", key)
		}
	}
}
