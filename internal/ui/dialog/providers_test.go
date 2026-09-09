package dialog

import (
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

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
				Type:    catalog.TypeOpenAICompat,
			},
		}),
	}
	known := []catalog.Provider{{ID: "available", Name: "Available"}}

	entries := providerEntries(cfg, known)
	require.Equal(t, []providerEntry{
		{id: "available", name: "Available"},
		{
			id:       "disabled-custom",
			name:     "Disabled Custom",
			disabled: true,
			owner:    providerregistry.RegistrationOwner{ProviderID: "disabled-custom"},
			ownerSet: true,
		},
	}, entries)

	_, preserved := cfg.Providers.Get("inactive")
	require.True(t, preserved, "filtering presentation must not delete persisted plugin configuration")
}
