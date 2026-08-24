package config

import (
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

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
