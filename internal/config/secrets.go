package config

import (
	"strings"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
)

func registerAccountSecrets(entry accounts.Entry) {
	redact.Register(entry.AccessToken, entry.RefreshToken)
	redact.RegisterJSONBytes(entry.Raw)
}

func registerOAuthTokenSecrets(token *oauth.Token) {
	if token == nil {
		return
	}
	redact.Register(token.AccessToken, token.RefreshToken)
	if token.Client != nil {
		redact.Register(token.Client.ClientSecret)
	}
}

func RegisterRemoteCredentialSecrets(binding RemoteCredentialBinding) {
	redact.Register(binding.APIKey)
	registerOAuthTokenSecrets(binding.OAuthToken)
	if binding.Account != nil {
		registerAccountSecrets(*binding.Account)
	}
}

func registerProviderSecrets(provider ProviderConfig, registration providerregistry.Registration, registered bool) {
	redact.Register(provider.APIKey, provider.APIKeyTemplate)
	for _, binding := range provider.resolvedCredentials {
		if binding != nil {
			redact.Register(binding.source, binding.literal)
		}
	}
	registerOAuthTokenSecrets(provider.OAuthToken)
	for name, value := range provider.ExtraHeaders {
		if provider.Preset != nil || secretHeaderName(name) {
			redact.Register(value)
		}
	}
	if provider.Preset != nil {
		redact.Register(provider.BaseURL, provider.SystemPromptPrefix)
		redact.RegisterJSONValue(provider.ExtraBody)
		redact.RegisterJSONValue(provider.ProviderOptions)
		redact.RegisterJSONValue(provider.Configuration)
		redact.RegisterJSONValue(provider.ExtraParams)
		return
	}
	if registered && registration.Manifest != nil {
		for _, credential := range registration.Manifest.Capabilities.Credentials {
			if credential.ConfigProperty != "" {
				redact.RegisterJSONValue(provider.Configuration[credential.ConfigProperty])
			}
		}
		for field, display := range registration.Manifest.Configuration.Fields {
			if display.Secret {
				redact.RegisterJSONValue(provider.Configuration[field])
			}
		}
	}
}

func registerConfigSecrets(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Images != nil {
		for _, provider := range cfg.Images.Providers {
			redact.RegisterJSONValue(provider.Configuration)
		}
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			registration, registered := cfg.ProviderRegistration(id)
			registerProviderSecrets(provider, registration, registered)
		}
	}
	for id, provider := range cfg.authenticationCandidates {
		registration, registered := providerRegistrationForProvider(cfg.providerCapabilities(), id, provider)
		registerProviderSecrets(provider, registration, registered)
	}
	for _, mcp := range cfg.MCP {
		redact.Register(mcp.OAuthClientSecret)
		if mcp.OAuthToken != nil {
			redact.Register(mcp.OAuthToken.AccessToken, mcp.OAuthToken.RefreshToken)
		}
		for name, value := range mcp.Headers {
			if secretHeaderName(name) {
				redact.Register(value)
			}
		}
	}
}

func secretHeaderName(name string) bool {
	normalized := strings.NewReplacer("-", "", "_", "").Replace(strings.ToLower(name))
	return strings.Contains(normalized, "authorization") || strings.Contains(normalized, "apikey") || strings.Contains(normalized, "token") || strings.Contains(normalized, "secret") || strings.Contains(normalized, "password") || strings.Contains(normalized, "credential") || strings.Contains(normalized, "cookie")
}
