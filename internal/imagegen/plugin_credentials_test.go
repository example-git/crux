package imagegen

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestPluginCredentialBridgeRequiresExactProviderBinding(t *testing.T) {
	store := config.NewTestStore(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"fixture-image-custom": {ID: "fixture-image-custom", APIKey: "synthetic-access", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}},
	})})
	provider, ok := store.Config().Providers.Get("fixture-image-custom")
	require.True(t, ok)
	owner, ok := store.RuntimeSnapshot().ProviderOwnerFor("fixture-image-custom", provider)
	require.True(t, ok)
	bundle := providerplugin.RegisteredImageBundle{Manifest: manifest.ImageManifest{Credentials: []manifest.ImageCredential{{ID: "access", Source: "provider", Provider: "fixture-image-custom"}}}}
	_, err := ResolvePluginCredentials(t.Context(), store, bundle, PluginCredentialBindings{})
	require.ErrorContains(t, err, "explicit exact owner")
	wrong := owner
	wrong.ManifestID = "different-owner"
	_, err = ResolvePluginCredentials(t.Context(), store, bundle, PluginCredentialBindings{Providers: map[string]providerregistry.RegistrationOwner{"access": wrong}})
	require.Error(t, err)
	credentials, err := ResolvePluginCredentials(t.Context(), store, bundle, PluginCredentialBindings{Providers: map[string]providerregistry.RegistrationOwner{"access": owner}})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"api_key": "synthetic-access", "access_token": "", "base_url": "", "account": map[string]any{}}, credentials.Values["access"])
	require.NoError(t, credentials.Validate())
	require.NotEmpty(t, credentials.Identity)
	provider.BaseURL = "https://replacement.example/v1"
	store.Config().Providers.Set(provider.ID, provider)
	require.ErrorContains(t, credentials.Validate(), "changed during execution")
}

func TestPluginCredentialBridgeRequiresExplicitBrowserSelection(t *testing.T) {
	store := config.NewTestStore(&config.Config{})
	bundle := providerplugin.RegisteredImageBundle{Manifest: manifest.ImageManifest{Credentials: []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{"provider.example"}}}}}
	_, err := ResolvePluginCredentials(t.Context(), store, bundle, PluginCredentialBindings{})
	require.ErrorContains(t, err, "process-local selection")
	jar, err := cookiejar.New(nil)
	require.NoError(t, err)
	credentials, err := ResolvePluginCredentials(t.Context(), store, bundle, PluginCredentialBindings{Browser: func(ctx context.Context, declaration manifest.ImageCredential) (http.CookieJar, string, error) {
		require.Equal(t, []string{"provider.example"}, declaration.Domains)
		return jar, "synthetic-profile-generation", nil
	}})
	require.NoError(t, err)
	require.Same(t, jar, credentials.CookieJars["browser"])
	require.Empty(t, credentials.Values)
	require.NotEmpty(t, credentials.Identity)
}
