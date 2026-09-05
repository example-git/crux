package config

import (
	"testing"

	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestBindProviderSurfaceOwnersRetainsExactOwnersAndCloneIsolation(t *testing.T) {
	owner := providerregistry.RegistrationOwner{
		ProviderID:           "same",
		AccountNamespace:     "same-account",
		Construction:         providerregistry.ConstructionGenericJSON,
		CompatibilityAdapter: providerregistry.ConstructionOpenAICompat,
		HasOAuth:             true,
		OAuthAdapter:         providerregistry.LoginBrowser,
		OAuthFlowID:          "login",
		HasManifest:          true,
		ManifestID:           "plugin.same",
		ManifestVersion:      "1.2.3",
	}
	cfg := &Config{}
	require.NoError(t, cfg.BindProviderSurfaceOwners([]providerregistry.Surface{
		{ID: owner.ProviderID, Owner: &owner},
		{ID: "unavailable"},
	}))

	got, ok := cfg.ProviderOwner(owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, owner, got)
	_, ok = cfg.ProviderOwner("unavailable")
	require.False(t, ok)

	clone := cfg.cloneForWrite()
	cloneOwner := clone.transportProviderOwners[owner.ProviderID]
	cloneOwner.ManifestVersion = "9.9.9"
	clone.transportProviderOwners[owner.ProviderID] = cloneOwner

	got, ok = cfg.ProviderOwner(owner.ProviderID)
	require.True(t, ok)
	require.Equal(t, "1.2.3", got.ManifestVersion)
}

func TestBindProviderSurfaceOwnersRejectsMalformedOwners(t *testing.T) {
	tests := map[string][]providerregistry.Surface{
		"mismatched provider": {{ID: "same", Owner: &providerregistry.RegistrationOwner{ProviderID: "other"}}},
		"missing surface ID":  {{Owner: &providerregistry.RegistrationOwner{ProviderID: "same"}}},
		"duplicate owner": {
			{ID: "same", Owner: &providerregistry.RegistrationOwner{ProviderID: "same"}},
			{ID: "same", Owner: &providerregistry.RegistrationOwner{ProviderID: "same"}},
		},
	}
	for name, surfaces := range tests {
		t.Run(name, func(t *testing.T) {
			require.Error(t, (&Config{}).BindProviderSurfaceOwners(surfaces))
		})
	}
}
