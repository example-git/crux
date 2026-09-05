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
	mcpSecret := "config-mcp-client-secret-value"
	mcpToken := "config-mcp-access-token-value"
	cfg := &Config{
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"test": {APIKey: providerKey, OAuthToken: &oauth.Token{RefreshToken: providerRefresh}},
		}),
		MCP: MCPs{"test": {OAuthClientSecret: mcpSecret, OAuthToken: &oauth.Token{AccessToken: mcpToken}}},
	}
	registerConfigSecrets(cfg)
	for _, secret := range []string{providerKey, providerRefresh, mcpSecret, mcpToken} {
		require.Equal(t, redact.Replacement, redact.String(secret))
	}
}
