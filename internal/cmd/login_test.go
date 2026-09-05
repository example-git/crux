package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestLoginCmd_Aliases(t *testing.T) {
	t.Parallel()

	require.Equal(t, "auth", loginCmd.Aliases[0])
}

func TestLoginCmd_ForceFlag(t *testing.T) {
	t.Parallel()

	flag := loginCmd.Flags().Lookup("force")
	require.NotNil(t, flag)
	require.Equal(t, "f", flag.Shorthand)
}

func TestLoginProviderRejectsOwnerReplacementAfterImportedRefresh(t *testing.T) {
	providerID := string(catalog.ProviderCopilot)
	var registration providerregistry.Registration
	for _, candidate := range providerregistry.Integrated() {
		if candidate.ProviderID == providerID {
			registration = candidate
			break
		}
	}
	require.NotNil(t, registration.OAuth)
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		providerID: {
			ID: providerID,
			Owner: &config.ProviderOwnerReference{
				Type:         config.ProviderOwnerCore,
				Construction: providerregistry.ConstructionCopilot,
			},
		},
	})}
	registration.OAuth.Import = func(context.Context) (*oauth.Token, bool, error) {
		provider, ok := cfg.Providers.Get(providerID)
		require.True(t, ok)
		provider.Owner = &config.ProviderOwnerReference{
			Type:         config.ProviderOwnerCustom,
			Construction: providerregistry.ConstructionOpenAICompat,
		}
		cfg.Providers.Set(providerID, provider)
		return &oauth.Token{AccessToken: "stale-access", RefreshToken: "stale-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}, true, nil
	}
	store := config.NewTestStoreWithRegistrations(cfg, registration)
	ws := workspace.NewAppWorkspace(nil, store)

	err := loginProvider(ws, registration, true)
	require.ErrorContains(t, err, "provider account owner copilot changed")
	provider, ok := cfg.Providers.Get(providerID)
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	require.Equal(t, config.ProviderOwnerCustom, provider.Owner.Type)
}
