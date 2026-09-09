package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func copilotImportAuthorityStore(t *testing.T, imported func(context.Context) (*oauth.Token, bool, error)) (*ConfigStore, providerregistry.RegistrationOwner) {
	t.Helper()
	store, owner, _ := authenticationCOWOAuthStore(t, "copilot")
	registrations := store.providerRegistry.Registrations()
	for i := range registrations {
		if registrations[i].ProviderID == owner.ProviderID {
			registrations[i].OAuth.Import = imported
		}
	}
	registry, err := providerregistry.New(registrations...)
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	scan := cloneProviderScan(*next.providerScan)
	scan.Registry = registry
	next.bindProviderScan(scan)
	store.providerRegistry = registry
	store.setConfig(next)
	return store, owner
}

func TestCopilotImportAdvancesCapturedLogoutAccount(t *testing.T) {
	exchange, calls := authenticationCOWHTTPS(t, "synthetic-import")
	store, owner := copilotImportAuthorityStore(t, func(ctx context.Context) (*oauth.Token, bool, error) {
		token, err := exchange(ctx, "synthetic-import")
		return token, err == nil, err
	})
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	logout, err := store.LogoutAuthentication(t.Context(), ScopeGlobal, capture, owner)
	require.NoError(t, err)
	require.True(t, logout.RuntimePublished)
	old := store.RuntimeSnapshot()
	prior, retained, err := old.CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.True(t, retained)
	require.Nil(t, prior)
	token, imported, err := store.ImportCopilotForOwner(t.Context(), owner)
	require.NoError(t, err)
	require.True(t, imported)
	require.Equal(t, int32(1), calls.Load())
	current, retained, err := store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.True(t, retained)
	require.NotNil(t, current, "successful import must replace authoritative captured absence")
	require.Equal(t, "default", current.ID)
	require.Equal(t, token.AccessToken, current.AccessToken)
	prior, _, err = old.CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Nil(t, prior, "old runtime authority remains immutable")
}

func TestCopilotImportCanceledConfigLockDoesNotSaveAccount(t *testing.T) {
	exchanged := make(chan struct{})
	store, owner := copilotImportAuthorityStore(t, func(context.Context) (*oauth.Token, bool, error) {
		close(exchanged)
		return selectedRefreshToken(), true, nil
	})
	previous := store.Config()
	before, err := captureCopilotImportAccounts(t.Context(), store.RuntimeSnapshot(), owner.AccountNamespace)
	require.NoError(t, err)
	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	release, err := lock.File(t.Context(), store.globalDataPath+".lock")
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := store.ImportCopilotForOwner(ctx, owner)
		done <- err
	}()
	select {
	case <-exchanged:
	case <-time.After(3 * time.Second):
		t.Fatal("import did not reach exchange")
	}
	require.Eventually(t, func() bool {
		if store.mu.TryLock() {
			store.mu.Unlock()
			return false
		}
		return true
	}, 3*time.Second, time.Millisecond, "import must be waiting for the held config lock")
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(3 * time.Second):
		t.Fatal("canceled config lock wait did not finish")
	}
	after, err := captureCopilotImportAccounts(t.Context(), store.RuntimeSnapshot(), owner.AccountNamespace)
	require.NoError(t, err)
	require.True(t, before.SameObservation(after))
	require.Same(t, previous, store.Config())
	actual, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, disk, actual)
}

func TestCopilotImportDoesNotOverwriteAccountChangedDuringExchange(t *testing.T) {
	var namespace string
	manual := accounts.Entry{ID: "default", AccessToken: "synthetic-manual"}
	store, owner := copilotImportAuthorityStore(t, func(ctx context.Context) (*oauth.Token, bool, error) {
		if err := accounts.Save(ctx, namespace, manual); err != nil {
			return nil, false, err
		}
		return selectedRefreshToken(), true, nil
	})
	namespace = owner.AccountNamespace
	previous := store.Config()
	disk, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	token, imported, err := store.ImportCopilotForOwner(t.Context(), owner)
	require.ErrorIs(t, err, accounts.ErrStateChanged)
	require.False(t, imported)
	require.Nil(t, token)
	require.Same(t, previous, store.Config())
	state, err := accounts.CaptureStateAt(t.Context(), filepath.Join(store.RuntimeSnapshot().Getenv("AI_CLI_DIR"), "accounts.json"), []string{namespace})
	require.NoError(t, err)
	require.Equal(t, []accounts.Entry{manual}, state.Entries(namespace))
	actual, err := os.ReadFile(store.globalDataPath)
	require.NoError(t, err)
	require.Equal(t, disk, actual)
}
