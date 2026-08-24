package config

import (
	"strings"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/redact"
)

func registerAccountSecrets(entry accounts.Entry) {
	redact.Register(entry.AccessToken, entry.RefreshToken)
}

func registerConfigSecrets(cfg *Config) {
	if cfg == nil {
		return
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			redact.Register(provider.APIKey)
			if provider.OAuthToken != nil {
				redact.Register(provider.OAuthToken.AccessToken, provider.OAuthToken.RefreshToken)
			}
			for name, value := range provider.ExtraHeaders {
				if secretHeaderName(name) {
					redact.Register(value)
				}
			}
			if registration, ok := ProviderCapabilities().Lookup(id); ok && registration.Manifest != nil {
				for field, display := range registration.Manifest.Configuration.Fields {
					if display.Secret {
						if value, ok := provider.Configuration[field].(string); ok {
							redact.Register(value)
						}
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
