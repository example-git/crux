package providerauth

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func authenticationFixture(t *testing.T) (*config.ConfigStore, providerregistry.RegistrationOwner, accounts.Entry, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	registration := providerregistry.Registration{ProviderID: "codex", AccountNamespace: "private-fixture-namespace", Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}
	entry := accounts.Entry{ID: "account-one", DisplayName: "Account One", AccessToken: "synthetic-private-access", RefreshToken: "synthetic-private-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(), Raw: json.RawMessage(`{"private":"synthetic-private-raw"}`)}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, entry))
	cfg := &config.Config{Options: &config.Options{DataDirectory: filepath.Join(root, "data")}, Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"codex": {ID: "codex", APIKey: entry.AccessToken, OAuthToken: entry.Token()}})}
	store := config.NewTestStoreWithRegistrations(cfg, registration)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("codex")
	require.True(t, ok)
	require.Equal(t, registration.AccountNamespace, owner.AccountNamespace)
	return store, owner, entry, root
}

func statusFor(t *testing.T, snapshot Snapshot, providerID string) Status {
	t.Helper()
	for _, status := range snapshot.Providers {
		if status.Owner.ProviderID == providerID {
			return status
		}
	}
	t.Fatalf("provider %q missing in status", providerID)
	return Status{}
}

func TestAuthenticationServiceStableGenerationPrivateStateAndCapturedPath(t *testing.T) {
	store, owner, entry, root := authenticationFixture(t)
	service := New(store, "workspace-one")
	first, err := service.Status(t.Context())
	require.NoError(t, err)
	second, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, second)
	status := statusFor(t, first, owner.ProviderID)
	require.True(t, status.Configured)
	require.Equal(t, "in-sync", status.AccountState)
	require.Equal(t, CredentialStatus{Kind: "oauth", State: "present", Refreshable: true}, status.Credentials[1], "expiry does not make refreshable credentials absent")
	target := Target{WorkspaceID: first.WorkspaceID, Owner: status.Owner, Generation: first.Generation}
	list, err := service.Accounts(t.Context(), target)
	require.NoError(t, err)
	require.Len(t, list.Accounts, 1)
	require.True(t, list.Accounts[0].Active)
	require.Equal(t, entry.ExpiresAt, list.Accounts[0].ExpiresAt)
	encoded, err := json.Marshal(struct {
		Snapshot Snapshot
		Accounts AccountsState
	}{first, list})
	require.NoError(t, err)
	for _, private := range []string{"synthetic-private", owner.AccountNamespace, "account_namespace", "access_token", "refresh_token", root} {
		require.NotContains(t, string(encoded), private)
	}
	other := t.TempDir()
	t.Setenv("AI_CLI_DIR", other)
	t.Setenv("HOME", other)
	unchanged, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, first, unchanged)
	_, err = os.Stat(filepath.Join(other, "accounts.json.lock"))
	require.True(t, os.IsNotExist(err))
	restarted, err := New(store, "workspace-one").Status(t.Context())
	require.NoError(t, err)
	require.NotEqual(t, first.Generation.Epoch, restarted.Generation.Epoch)
	_, err = New(store, "workspace-one").Accounts(t.Context(), target)
	require.ErrorIs(t, err, ErrStale)
}

func TestAuthenticationServiceAccountFreshnessAndOutOfSync(t *testing.T) {
	store, owner, entry, _ := authenticationFixture(t)
	service := New(store, "workspace-one")
	first, err := service.Status(t.Context())
	require.NoError(t, err)
	target := Target{WorkspaceID: first.WorkspaceID, Owner: PublicOwner(owner), Generation: first.Generation}
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, entry))
	_, err = service.Accounts(t.Context(), target)
	require.ErrorIs(t, err, ErrStale)
	second, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, first.Generation.Sequence+1, second.Generation.Sequence)
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, accounts.Entry{ID: "inactive", DisplayName: "Inactive", AccessToken: "synthetic-other"}))
	require.NoError(t, accounts.SetActive(t.Context(), owner.AccountNamespace, entry.ID))
	third, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Greater(t, third.Generation.Sequence, second.Generation.Sequence)
	require.Equal(t, "in-sync", statusFor(t, third, owner.ProviderID).AccountState)
	require.NoError(t, accounts.SetActive(t.Context(), owner.AccountNamespace, "inactive"))
	fourth, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, "out-of-sync", statusFor(t, fourth, owner.ProviderID).AccountState)
	require.Equal(t, "inactive", statusFor(t, fourth, owner.ProviderID).ActiveAccountID)
	// A status read never copies the newly selected account into runtime config.
	configured, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Equal(t, entry.AccessToken, configured.APIKey)
	require.NoError(t, accounts.SetActive(t.Context(), owner.AccountNamespace, entry.ID))
	fifth, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, "in-sync", statusFor(t, fifth, owner.ProviderID).AccountState)
	require.Greater(t, fifth.Generation.Sequence, fourth.Generation.Sequence)
	wrong := Target{WorkspaceID: fifth.WorkspaceID, Owner: PublicOwner(owner), Generation: fifth.Generation}
	wrong.Owner.OAuthFlowID = "other"
	_, err = service.Accounts(t.Context(), wrong)
	require.Error(t, err)
}

func TestAuthenticationServiceUnconfiguredDisabledAndExpression(t *testing.T) {
	store, owner, _, _ := authenticationFixture(t)
	service := New(store, "workspace-one")
	first, err := service.Status(t.Context())
	require.NoError(t, err)
	// Same-value publication must invalidate an earlier target, independently of accounts.
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.NoError(t, store.ApplyEphemeralProviderState(map[string]config.ProviderConfig{owner.ProviderID: provider}, nil))
	second, err := service.Status(t.Context())
	require.NoError(t, err)
	require.Greater(t, second.Generation.Sequence, first.Generation.Sequence)
	registration := providerregistry.Registration{ProviderID: "codex", AccountNamespace: owner.AccountNamespace, Construction: providerregistry.ConstructionCodex, OAuth: &providerregistry.OAuthCapability{}}
	unconfigured := config.NewTestStoreWithRegistrations(&config.Config{}, registration)
	unconfiguredStatus, err := New(unconfigured, "unconfigured").Status(t.Context())
	require.NoError(t, err)
	require.False(t, statusFor(t, unconfiguredStatus, "codex").Configured)
	require.Equal(t, "out-of-sync", statusFor(t, unconfiguredStatus, "codex").AccountState)
	marker := filepath.Join(t.TempDir(), "must-not-run")
	expression := "$(touch " + marker + ")"
	expr := config.NewTestStore(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"custom": {ID: "custom", APIKey: expression, Disable: true}})})
	exprStatus, err := New(expr, "expression").Status(t.Context())
	require.NoError(t, err)
	p := statusFor(t, exprStatus, "custom")
	require.True(t, p.Disabled)
	require.Equal(t, "configured", p.Credentials[0].State)
	_, err = os.Stat(marker)
	require.True(t, os.IsNotExist(err))
	encoded, err := json.Marshal(exprStatus)
	require.NoError(t, err)
	require.False(t, strings.Contains(string(encoded), expression))
}

func TestAuthenticationServiceCancellationAndEpochValidationBeforeIO(t *testing.T) {
	store, owner, _, root := authenticationFixture(t)
	service := New(store, "workspace-one")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := service.Status(ctx)
	require.ErrorIs(t, err, context.Canceled)
	service.gate <- struct{}{}
	blocked, stop := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer stop()
	_, err = service.Status(blocked)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	<-service.gate
	require.NoError(t, os.Remove(filepath.Join(root, "accounts.json.lock")))
	wrong := Target{WorkspaceID: "workspace-one", Owner: PublicOwner(owner), Generation: Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	_, err = service.Accounts(t.Context(), wrong)
	require.ErrorIs(t, err, ErrStale)
	_, err = os.Stat(filepath.Join(root, "accounts.json.lock"))
	require.True(t, os.IsNotExist(err))
}
