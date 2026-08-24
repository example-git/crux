package config

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderSurfacesPreserveCustomProviders(t *testing.T) {
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"custom": {ID: "custom", Name: "Custom Provider", Models: []catwalk.Model{{ID: "custom-model"}}},
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
				Models: []catwalk.Model{{ID: "model-1", DefaultMaxTokens: 1024}},
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

func TestProviderSurfacesTreatUnmarkedOAuthProviderAsUnavailable(t *testing.T) {
	resetProviderState()
	defer resetProviderState()
	cfg := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"legacy-oauth": {
				ID: "legacy-oauth", Name: "Legacy OAuth", OAuthToken: &oauth.Token{AccessToken: "secret"},
				Type: catwalk.TypeOpenAI, Models: []catwalk.Model{{ID: "legacy-model"}},
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

func lookupTestSurface(surfaces []providerregistry.Surface, id string) (providerregistry.Surface, bool) {
	return providerregistry.LookupSurface(surfaces, id)
}
