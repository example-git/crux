package cmd

import (
	"encoding/json"
	"github.com/example-git/crux/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestCollectedRuntimeIncludesOnlySelectedCanonicalAccount(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	entry := accounts.Entry{ID: "forwarded", DisplayName: "Forwarded", AccessToken: "account-secret", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(dataDir, "crux.json"),
		[]byte(`{"providers":{"codex":{"api_key":"account-secret","owner":{"type":"core","construction":"integrated-codex"},"models":[{"id":"fixture-model","name":"Fixture"}]}},"models":{"large":{"provider":"codex","model":"fixture-model"},"small":{"provider":"codex","model":"fixture-model"}}}`),
		0o600,
	))

	proposal, err := collectRemoteProviderState(t.Context(), root, dataDir, false, 1)
	require.NoError(t, err)
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("codex")
	require.True(t, ok)
	owner := registration.Owner()
	require.Equal(t, accounts.ProviderCodex, owner.AccountNamespace)
	require.Len(t, proposal.Credentials, 1)
	require.Equal(t, owner, proposal.Credentials[0].Owner)
	require.Equal(t, &entry, proposal.Credentials[0].Account)

	require.NoError(t, accounts.Save(t.Context(), "unknown.accounts", accounts.Entry{ID: "unknown", AccessToken: "unknown-secret"}))
	proposal, err = collectRemoteProviderState(t.Context(), root, dataDir, false, 2)
	require.NoError(t, err)
	data, err := json.Marshal(proposal)
	require.NoError(t, err)
	require.NotContains(t, string(data), "unknown-secret")
	require.Len(t, proposal.Credentials, 1)
	require.Len(t, proposal.Providers, 1)
	require.Equal(t, "codex", proposal.Models[config.SelectedModelTypeLarge].Provider)
	changed := accounts.Entry{ID: "changed", AccessToken: "different-active-token"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, changed))
	require.NoError(t, accounts.SetActive(t.Context(), accounts.ProviderCodex, changed.ID))
	_, err = collectRemoteProviderState(t.Context(), root, dataDir, false, 3)
	require.ErrorContains(t, err, "selected client account changed")
}
