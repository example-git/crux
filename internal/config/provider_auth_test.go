package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func authenticationCaptureTestStore(t *testing.T) (*ConfigStore, providerregistry.RegistrationOwner, accounts.Entry, string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	registration := ownerTestRegistration("capture-provider", "capture.plugin")
	entry := accounts.Entry{ID: "first", DisplayName: "First", AccessToken: "synthetic-capture-access", RefreshToken: "synthetic-capture-refresh", Raw: json.RawMessage(`{"private":"synthetic-capture-raw"}`)}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, entry))
	provider := forwardedAccountOwnerTestProvider(registration, entry.Token())
	store := NewTestStoreWithRegistrations(&Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{registration.ProviderID: provider})}, registration)
	return store, registration.Owner(), entry, root
}

func TestCaptureAuthenticationRetainsCoherentPrivateObservation(t *testing.T) {
	store, owner, entry, _ := authenticationCaptureTestStore(t)
	first, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	repeated, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, first.SameObservation(repeated))
	require.True(t, first.runtime.SamePublication(store.RuntimeSnapshot()))
	require.Equal(t, "in-sync", first.Providers()[0].AccountState)
	_, err = json.Marshal(first)
	require.ErrorContains(t, err, "authentication captures are private")
	for _, output := range []string{fmt.Sprint(first), fmt.Sprintf("%#v", first)} {
		for _, secret := range []string{entry.AccessToken, entry.RefreshToken, owner.AccountNamespace, "synthetic-capture-raw"} {
			require.NotContains(t, output, secret)
		}
	}

	// Both changes occur under the config publication lock. A capture admitted
	// after this transaction must contain the new config and the new account.
	store.writeMu.Lock()
	updated := entry
	updated.ID, updated.DisplayName, updated.AccessToken = "second", "Second", "synthetic-capture-next"
	err = accounts.Save(t.Context(), owner.AccountNamespace, updated)
	next := store.Config().cloneForWrite()
	provider, _ := next.Providers.Get(owner.ProviderID)
	provider.APIKey, provider.OAuthToken = updated.AccessToken, updated.Token()
	next.Providers.Set(owner.ProviderID, provider)
	store.setConfig(next)
	store.writeMu.Unlock()
	require.NoError(t, err)
	second, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.False(t, first.SameObservation(second))
	require.Equal(t, "second", second.Providers()[0].ActiveAccountID)
	require.Equal(t, "in-sync", second.Providers()[0].AccountState)
	require.Equal(t, "first", first.Providers()[0].ActiveAccountID)
	oldAccounts, err := first.Accounts(owner)
	require.NoError(t, err)
	require.Len(t, oldAccounts, 1)
	oldAccounts[0].DisplayName = "caller mutation"
	oldAccounts, err = first.Accounts(owner)
	require.NoError(t, err)
	require.Equal(t, "First", oldAccounts[0].DisplayName)

	// Same-value config publications and account-only mutations each invalidate
	// freshness independently, while the retained observation stays unchanged.
	store.setConfig(store.Config())
	published, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.False(t, second.SameObservation(published))
	require.True(t, second.accounts.SameObservation(published.accounts))
	require.NoError(t, accounts.Save(t.Context(), owner.AccountNamespace, updated))
	saved, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.True(t, published.runtime.SamePublication(saved.runtime))
	require.False(t, published.SameObservation(saved))
}

func TestCaptureAuthenticationHoldsPublicationDuringAccountRead(t *testing.T) {
	store, owner, entry, _ := authenticationCaptureTestStore(t)
	before := store.RuntimeSnapshot()
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	held, release := make(chan struct{}), make(chan struct{})
	accountDone := make(chan error, 1)
	go func() {
		accountDone <- accounts.WithSelectedForOwner(ctx, owner.AccountNamespace, entry, func() error { return nil }, func() error {
			close(held)
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}()
	select {
	case <-held:
	case <-ctx.Done():
		t.Fatal("account fixture failed to acquire its lock")
	}
	type capturedResult struct {
		capture AuthenticationCapture
		err     error
	}
	result := make(chan capturedResult, 1)
	go func() {
		capture, err := store.CaptureAuthentication(ctx)
		result <- capturedResult{capture: capture, err: err}
	}()
	require.Eventually(t, func() bool {
		if store.writeMu.TryLock() {
			store.writeMu.Unlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond, "capture must hold the config publication lock while account capture waits")
	select {
	case <-result:
		t.Fatal("capture returned before it could inspect the account store")
	default:
	}
	close(release)
	require.NoError(t, <-accountDone)
	completed := <-result
	require.NoError(t, completed.err)
	require.True(t, before.SamePublication(completed.capture.runtime))
	require.Equal(t, "in-sync", completed.capture.Providers()[0].AccountState)
}

func TestCaptureAuthenticationUsesCapturedPathsWithoutLiveFallback(t *testing.T) {
	homeVariable, otherHomeVariable := "HOME", "USERPROFILE"
	if runtime.GOOS == "windows" {
		homeVariable, otherHomeVariable = otherHomeVariable, homeVariable
	}
	for _, variable := range []string{"AI_CLI_DIR", homeVariable} {
		t.Run(variable, func(t *testing.T) {
			root, live := t.TempDir(), t.TempDir()
			captured := env.NewFromMap(map[string]string{variable: root})
			store := NewTestStoreWithRegistrations(&Config{}, ownerTestRegistration("capture-provider", "capture.plugin"))
			store.effectiveEnvironment = captured
			for _, key := range []string{"AI_CLI_DIR", "HOME", "USERPROFILE"} {
				t.Setenv(key, live)
			}
			_, err := store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			accountRoot := root
			if variable != "AI_CLI_DIR" {
				accountRoot = filepath.Join(root, ".ai-cli")
			}
			_, err = os.Stat(filepath.Join(accountRoot, "accounts.json.lock"))
			require.NoError(t, err)
			entries, err := os.ReadDir(live)
			require.NoError(t, err)
			require.Empty(t, entries, "live environment must not route account I/O")
		})
	}
	t.Run("disagreeing captured homes", func(t *testing.T) {
		home, other := t.TempDir(), t.TempDir()
		store := NewTestStoreWithRegistrations(&Config{}, ownerTestRegistration("capture-provider", "capture.plugin"))
		store.effectiveEnvironment = env.NewFromMap(map[string]string{homeVariable: home, otherHomeVariable: other})
		_, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		_, err = os.Stat(filepath.Join(home, ".ai-cli", "accounts.json.lock"))
		require.NoError(t, err)
		entries, err := os.ReadDir(other)
		require.NoError(t, err)
		require.Empty(t, entries, "the other platform's home must not receive account I/O")
	})
	for _, captured := range []map[string]string{{}, {"AI_CLI_DIR": "relative"}, {homeVariable: "relative"}, {otherHomeVariable: t.TempDir()}} {
		live := t.TempDir()
		t.Setenv("AI_CLI_DIR", live)
		t.Setenv("HOME", live)
		t.Setenv("USERPROFILE", live)
		store := NewTestStoreWithRegistrations(&Config{}, ownerTestRegistration("capture-provider", "capture.plugin"))
		store.effectiveEnvironment = env.NewFromMap(captured)
		_, err := store.CaptureAuthentication(t.Context())
		require.EqualError(t, err, "authentication account store cannot be read")
		entries, err := os.ReadDir(live)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
}

func TestCaptureAuthenticationEmptyCatalogueDoesNotResolveAccountPaths(t *testing.T) {
	for _, captured := range []map[string]string{{}, {"AI_CLI_DIR": "relative"}, {"HOME": "relative", "USERPROFILE": "relative"}} {
		live := t.TempDir()
		for _, key := range []string{"AI_CLI_DIR", "HOME", "USERPROFILE"} {
			t.Setenv(key, live)
		}
		store := NewTestStoreWithRegistrations(&Config{})
		store.effectiveEnvironment = env.NewFromMap(captured)
		capture, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		require.Empty(t, capture.Providers())
		again, err := store.CaptureAuthentication(t.Context())
		require.NoError(t, err)
		require.True(t, capture.SameObservation(again))
		entries, err := os.ReadDir(live)
		require.NoError(t, err)
		require.Empty(t, entries, "empty account authority must not resolve or lock ambient storage")
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = store.CaptureAuthentication(ctx)
		require.ErrorIs(t, err, context.Canceled)
	}
}

func TestCaptureAuthenticationDetachedRejectsBeforePathAccess(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "state"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := remoteBaselineTree(t, root)
	for _, environment := range []map[string]string{
		{}, {"AI_CLI_DIR": "relative"}, {"AI_CLI_DIR": filepath.Join(root, "accounts")}, {"HOME": filepath.Join(root, "home")},
	} {
		store.effectiveEnvironment = env.NewFromMap(environment)
		_, err := store.CaptureAuthentication(t.Context())
		require.ErrorIs(t, err, ErrClientRuntimeManaged, "authority must be checked before even validating the account path")
		require.Equal(t, before, remoteBaselineTree(t, root))
	}
}

func TestCaptureAuthenticationCancellationWhileWaitingForPublication(t *testing.T) {
	root := t.TempDir()
	store := NewTestStoreWithRegistrations(&Config{})
	store.effectiveEnvironment = env.NewFromMap(map[string]string{"AI_CLI_DIR": filepath.Join(root, "accounts")})
	store.writeMu.Lock()
	ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := store.CaptureAuthentication(ctx)
	elapsed := time.Since(started)
	store.writeMu.Unlock()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, elapsed, time.Second)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries, "canceled lock admission must precede account-path I/O")
	canceled, stop := context.WithCancel(t.Context())
	stop()
	_, err = store.CaptureAuthentication(canceled)
	require.ErrorIs(t, err, context.Canceled)
	entries, err = os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestCaptureAuthenticationIncludesUnconfiguredOwners(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	registration := ownerTestRegistration("installed-provider", "installed.plugin")
	preset := ProviderPresetReference{ID: "installed.preset", Version: "1.0.0", Digest: "preset-digest"}
	store := NewTestStoreWithProviderGeneration(&Config{}, map[string]ProviderPresetReference{"preset-provider": preset}, registration)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	providers := capture.Providers()
	require.Len(t, providers, 2)
	for _, provider := range providers {
		require.False(t, provider.Configured)
		require.False(t, provider.APIKeyConfigured)
		require.Equal(t, "absent", provider.OAuthState)
		entries, err := capture.Accounts(provider.Owner)
		require.NoError(t, err)
		require.Empty(t, entries)
	}
	require.Equal(t, registration.Owner(), providers[0].Owner)
	require.Equal(t, providerregistry.RegistrationOwner{ProviderID: "preset-provider", HasPreset: true, PresetID: preset.ID, PresetVersion: preset.Version, PresetDigest: preset.Digest}, providers[1].Owner)
}

func TestCaptureAuthenticationDoesNotSubstituteUnavailableOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	oldRegistration := ownerTestRegistration("same-provider", "previous.plugin")
	currentRegistration := ownerTestRegistration("same-provider", "replacement.plugin")
	entry := accounts.Entry{ID: "replacement-account", AccessToken: "synthetic-replacement-access"}
	require.NoError(t, accounts.Save(t.Context(), currentRegistration.AccountNamespace, entry))
	provider := forwardedAccountOwnerTestProvider(oldRegistration, entry.Token())
	store := NewTestStoreWithRegistrations(&Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{provider.ID: provider})}, currentRegistration)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	require.Empty(t, capture.Providers())
	for _, owner := range []providerregistry.RegistrationOwner{oldRegistration.Owner(), currentRegistration.Owner(), {ProviderID: provider.ID}} {
		_, err := capture.Accounts(owner)
		require.ErrorContains(t, err, "owner changed")
	}
	require.Empty(t, capture.accounts.Entries(currentRegistration.AccountNamespace), "a new registration must not route accounts for the unavailable configured owner")
}
