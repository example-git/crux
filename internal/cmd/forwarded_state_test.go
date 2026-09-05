package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestForwardedProviderStateBindsAccountsToExactCanonicalOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	entry := accounts.Entry{ID: "forwarded", DisplayName: "Forwarded", AccessToken: "account-secret"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "crux.json"),
		[]byte(`{"providers":{"codex":{"api_key":"account-secret","owner":{"type":"core","construction":"integrated-codex"}}}}`),
		0o600,
	))

	_, forwarded, err := forwardedProviderState(t.Context(), root, dataDir, false)
	require.NoError(t, err)
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	owner := registration.Owner()
	require.Equal(t, accounts.ProviderCodex, owner.AccountNamespace)
	require.Equal(t, owner, forwarded[owner.AccountNamespace].Owner)
	require.Equal(t, entry, forwarded[owner.AccountNamespace].Entry)

	require.NoError(t, accounts.Save(t.Context(), "unknown.accounts", accounts.Entry{ID: "unknown", AccessToken: "unknown-secret"}))
	_, _, err = forwardedProviderState(t.Context(), root, dataDir, false)
	require.ErrorContains(t, err, `account namespace "unknown.accounts" does not match an active exact provider owner`)
}
