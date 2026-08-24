package dialog

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestActiveAccountProvidersOmitStoredInactiveIntegrations(t *testing.T) {
	registry, err := providerregistry.New(providerregistry.Registration{
		ProviderID:       "active",
		AccountNamespace: "active-account",
		AccountAliases:   []string{"active-account-legacy"},
	})
	require.NoError(t, err)

	providers := activeAccountProviders(
		[]string{"codex", "active-account", "gemini", "active-account-legacy"},
		registry,
	)
	require.Equal(t, []string{"active-account", "active-account-legacy"}, providers)
}

func TestProviderEntriesOmitInactiveIntegrations(t *testing.T) {
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"inactive": {
				ID:     "inactive",
				Name:   "Inactive Plugin",
				Plugin: &config.ProviderPluginReference{ID: "inactive.plugin", Version: "1"},
			},
			"disabled-custom": {
				ID:      "disabled-custom",
				Name:    "Disabled Custom",
				Disable: true,
				Type:    catwalk.TypeOpenAICompat,
			},
		}),
	}
	known := []catwalk.Provider{{ID: "available", Name: "Available"}}

	entries := providerEntries(cfg, known)
	require.Equal(t, []providerEntry{
		{id: "available", name: "Available"},
		{id: "disabled-custom", name: "Disabled Custom", disabled: true},
	}, entries)

	_, preserved := cfg.Providers.Get("inactive")
	require.True(t, preserved, "filtering presentation must not delete persisted plugin configuration")
}
