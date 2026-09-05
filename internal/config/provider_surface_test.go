package config

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderSurfacesPreserveCustomProviders(t *testing.T) {
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {ID: "custom", Name: "Custom Provider", Models: []catalog.Model{{ID: "custom-model"}}},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "custom", Model: "custom-model"},
		},
	}

	surfaces := ProviderSurfaces(cfg)
	surface, ok := lookupTestSurface(surfaces, "custom")
	require.True(t, ok)
	require.Equal(t, "Custom Provider", surface.Name)
	require.Equal(t, "custom-model", surface.Models[0].ID)
	require.Empty(t, surface.Authentication)
	require.Nil(t, surface.Configuration)
}

func TestProviderSurfacesExposeUnavailablePluginWithoutLosingSelection(t *testing.T) {
	resetProviderState()
	defer resetProviderState()
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"synthetic": {
				ID: "synthetic", Name: "Synthetic", Plugin: &ProviderPluginReference{ID: "synthetic.plugin", Version: "1.0.0"},
				Models: []catalog.Model{{ID: "model-1", DefaultMaxTokens: 1024}},
			},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "synthetic", Model: "model-1"},
			SelectedModelTypeSmall: {Provider: "synthetic", Model: "model-1"},
		},
	}

	surfaces := ProviderSurfaces(cfg)
	surface, ok := lookupTestSurface(surfaces, "synthetic")
	require.True(t, ok)
	require.False(t, surface.Available)
	require.Equal(t, "missing", surface.Availability)
	require.Contains(t, surface.Diagnostic, "not installed")
	require.False(t, cfg.IsModelAvailable("synthetic", "model-1"))

	resolved, err := resolveSelectedModels(cfg, nil)
	require.NoError(t, err)
	require.Equal(t, cfg.Models[SelectedModelTypeLarge], resolved.Large)
	require.Equal(t, cfg.Models[SelectedModelTypeSmall], resolved.Small)
	require.False(t, resolved.LargeFallback)
	require.False(t, resolved.SmallFallback)
}

func TestProviderSurfacesExposeUnavailablePresetDespiteSameIDCatalog(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	providerOnce.Do(func() {})
	providerList = []catalog.Provider{{ID: "synthetic", Name: "Other Owner", Models: []catalog.Model{{ID: "other-model"}}}}
	var err error
	providerRegistry, err = providerregistry.New()
	require.NoError(t, err)
	providerPresetReferences = map[string]ProviderPresetReference{}

	cfg := &Config{
		Options: &Options{},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"synthetic": {
				ID: "synthetic", Name: "Retained", Preset: &ProviderPresetReference{ID: "expected.preset", Version: "1"},
				Models: []catalog.Model{{ID: "retained-model"}},
			},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "synthetic", Model: "retained-model"},
		},
	}
	cfg.bindProviderScan(ProviderScan{
		Providers:        providerList,
		Registry:         providerRegistry,
		presetReferences: providerPresetReferences,
	})

	surface, ok := lookupTestSurface(ProviderSurfaces(cfg), "synthetic")
	require.True(t, ok)
	require.False(t, surface.Available)
	require.Equal(t, "missing", surface.Availability)
	require.Contains(t, surface.Diagnostic, "expected.preset")
	require.False(t, cfg.IsModelAvailable("synthetic", "retained-model"))
}

func TestProviderSurfacesDoNotExposeMismatchedSameIDRegistration(t *testing.T) {
	providerID := "retained"
	registration := providerregistry.Registration{
		ProviderID: providerID,
		Name:       "Mismatched Owner",
		Manifest: &manifest.Manifest{
			ID:      "other.plugin",
			Version: "2.0.0",
			Provider: manifest.Provider{
				ID:          providerID,
				Name:        "Mismatched Owner",
				Description: "must not leak",
			},
			Configuration: manifest.Configuration{
				Schema: map[string]any{"type": "object"},
				Fields: map[string]manifest.FieldDisplay{"region": {}},
			},
		},
		Brand: &providerregistry.Brand{Label: "Must Not Leak"},
		OAuth: &providerregistry.OAuthCapability{FlowID: "login"},
	}
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	cfg := &Config{
		Options: &Options{},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			providerID: {
				ID: providerID, Name: "Retained Provider", FlatRate: true,
				Plugin: &ProviderPluginReference{ID: "expected.plugin", Version: "1.0.0"},
				Models: []catalog.Model{{ID: "retained-model"}},
			},
		}),
	}
	cfg.bindProviderScan(ProviderScan{
		Providers: []catalog.Provider{{ID: catalog.ProviderID(providerID), Name: "Mismatched Owner", Models: []catalog.Model{{ID: "other-model"}}}},
		Registry:  registry,
	})

	surface, ok := lookupTestSurface(ProviderSurfaces(cfg), providerID)
	require.True(t, ok)
	require.False(t, surface.Available)
	require.Equal(t, "Retained Provider", surface.Name)
	require.True(t, surface.FlatRate)
	require.Equal(t, []catalog.Model{{ID: "retained-model"}}, surface.Models)
	require.Empty(t, surface.Description)
	require.Nil(t, surface.Brand)
	require.Empty(t, surface.Authentication)
	require.Nil(t, surface.Configuration)
	require.Nil(t, surface.ConfigurationUI)
	require.Nil(t, surface.Images)
	require.Nil(t, surface.Instructions)
	require.Empty(t, surface.RuntimeControls)
	require.False(t, surface.UsageAvailable)
}

func TestConfiguredProviderOwnershipRejectsSameIDMasking(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	providerID := "custom-owned"
	preset := ProviderPresetReference{ID: "preset.one", Version: "1.0.0", Digest: "digest-one"}
	providerPresetReferences = map[string]ProviderPresetReference{providerID: preset}
	pluginRegistration := providerregistry.Registration{
		ProviderID:   providerID,
		Construction: providerregistry.ConstructionGenericJSON,
		Manifest:     &manifest.Manifest{ID: "plugin.one", Version: "1.0.0"},
	}
	pluginRegistry, err := providerregistry.New(pluginRegistration)
	require.NoError(t, err)

	exactPlugin := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			Owner:  providerOwnerReferenceForRegistration(pluginRegistration),
			Plugin: &ProviderPluginReference{ID: "plugin.one", Version: "1.0.0"},
		},
	})}
	registration, active := exactPlugin.providerRegistration(pluginRegistry, providerID)
	require.True(t, active)
	require.Equal(t, "plugin.one", registration.Manifest.ID)
	_, active = exactPlugin.providerPreset(pluginRegistry, providerID)
	require.False(t, active)

	mismatchedPlugin := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			Owner:  providerOwnerReferenceForRegistration(pluginRegistration),
			Plugin: &ProviderPluginReference{ID: "plugin.two", Version: "1.0.0"},
		},
	})}
	_, active = mismatchedPlugin.providerRegistration(pluginRegistry, providerID)
	require.False(t, active)
	_, active = mismatchedPlugin.providerPreset(pluginRegistry, providerID)
	require.False(t, active)

	mismatchedPluginVersion := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			Owner:  providerOwnerReferenceForRegistration(pluginRegistration),
			Plugin: &ProviderPluginReference{ID: "plugin.one", Version: "2.0.0"},
		},
	})}
	_, active = mismatchedPluginVersion.providerRegistration(pluginRegistry, providerID)
	require.False(t, active)

	legacyPlugin := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {Plugin: &ProviderPluginReference{ID: "plugin.one"}},
	})}
	_, active = legacyPlugin.providerRegistration(pluginRegistry, providerID)
	require.False(t, active)

	exactPreset := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {Owner: providerPresetOwnerReference(), Preset: &preset},
	})}
	for _, test := range []struct {
		name         string
		registration providerregistry.Registration
	}{
		{name: "plugin selected", registration: pluginRegistration},
		{name: "core selected", registration: providerregistry.Registration{ProviderID: providerID, Construction: providerregistry.ConstructionGenericJSON}},
	} {
		t.Run(test.name, func(t *testing.T) {
			registry, err := providerregistry.New(test.registration)
			require.NoError(t, err)
			exactPreset.bindProviderScan(ProviderScan{
				Registry:         registry,
				presetReferences: map[string]ProviderPresetReference{providerID: preset},
			})
			_, active := exactPreset.providerPreset(registry, providerID)
			require.False(t, active)
			require.True(t, exactPreset.isUnavailableRegisteredProvider(providerID))
			require.False(t, exactPreset.IsProviderIntegrationAvailable(providerID))
		})
	}

	emptyRegistry, err := providerregistry.New()
	require.NoError(t, err)
	exactPreset.bindProviderScan(ProviderScan{
		Registry:         emptyRegistry,
		presetReferences: map[string]ProviderPresetReference{providerID: preset},
	})
	resolvedPreset, active := exactPreset.providerPreset(emptyRegistry, providerID)
	require.True(t, active)
	require.Equal(t, preset, resolvedPreset)
	require.False(t, exactPreset.isUnavailableRegisteredProvider(providerID))
	require.True(t, exactPreset.IsProviderIntegrationAvailable(providerID))

	mismatchedPresetVersion := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {Owner: providerPresetOwnerReference(), Preset: &ProviderPresetReference{ID: preset.ID, Version: "2.0.0", Digest: preset.Digest}},
	})}
	_, active = mismatchedPresetVersion.providerPreset(emptyRegistry, providerID)
	require.False(t, active)

	legacyPreset := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {Owner: providerPresetOwnerReference(), Preset: &ProviderPresetReference{ID: preset.ID}},
	})}
	_, active = legacyPreset.providerPreset(emptyRegistry, providerID)
	require.False(t, active)
}

func TestConfiguredProviderRequiresFullOwnerTuple(t *testing.T) {
	plugin := providerregistry.Registration{
		ProviderID:           "owned",
		Construction:         providerregistry.ConstructionCodex,
		CompatibilityAdapter: providerregistry.ConstructionCodex,
		Manifest:             &manifest.Manifest{ID: "plugin.owned", Version: "1.0.0"},
	}
	pluginRegistry, err := providerregistry.New(plugin)
	require.NoError(t, err)
	pluginConfig := ProviderConfig{
		Owner: &ProviderOwnerReference{
			Type:                 ProviderOwnerPlugin,
			Construction:         plugin.Construction,
			CompatibilityAdapter: plugin.CompatibilityAdapter,
		},
		Plugin: &ProviderPluginReference{ID: plugin.Manifest.ID, Version: plugin.Manifest.Version},
	}

	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{"owned": pluginConfig})}
	registration, active := cfg.providerRegistration(pluginRegistry, "owned")
	require.True(t, active)
	require.Equal(t, plugin.Owner(), registration.Owner())

	wrongConstruction := pluginConfig
	wrongConstruction.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: providerregistry.ConstructionGenericJSON, CompatibilityAdapter: plugin.CompatibilityAdapter}
	cfg.Providers.Set("owned", wrongConstruction)
	_, active = cfg.providerRegistration(pluginRegistry, "owned")
	require.False(t, active)

	wrongAdapter := pluginConfig
	wrongAdapter.Owner = &ProviderOwnerReference{Type: ProviderOwnerPlugin, Construction: plugin.Construction, CompatibilityAdapter: providerregistry.ConstructionGeminiAntigravity}
	cfg.Providers.Set("owned", wrongAdapter)
	_, active = cfg.providerRegistration(pluginRegistry, "owned")
	require.False(t, active)

	custom := pluginConfig
	custom.Owner = &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}
	custom.Plugin = nil
	cfg.Providers.Set("owned", custom)
	_, active = cfg.providerRegistration(pluginRegistry, "owned")
	require.False(t, active)

	coreProviderID := string(catalog.ProviderCopilot)
	core := providerregistry.Registration{ProviderID: coreProviderID, Construction: providerregistry.ConstructionCopilot}
	coreRegistry, err := providerregistry.New(core)
	require.NoError(t, err)
	cfg.Providers.Set(coreProviderID, ProviderConfig{Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: core.Construction}})
	_, active = cfg.providerRegistration(coreRegistry, coreProviderID)
	require.True(t, active)
	cfg.Providers.Set(coreProviderID, ProviderConfig{Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity}})
	_, active = cfg.providerRegistration(coreRegistry, coreProviderID)
	require.False(t, active)
}

func TestConfiguredPresetRequiresPersistedDigest(t *testing.T) {
	providerID := "preset-owned"
	active := ProviderPresetReference{ID: "preset.one", Version: "1.0.0", Digest: "digest-one"}
	registry, err := providerregistry.New()
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			Owner:  &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
			Preset: &ProviderPresetReference{ID: active.ID, Version: active.Version, Digest: "digest-other"},
		},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: registry, presetReferences: map[string]ProviderPresetReference{providerID: active}})

	_, available := cfg.ProviderPreset(providerID)
	require.False(t, available)
	provider, found := cfg.Providers.Get(providerID)
	require.True(t, found)
	provider.Preset.Digest = active.Digest
	cfg.Providers.Set(providerID, provider)
	resolved, available := cfg.ProviderPreset(providerID)
	require.True(t, available)
	require.Equal(t, active, resolved)
}

func TestMigratedPresetRequiresCanonicalPersistedVersion(t *testing.T) {
	providerID := "deepseek"
	presetID, version, digest, migrated := providerplugin.CanonicalMigratedProviderPreset(providerID)
	require.True(t, migrated)
	active := ProviderPresetReference{ID: presetID, Version: version, Digest: digest}
	registry, err := providerregistry.New()
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {Owner: providerPresetOwnerReference(), Preset: &ProviderPresetReference{ID: active.ID}},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: registry, presetReferences: map[string]ProviderPresetReference{providerID: active}})

	_, available := cfg.ProviderPreset(providerID)
	require.False(t, available)
	require.True(t, cfg.isUnavailableRegisteredProvider(providerID))

	provider, found := cfg.Providers.Get(providerID)
	require.True(t, found)
	provider.Preset.Version = active.Version
	cfg.Providers.Set(providerID, provider)
	_, available = cfg.ProviderPreset(providerID)
	require.False(t, available)
	provider.Preset.Digest = active.Digest
	cfg.Providers.Set(providerID, provider)
	resolved, available := cfg.ProviderPreset(providerID)
	require.True(t, available)
	require.Equal(t, active, resolved)
}

func TestProviderSurfacesExposePluginOAuthBindingErrors(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	providerOnce.Do(func() {})
	providerID := "synthetic"
	pluginID := "synthetic.plugin"
	value := manifest.Manifest{
		ID:       pluginID,
		Version:  "1.0.0",
		Provider: manifest.Provider{ID: providerID, Name: "Synthetic"},
		Capabilities: manifest.Capabilities{
			OAuth: []manifest.OAuthFlow{{
				ID: "login", AuthorizationEndpoint: "authorize", TokenEndpoint: "token",
				ClientID: manifest.Template{Kind: "config", Ref: "oauth_client_id"},
			}},
			Endpoints: []manifest.Endpoint{
				{ID: "authorize", BaseURL: "https://example.invalid/authorize"},
				{ID: "token", BaseURL: "https://example.invalid/token"},
			},
		},
	}
	registration := providerregistry.Registration{
		ProviderID: providerID,
		Manifest:   &value,
		OAuth:      &providerregistry.OAuthCapability{FlowID: "login"},
	}
	var err error
	providerRegistry, err = providerregistry.New(registration)
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		providerID: {
			ID: providerID, Name: "Synthetic", Plugin: &ProviderPluginReference{ID: pluginID, Version: "1.0.0"},
			Models: []catalog.Model{{ID: "model-1"}},
		},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: providerRegistry})

	require.ErrorContains(t, cfg.ProviderRegistrationError(providerID), `configuration property "oauth_client_id"`)
	surface, ok := lookupTestSurface(ProviderSurfaces(cfg), providerID)
	require.True(t, ok)
	require.False(t, surface.Available)
	require.Equal(t, "invalid", surface.Availability)
	require.Contains(t, surface.Diagnostic, `configuration property "oauth_client_id"`)
}

func TestProviderSurfacesTreatUnmarkedOAuthProviderAsUnavailable(t *testing.T) {
	resetProviderState()
	defer resetProviderState()
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"legacy-oauth": {
				ID: "legacy-oauth", Name: "Legacy OAuth", OAuthToken: &oauth.Token{AccessToken: "secret"},
				Type: catalog.TypeOpenAI, Models: []catalog.Model{{ID: "legacy-model"}},
			},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "legacy-oauth", Model: "legacy-model"},
			SelectedModelTypeSmall: {Provider: "legacy-oauth", Model: "legacy-model"},
		},
	}

	surface, ok := lookupTestSurface(ProviderSurfaces(cfg), "legacy-oauth")
	require.True(t, ok)
	require.False(t, surface.Available)
	require.Equal(t, "missing", surface.Availability)
	require.Contains(t, surface.Diagnostic, "OAuth provider integration is not active")
	resolved, err := resolveSelectedModels(cfg, nil)
	require.NoError(t, err)
	require.Equal(t, "legacy-oauth", resolved.Large.Provider)
	require.Equal(t, "legacy-model", resolved.Large.Model)
	require.False(t, resolved.LargeFallback)
}

func TestRedactedForTransportRemovesProviderSecrets(t *testing.T) {
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"custom": {
			ID: "custom", APIKey: "api-secret", APIKeyTemplate: "$SECRET",
			OAuthToken:    &oauth.Token{AccessToken: "access-secret", RefreshToken: "refresh-secret"},
			ExtraHeaders:  map[string]string{"Authorization": "secret"},
			Configuration: map[string]any{"region": "local"},
		},
	})}

	redacted := cfg.RedactedForTransport()
	provider, ok := redacted.Providers.Get("custom")
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
	require.Empty(t, provider.APIKeyTemplate)
	require.Nil(t, provider.OAuthToken)
	require.Nil(t, provider.ExtraHeaders)
	require.Equal(t, "local", provider.Configuration["region"])

	original, ok := cfg.Providers.Get("custom")
	require.True(t, ok)
	require.Equal(t, "api-secret", original.APIKey)
	require.Equal(t, "access-secret", original.OAuthToken.AccessToken)
	require.Equal(t, "secret", original.ExtraHeaders["Authorization"])
}

func TestRedactedForTransportUsesExactPluginOwnership(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	var err error
	providerRegistry, err = providerregistry.New(providerregistry.Registration{
		ProviderID: "synthetic",
		Manifest: &manifest.Manifest{
			ID:      "plugin.one",
			Version: "1.0.0",
			Configuration: manifest.Configuration{Fields: map[string]manifest.FieldDisplay{
				"secret": {Secret: true},
				"public": {Secret: false},
			}},
		},
	})
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"synthetic": {
			ID: "synthetic",
			Plugin: &ProviderPluginReference{
				ID:      "plugin.one",
				Version: "1.0.0",
			},
			Configuration: map[string]any{
				"secret": "private",
				"public": "visible",
			},
		},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: providerRegistry})

	redacted := cfg.RedactedForTransport()
	provider, ok := redacted.Providers.Get("synthetic")
	require.True(t, ok)
	require.Equal(t, map[string]any{"public": "visible"}, provider.Configuration)
	original, ok := cfg.Providers.Get("synthetic")
	require.True(t, ok)
	require.Equal(t, "private", original.Configuration["secret"])
}

func TestRedactedForTransportHidesUnavailablePluginConfiguration(t *testing.T) {
	for _, test := range []struct {
		name          string
		registrations []providerregistry.Registration
	}{
		{name: "missing"},
		{name: "mismatched", registrations: []providerregistry.Registration{{
			ProviderID: "synthetic",
			Manifest:   &manifest.Manifest{ID: "plugin.two", Version: "1.0.0"},
		}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			resetProviderState()
			t.Cleanup(resetProviderState)
			var err error
			providerRegistry, err = providerregistry.New(test.registrations...)
			require.NoError(t, err)
			cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
				"synthetic": {
					ID:            "synthetic",
					Plugin:        &ProviderPluginReference{ID: "plugin.one", Version: "1.0.0"},
					Configuration: map[string]any{"unknown": "private"},
				},
			})}
			cfg.bindProviderScan(ProviderScan{Registry: providerRegistry})

			redacted := cfg.RedactedForTransport()
			provider, ok := redacted.Providers.Get("synthetic")
			require.True(t, ok)
			require.Nil(t, provider.Configuration)
			original, ok := cfg.Providers.Get("synthetic")
			require.True(t, ok)
			require.Equal(t, "private", original.Configuration["unknown"])
		})
	}
}

func TestRedactedForTransportHidesPresetRuntimeMaterialAndPreservesPublicOwner(t *testing.T) {
	owner := providerPresetOwnerReference()
	preset := &ProviderPresetReference{ID: "preset.one", Version: "1.0.0", Digest: "sha256:public-owner"}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"synthetic": {
			ID:                  "synthetic",
			Name:                "Synthetic",
			BaseURL:             "https://private.invalid/v1",
			Type:                catalog.TypeOpenAICompat,
			Owner:               owner,
			Preset:              preset,
			SystemPromptPrefix:  "private prompt",
			ToolingInstructions: "native",
			ExtraBody:           map[string]any{"private": "body"},
			ProviderOptions:     map[string]any{"private": "option"},
			Configuration:       map[string]any{"private": "configuration"},
			ExtraParams:         map[string]string{"private": "parameter"},
			FlatRate:            true,
			Models:              []catalog.Model{{ID: "model-one", Name: "Model One"}},
		},
	})}

	redacted := cfg.RedactedForTransport()
	provider, ok := redacted.Providers.Get("synthetic")
	require.True(t, ok)
	require.Empty(t, provider.BaseURL)
	require.Empty(t, provider.SystemPromptPrefix)
	require.Nil(t, provider.ExtraBody)
	require.Nil(t, provider.ProviderOptions)
	require.Nil(t, provider.Configuration)
	require.Nil(t, provider.ExtraParams)
	require.Equal(t, "Synthetic", provider.Name)
	require.Equal(t, catalog.TypeOpenAICompat, provider.Type)
	require.Equal(t, "native", provider.ToolingInstructions)
	require.True(t, provider.FlatRate)
	require.Equal(t, []catalog.Model{{ID: "model-one", Name: "Model One"}}, provider.Models)
	require.Equal(t, owner, provider.Owner)
	require.Equal(t, preset, provider.Preset)

	original, ok := cfg.Providers.Get("synthetic")
	require.True(t, ok)
	require.Equal(t, "https://private.invalid/v1", original.BaseURL)
	require.Equal(t, "private prompt", original.SystemPromptPrefix)
	require.Equal(t, "body", original.ExtraBody["private"])
	require.Equal(t, "configuration", original.Configuration["private"])
}

func TestRedactedForTransportDoesNotApplyPluginPolicyToPreset(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	var err error
	providerRegistry, err = providerregistry.New(providerregistry.Registration{
		ProviderID: "synthetic",
		Manifest: &manifest.Manifest{
			ID:      "plugin.one",
			Version: "1.0.0",
			Configuration: manifest.Configuration{Fields: map[string]manifest.FieldDisplay{
				"shared": {Secret: true},
			}},
		},
	})
	require.NoError(t, err)
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"synthetic": {
			ID:            "synthetic",
			Preset:        &ProviderPresetReference{ID: "preset.one", Version: "1.0.0"},
			Configuration: map[string]any{"shared": "preset-value"},
		},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: providerRegistry})

	redacted := cfg.RedactedForTransport()
	provider, ok := redacted.Providers.Get("synthetic")
	require.True(t, ok)
	require.Nil(t, provider.Configuration)
}

func TestProviderConfigTestConnectionRequiresExactPresetOwner(t *testing.T) {
	resolver := NewShellVariableResolver(env.NewFromMap(nil))
	ctx := context.Background()
	validOwner := func() error { return nil }

	providerID := string(catalog.ProviderMiniMax)
	exact := ProviderConfig{
		ID:     providerID,
		Owner:  providerPresetOwnerReference(),
		Preset: &ProviderPresetReference{ID: "preset.minimax", Version: "1.0.0", Digest: "digest-minimax"},
	}
	require.ErrorContains(t, exact.TestConnection(ctx, resolver, nil), "validator is unavailable")
	require.NoError(t, exact.TestConnection(ctx, resolver, validOwner))

	incomplete := exact
	incomplete.Preset = &ProviderPresetReference{ID: exact.Preset.ID, Version: exact.Preset.Version}
	require.ErrorContains(t, incomplete.TestConnection(ctx, resolver, validOwner), "incomplete owner reference")

	var successfulRequests atomic.Int32
	successServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		successfulRequests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(successServer.Close)
	custom := ProviderConfig{
		ID:      "example-echo",
		BaseURL: successServer.URL,
		Type:    catalog.TypeOpenAICompat,
		Owner: &ProviderOwnerReference{
			Type:         ProviderOwnerCustom,
			Construction: providerregistry.ConstructionOpenAICompat,
		},
	}
	require.NoError(t, custom.TestConnection(ctx, resolver, validOwner))
	require.EqualValues(t, 1, successfulRequests.Load())

	var validationCalls atomic.Int32
	replacedBeforeDispatch := func() error {
		if validationCalls.Add(1) > 1 {
			return fmt.Errorf("provider owner changed before dispatch")
		}
		return nil
	}
	require.ErrorContains(t, custom.TestConnection(ctx, resolver, replacedBeforeDispatch), "changed before dispatch")
	require.EqualValues(t, 1, successfulRequests.Load())

	var ownerCurrent atomic.Bool
	ownerCurrent.Store(true)
	var initialRequests atomic.Int32
	var redirectedRequests atomic.Int32
	redirectServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/models":
			initialRequests.Add(1)
			ownerCurrent.Store(false)
			http.Redirect(writer, request, "/redirected", http.StatusFound)
		case "/redirected":
			redirectedRequests.Add(1)
			writer.WriteHeader(http.StatusOK)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(redirectServer.Close)
	redirected := custom
	redirected.BaseURL = redirectServer.URL
	validateRedirectOwner := func() error {
		if !ownerCurrent.Load() {
			return fmt.Errorf("provider owner changed during redirect")
		}
		return nil
	}
	require.ErrorContains(t, redirected.TestConnection(ctx, resolver, validateRedirectOwner), "changed during redirect")
	require.EqualValues(t, 1, initialRequests.Load())
	require.Zero(t, redirectedRequests.Load())
}

func lookupTestSurface(surfaces []providerregistry.Surface, id string) (providerregistry.Surface, bool) {
	return providerregistry.LookupSurface(surfaces, id)
}
