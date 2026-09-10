package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationRemoveActiveInactiveAndLast(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			before := f.capture(t)
			old := f.store.Config()
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			calls := 0
			f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				calls++
				entry, ok, err := snapshot.CapturedConstructionAccount(f.owner)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, f.second.ID, entry.ID)
				return RuntimeGenerationCandidate{Commit: func() {}, Abort: func() {}}, nil
			})
			id := f.second.ID
			if active {
				id = f.first.ID
			}
			result, err := f.store.RemoveAuthenticationAccount(t.Context(), ScopeGlobal, before, f.owner, id)
			require.NoError(t, err)
			require.True(t, result.AccountsSaved)
			require.False(t, result.AccountRefreshed)
			require.Equal(t, active, result.RuntimePublished)
			require.Equal(t, active, result.ConfigSaved)
			_, coherent := result.RuntimeSnapshot()
			require.True(t, coherent)
			require.True(t, result.After.SameObservation(f.capture(t)))
			provider, _ := f.store.Config().Providers.Get(f.owner.ProviderID)
			if active {
				require.Equal(t, 1, calls)
				require.Equal(t, f.second.AccessToken, provider.APIKey)
			} else {
				require.Zero(t, calls)
				require.Same(t, old, f.store.Config())
				afterData, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, data, afterData)
				require.Equal(t, f.first.AccessToken, provider.APIKey)
			}
			remaining, err := accounts.List(t.Context(), f.owner.AccountNamespace)
			require.NoError(t, err)
			require.Len(t, remaining, 1)
			require.NotEqual(t, id, remaining[0].ID)
			f.store.SetRuntimeGenerationPreparer(nil)
			last, err := f.store.RemoveAuthenticationAccount(t.Context(), ScopeGlobal, result.After, f.owner, remaining[0].ID)
			require.NoError(t, err)
			require.True(t, last.RuntimePublished)
			require.True(t, last.AccountsSaved)
			require.Empty(t, last.After.accounts.Entries(f.owner.AccountNamespace))
			require.Empty(t, last.After.accounts.ActiveID(f.owner.AccountNamespace))
			for _, state := range last.After.Providers() {
				if state.Owner == f.owner {
					require.False(t, state.APIKeyConfigured)
					require.Equal(t, "absent", state.OAuthState)
					require.Equal(t, "none", state.AccountState)
				}
			}
		})
	}
}

func TestAuthenticationRemovePreparationFailurePreservesAccounts(t *testing.T) {
	f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
	before := f.capture(t)
	f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{}, errors.New("synthetic preparation failure")
	})
	result, err := f.store.RemoveAuthenticationAccount(t.Context(), ScopeGlobal, before, f.owner, f.first.ID)
	require.ErrorContains(t, err, "synthetic preparation failure")
	require.False(t, result.AccountsSaved)
	require.False(t, result.RuntimePublished)
	require.True(t, before.SameObservation(f.capture(t)))
}

func TestAuthenticationRemoveRejectsPrecommitDrift(t *testing.T) {
	for _, kind := range []string{"account", "config", "publication", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			before := f.capture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.second))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, ' '), 0o600))
			case "publication":
				f.store.setConfig(f.store.Config())
			case "canceled":
				cancel()
			}
			current := f.capture(t)
			result, err := f.store.RemoveAuthenticationAccount(ctx, ScopeGlobal, before, f.owner, f.second.ID)
			require.Error(t, err)
			require.False(t, result.AccountsSaved)
			require.False(t, result.RuntimePublished)
			require.True(t, current.SameObservation(f.capture(t)))
		})
	}
}

func TestAuthenticationRemoveLateProgressAndCancellation(t *testing.T) {
	for _, change := range []string{"account drift", "caller canceled"} {
		t.Run(change, func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			before := f.capture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				return RuntimeGenerationCandidate{Commit: func() {
					if change == "caller canceled" {
						cancel()
						return
					}
					path := filepath.Join(f.root, "accounts", "accounts.json")
					data, err := os.ReadFile(path)
					require.NoError(t, err)
					require.NoError(t, os.WriteFile(path, append(data, ' '), 0o600))
				}, Abort: func() {}}, nil
			})
			result, err := f.store.RemoveAuthenticationAccount(ctx, ScopeGlobal, before, f.owner, f.first.ID)
			require.True(t, result.AccountsSaved && result.ConfigSaved && result.RuntimePublished)
			_, coherent := result.RuntimeSnapshot()
			if change == "caller canceled" {
				require.NoError(t, err)
				require.True(t, coherent)
				require.ErrorIs(t, ctx.Err(), context.Canceled)
			} else {
				require.Error(t, err)
				require.False(t, coherent)
			}
			remaining, err := accounts.List(t.Context(), f.owner.AccountNamespace)
			require.NoError(t, err)
			require.Len(t, remaining, 1)
			require.Equal(t, f.second.ID, remaining[0].ID)
		})
	}
}
