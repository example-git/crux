package config

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func selectedRefreshFixture(t *testing.T) (*ConfigStore, providerregistry.RegistrationOwner, accounts.Entry) {
	t.Helper()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	store := newRefreshTestStore(t, filepath.Join(t.TempDir(), "config.json"), nil)
	store.config.bindProviderScan(ProviderScan{Registry: store.providerRegistry})
	owner := refreshTestOwner(t, store)
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	entry := accounts.FromToken("selected", "Selected", provider.OAuthToken, nil)
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, entry))
	return store, owner, entry
}

func selectedRefreshToken() *oauth.Token {
	return &oauth.Token{AccessToken: "synthetic-fresh", RefreshToken: "synthetic-rotated", ExpiresIn: 3600, ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestSelectedOAuthRefreshPersistsAndAdoptsPeerRotation(t *testing.T) {
	first, owner, entry := selectedRefreshFixture(t)
	second := newRefreshTestStore(t, first.globalDataPath, nil)
	second.config.bindProviderScan(ProviderScan{Registry: second.providerRegistry})
	var calls atomic.Int32
	exchange := func(context.Context, string, string) (*oauth.Token, error) {
		calls.Add(1)
		return selectedRefreshToken(), nil
	}
	first.exchangeToken, second.exchangeToken = exchange, exchange
	for _, store := range []*ConfigStore{first, second, first} {
		fresh, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
		require.NoError(t, err)
		require.Equal(t, "synthetic-rotated", fresh.RefreshToken)
		provider, _ := store.Config().Providers.Get(owner.ProviderID)
		require.Equal(t, fresh.Token(), provider.OAuthToken)
		stored, err := accounts.Active(t.Context(), owner.AccountNamespace)
		require.NoError(t, err)
		require.Equal(t, accounts.CredentialID(*fresh), accounts.CredentialID(*stored))
		data, err := os.ReadFile(store.globalDataPath)
		require.NoError(t, err)
		require.Equal(t, fresh.RefreshToken, gjson.GetBytes(data, "providers.codex.oauth.refresh_token").String())
	}
	require.EqualValues(t, 1, calls.Load())
}

func TestSelectedOAuthRefreshRejectsConcurrentMutation(t *testing.T) {
	for _, action := range []string{"disk-credential", "disk-owner", "memory-credential", "account-switch", "logout", "owner"} {
		t.Run(action, func(t *testing.T) {
			store, owner, entry := selectedRefreshFixture(t)
			store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
				switch action {
				case "disk-credential", "disk-owner":
					data, err := os.ReadFile(store.globalDataPath)
					require.NoError(t, err)
					key := "providers.codex.api_key"
					if action == "disk-owner" {
						key = "providers.codex.owner.type"
					}
					data, err = sjson.SetBytes(data, key, "manual-change")
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(store.globalDataPath, data, 0o600))
				case "memory-credential", "owner":
					store.mutateInMemory(func(cfg *Config) {
						provider, _ := cfg.Providers.Get(owner.ProviderID)
						if action == "owner" {
							provider.Disable = true
						} else {
							provider.APIKey = "manual-change"
						}
						cfg.Providers.Set(owner.ProviderID, provider)
					})
				case "account-switch":
					require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, accounts.Entry{ID: "other", AccessToken: "other-token"}))
				case "logout":
					require.NoError(t, accounts.RemoveProvider(t.Context(), owner.AccountNamespace))
				}
				return selectedRefreshToken(), nil
			}
			_, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
			require.Error(t, err)
			provider, _ := store.Config().Providers.Get(owner.ProviderID)
			require.NotEqual(t, "synthetic-fresh", provider.APIKey)
			stored, readErr := accounts.Active(t.Context(), owner.AccountNamespace)
			require.NoError(t, readErr)
			switch action {
			case "disk-credential", "disk-owner", "memory-credential":
				require.ErrorContains(t, err, "account token saved; provider config was not updated")
				require.Equal(t, "synthetic-rotated", stored.RefreshToken)
			case "account-switch":
				require.Equal(t, "other", stored.ID)
			case "logout":
				require.Nil(t, stored)
			case "owner":
				require.Equal(t, entry.RefreshToken, stored.RefreshToken)
			}
		})
	}
}

func TestSelectedOAuthRefreshFinishesLocalCommitAfterDisconnect(t *testing.T) {
	store, owner, entry := selectedRefreshFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	store.exchangeToken = func(exchangeCtx context.Context, _, _ string) (*oauth.Token, error) {
		cancel()
		require.NoError(t, exchangeCtx.Err())
		return selectedRefreshToken(), nil
	}
	fresh, err := store.RefreshSelectedOAuthAccount(ctx, ScopeGlobal, owner, entry, true)
	require.NoError(t, err)
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, fresh.Token(), provider.OAuthToken)
}

func TestSelectedOAuthRefreshWriteFailureRetainsRotatedAccount(t *testing.T) {
	store, owner, entry := selectedRefreshFixture(t)
	store.exchangeToken = func(context.Context, string, string) (*oauth.Token, error) {
		require.NoError(t, os.Remove(store.globalDataPath))
		require.NoError(t, os.Mkdir(store.globalDataPath, 0o700))
		return selectedRefreshToken(), nil
	}
	_, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
	require.ErrorContains(t, err, "account token saved; provider config was not updated")
	stored, err := accounts.Active(t.Context(), owner.AccountNamespace)
	require.NoError(t, err)
	require.Equal(t, "synthetic-rotated", stored.RefreshToken)
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, entry.RefreshToken, provider.OAuthToken.RefreshToken)
}
