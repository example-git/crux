package config

import (
	"strings"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/redact"
)

func registerAccountSecrets(entry accounts.Entry) {
	redact.Register(entry.AccessToken, entry.RefreshToken)
	redact.RegisterJSONBytes(entry.Raw)
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
			redact.Register(provider.APIKey, provider.APIKeyTemplate)
			if provider.OAuthToken != nil {
				redact.Register(provider.OAuthToken.AccessToken, provider.OAuthToken.RefreshToken)
			}
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
				continue
			}
			if registration, ok := cfg.ProviderRegistration(id); ok && registration.Manifest != nil {
				for field, display := range registration.Manifest.Configuration.Fields {
					if display.Secret {
						redact.RegisterJSONValue(provider.Configuration[field])
					}
				}
			}
		}
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
