package config

import (
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestRegisterConfigSecretsCoversPresetRuntimeAndNestedPluginValues(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	pluginSecret := "config-plugin-nested-secret-value"
	pluginPublic := "config-plugin-public-value"
	registration := providerregistry.Registration{
		ProviderID:   "plugin-provider",
		Construction: providerregistry.ConstructionOpenAICompat,
		Manifest: &manifest.Manifest{
			ID:      "plugin.owner",
			Version: "1.0.0",
			Configuration: manifest.Configuration{Fields: map[string]manifest.FieldDisplay{
				"private": {Secret: true},
				"public":  {Secret: false},
			}},
		},
	}
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	presetSecrets := []string{
		"config-preset-endpoint-value",
		"config-preset-prompt-value",
		"config-preset-header-value",
		"config-preset-body-value",
		"config-preset-option-value",
		"config-preset-configuration-value",
		"config-preset-param-value",
		"config-preset-api-key-template-value",
	}
	cfg := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
		"preset-provider": {
			ID:                 "preset-provider",
			Owner:              providerPresetOwnerReference(),
			Preset:             &ProviderPresetReference{ID: "preset.owner", Version: "1.0.0", Digest: "preset-public-digest"},
			BaseURL:            presetSecrets[0],
			SystemPromptPrefix: presetSecrets[1],
			ExtraHeaders:       map[string]string{"X-Ordinary": presetSecrets[2]},
			ExtraBody:          map[string]any{"nested": []any{presetSecrets[3]}},
			ProviderOptions:    map[string]any{"nested": presetSecrets[4]},
			Configuration:      map[string]any{"nested": map[string]any{"value": presetSecrets[5]}},
			ExtraParams:        map[string]string{"private": presetSecrets[6]},
			APIKeyTemplate:     presetSecrets[7],
		},
		"plugin-provider": {
			ID:            "plugin-provider",
			Owner:         providerOwnerReferenceForRegistration(registration),
			Plugin:        &ProviderPluginReference{ID: "plugin.owner", Version: "1.0.0"},
			Configuration: map[string]any{"private": map[string]any{"nested": []any{pluginSecret}}, "public": pluginPublic},
		},
	})}
	cfg.bindProviderScan(ProviderScan{Registry: registry})

	registerConfigSecrets(cfg)
	for _, secret := range append(presetSecrets, pluginSecret) {
		require.Equal(t, redact.Replacement, redact.String(secret))
	}
	require.Equal(t, pluginPublic, redact.String(pluginPublic))
	require.Equal(t, "preset-public-digest", redact.String("preset-public-digest"))
}

func TestRegisterAccountSecretsCoversOpaqueRawValues(t *testing.T) {
	secret := "config-forwarded-account-raw-secret-value"
	registerAccountSecrets(accounts.Entry{Raw: []byte(`{"metadata":{"tenant":"` + secret + `"}}`)})
	require.Equal(t, redact.Replacement, redact.String(secret))
}

func TestRegisterConfigSecretsCoversProvidersAndMCP(t *testing.T) {
	providerKey := "config-provider-key-value"
	providerRefresh := "config-provider-refresh-value"
	providerClient := "config-provider-oauth-client-secret-value"
	mcpSecret := "config-mcp-client-secret-value"
	mcpToken := "config-mcp-access-token-value"
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"test": {APIKey: providerKey, OAuthToken: &oauth.Token{RefreshToken: providerRefresh, Client: &oauth.OAuthClient{ClientSecret: providerClient}}},
		}),
		MCP: MCPs{"test": {OAuthClientSecret: mcpSecret, OAuthToken: &oauth.Token{AccessToken: mcpToken}}},
	}
	registerConfigSecrets(cfg)
	for _, secret := range []string{providerKey, providerRefresh, providerClient, mcpSecret, mcpToken} {
		require.Equal(t, redact.Replacement, redact.String(secret))
	}
}

func TestRegisterConfigSecretsCoversPrivateAuthenticationCandidates(t *testing.T) {
	registration := providerregistry.Registration{
		ProviderID: "candidate-provider", Construction: providerregistry.ConstructionOpenAICompat,
		Manifest: &manifest.Manifest{ID: "plugin.owner", Version: "1.0.0", Configuration: manifest.Configuration{Fields: map[string]manifest.FieldDisplay{"private": {Secret: true}, "public": {}}}},
	}
	registry, err := providerregistry.New(registration)
	require.NoError(t, err)
	secret, header, public := "private-candidate-declared-secret", "private-candidate-auth-header", "private-candidate-public-value"
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), authenticationCandidates: map[string]ProviderConfig{
		registration.ProviderID: {ID: registration.ProviderID, Owner: providerOwnerReferenceForRegistration(registration), Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}, ExtraHeaders: map[string]string{"Authorization": header}, Configuration: map[string]any{"private": map[string]any{"nested": secret}, "public": public}},
	}}
	cfg.bindProviderScan(ProviderScan{Registry: registry})
	registerConfigSecrets(cfg)
	require.Equal(t, redact.Replacement, redact.String(secret))
	require.Equal(t, redact.Replacement, redact.String(header))
	require.Equal(t, public, redact.String(public))
	require.Nil(t, cfg.RedactedForTransport().authenticationCandidates)
	_, configured := cfg.Providers.Get(registration.ProviderID)
	require.False(t, configured)
}

func TestRegisterRemoteCredentialSecretsExcludesPublicMetadata(t *testing.T) {
	secrets := []string{
		"remote-binding-api-key-secret",
		"remote-binding-oauth-access-secret",
		"remote-binding-oauth-refresh-secret",
		"remote-binding-oauth-client-secret",
		"remote-binding-account-access-secret",
		"remote-binding-account-refresh-secret",
		"remote-binding-account-raw-secret",
	}
	public := []string{
		"remote-binding-provider-id",
		"remote-binding-manifest-id",
		"remote-binding-manifest-version",
		"remote-binding-preset-digest",
		"remote-binding-account-id",
		"remote-binding-account-display",
		"remote-binding-generation-marker",
	}
	binding := RemoteCredentialBinding{
		Owner: providerregistry.RegistrationOwner{
			ProviderID:      public[0],
			ManifestID:      public[1],
			ManifestVersion: public[2],
			PresetDigest:    public[3],
		},
		Generation: 73,
		APIKey:     secrets[0],
		OAuthToken: &oauth.Token{
			AccessToken:  secrets[1],
			RefreshToken: secrets[2],
			Client:       &oauth.OAuthClient{ClientSecret: secrets[3]},
		},
		Account: &accounts.Entry{
			ID:           public[4],
			DisplayName:  public[5],
			AccessToken:  secrets[4],
			RefreshToken: secrets[5],
			Raw:          []byte(`{"nested":"` + secrets[6] + `"}`),
		},
	}

	RegisterRemoteCredentialSecrets(binding)
	for _, secret := range secrets {
		require.Equal(t, redact.Replacement, redact.String(secret))
	}
	for _, value := range public {
		require.Equal(t, value, redact.String(value))
	}
}
