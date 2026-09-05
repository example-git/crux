package accounts

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func setup(t *testing.T) context.Context {
	t.Helper()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	return context.Background()
}

func TestConcurrentProviderPublicationKeepsNamespaceAndRefresherTogether(t *testing.T) {
	oldRefresh := func(context.Context, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "old"}, nil
	}
	newRefresh := func(context.Context, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "new"}, nil
	}
	oldGeneration := []ProviderRegistration{{ProviderID: "provider", Namespace: "old", Refresher: oldRefresh}}
	newGeneration := []ProviderRegistration{{ProviderID: "provider", Namespace: "new", Refresher: newRefresh}}
	PublishProviders(oldGeneration)
	t.Cleanup(func() { PublishProviders(nil) })

	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for index := range 500 {
				if index%2 == 0 {
					PublishProviders(oldGeneration)
				} else {
					PublishProviders(newGeneration)
				}
				namespace, refresher, ok := ProviderSnapshot("provider")
				if !ok || refresher == nil {
					t.Errorf("published provider snapshot missing")
					return
				}
				token, err := refresher(t.Context(), "refresh")
				if err != nil || token.AccessToken != namespace {
					t.Errorf("torn provider snapshot: namespace=%q token=%v err=%v", namespace, token, err)
					return
				}
			}
		}()
	}
	wait.Wait()
}

func TestProviderPublicationBlocksReadersUntilCommitCompletes(t *testing.T) {
	oldGeneration := []ProviderRegistration{{ProviderID: "provider", Namespace: "old"}}
	newGeneration := []ProviderRegistration{{ProviderID: "provider", Namespace: "new"}}
	PublishProviders(oldGeneration)
	t.Cleanup(func() { PublishProviders(nil) })

	entered := make(chan struct{})
	release := make(chan struct{})
	published := make(chan struct{})
	go func() {
		PublishProvidersWith(newGeneration, func() {
			close(entered)
			<-release
		})
		close(published)
	}()
	<-entered
	require.False(t, providerMu.TryRLock())

	observed := make(chan string, 1)
	go func() {
		namespace, _, _ := ProviderSnapshot("provider")
		observed <- namespace
	}()
	close(release)
	<-published
	require.Equal(t, "new", <-observed)
}

func TestFailedProviderPublicationRetainsPreviousGeneration(t *testing.T) {
	oldRefresh := func(context.Context, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "old"}, nil
	}
	newRefresh := func(context.Context, string) (*oauth.Token, error) {
		return &oauth.Token{AccessToken: "new"}, nil
	}
	PublishProviders([]ProviderRegistration{{ProviderID: "provider", Namespace: "old", Refresher: oldRefresh}})
	t.Cleanup(func() { PublishProviders(nil) })

	err := PublishProvidersTransaction([]ProviderRegistration{{ProviderID: "provider", Namespace: "new", Refresher: newRefresh}}, func() error {
		return errors.New("publish blocked")
	})
	require.EqualError(t, err, "publish blocked")
	namespace, refresher, ok := ProviderSnapshot("provider")
	require.True(t, ok)
	require.Equal(t, "old", namespace)
	token, err := refresher(t.Context(), "refresh")
	require.NoError(t, err)
	require.Equal(t, "old", token.AccessToken)
}

func TestProvidersForUsesExplicitGenerationOrder(t *testing.T) {
	ctx := setup(t)
	PublishProviders([]ProviderRegistration{
		{ProviderID: "global-first", Namespace: "generation-first", Order: 10},
		{ProviderID: "global-second", Namespace: "generation-second", Order: 20},
	})
	t.Cleanup(func() { PublishProviders(nil) })
	for _, provider := range []string{"generation-first", "generation-second", "orphan-z", "orphan-a"} {
		require.NoError(t, Save(ctx, provider, Entry{ID: provider, AccessToken: provider + "-token"}))
	}

	providers, err := ProvidersFor(ctx, []string{"generation-second", "", "generation-first", "generation-second"})
	require.NoError(t, err)
	require.Equal(t, []string{"generation-second", "generation-first", "orphan-a", "orphan-z"}, providers)

	providers, err = Providers(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"generation-first", "generation-second", "orphan-a", "orphan-z"}, providers)
}

func TestEnsureFreshForOwnerRejectsReplacementAfterExchangeWithoutWriting(t *testing.T) {
	ctx := setup(t)
	entry := Entry{
		ID: "account", AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	require.NoError(t, Save(ctx, "owner-refresh", entry))
	active := true
	refresher := func(context.Context, string) (*oauth.Token, error) {
		active = false
		return &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}
	validate := func() error {
		if !active {
			return errors.New("owner changed")
		}
		return nil
	}

	_, err := EnsureFreshForOwner(ctx, "owner-refresh", &entry, refresher, validate)
	require.ErrorContains(t, err, "owner changed")
	stored, err := Active(ctx, "owner-refresh")
	require.NoError(t, err)
	require.Equal(t, "old-access", stored.AccessToken)
	require.Equal(t, "old-refresh", stored.RefreshToken)
}

func TestEnsureFreshForOwnerRejectsReplacementAtPersistenceBoundary(t *testing.T) {
	ctx := setup(t)
	entry := Entry{
		ID: "account", AccessToken: "old-access", RefreshToken: "old-refresh",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}
	require.NoError(t, Save(ctx, "owner-refresh-boundary", entry))
	exchanged := false
	afterExchange := 0
	refresher := func(context.Context, string) (*oauth.Token, error) {
		exchanged = true
		return &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	}
	validate := func() error {
		if exchanged {
			afterExchange++
			if afterExchange == 6 {
				return errors.New("owner changed")
			}
		}
		return nil
	}

	_, err := EnsureFreshForOwner(ctx, "owner-refresh-boundary", &entry, refresher, validate)
	require.ErrorContains(t, err, "owner changed")
	require.Equal(t, 6, afterExchange)
	stored, err := Active(ctx, "owner-refresh-boundary")
	require.NoError(t, err)
	require.Equal(t, "old-access", stored.AccessToken)
	require.Equal(t, "old-refresh", stored.RefreshToken)
}

type ownerMutationTest struct {
	name    string
	prepare func(*testing.T)
	mutate  func(Validator) error
}

func ownerMutationTests(ctx context.Context) []ownerMutationTest {
	return []ownerMutationTest{
		{
			name: "save",
			prepare: func(t *testing.T) {
				require.NoError(t, Save(ctx, "owner-save", Entry{ID: "existing", AccessToken: "old"}))
			},
			mutate: func(validate Validator) error {
				return SaveForOwner(ctx, "owner-save", Entry{ID: "new", AccessToken: "new"}, validate)
			},
		},
		{
			name: "save without activating",
			prepare: func(t *testing.T) {
				require.NoError(t, Save(ctx, "owner-save-inactive", Entry{ID: "existing", AccessToken: "old"}))
			},
			mutate: func(validate Validator) error {
				return SaveWithoutActivatingForOwner(ctx, "owner-save-inactive", Entry{ID: "new", AccessToken: "new"}, validate)
			},
		},
		{
			name: "set active",
			prepare: func(t *testing.T) {
				require.NoError(t, Save(ctx, "owner-active", Entry{ID: "one", AccessToken: "one"}))
				require.NoError(t, Save(ctx, "owner-active", Entry{ID: "two", AccessToken: "two"}))
			},
			mutate: func(validate Validator) error {
				return SetActiveForOwner(ctx, "owner-active", "one", validate)
			},
		},
		{
			name: "remove",
			prepare: func(t *testing.T) {
				require.NoError(t, Save(ctx, "owner-remove", Entry{ID: "one", AccessToken: "one"}))
				require.NoError(t, Save(ctx, "owner-remove", Entry{ID: "two", AccessToken: "two"}))
			},
			mutate: func(validate Validator) error {
				return RemoveForOwner(ctx, "owner-remove", "one", validate)
			},
		},
		{
			name: "remove provider",
			prepare: func(t *testing.T) {
				require.NoError(t, Save(ctx, "owner-remove-provider", Entry{ID: "one", AccessToken: "one"}))
			},
			mutate: func(validate Validator) error {
				return RemoveProviderForOwner(ctx, "owner-remove-provider", validate)
			},
		},
	}
}

func accountStoreBytes(t *testing.T) []byte {
	t.Helper()
	path, err := dbPath()
	require.NoError(t, err)
	value, err := os.ReadFile(path)
	require.NoError(t, err)
	return value
}

func TestOwnerBoundMutationsRevalidateAtLockedWriteBoundary(t *testing.T) {
	for _, test := range ownerMutationTests(context.Background()) {
		t.Run(test.name, func(t *testing.T) {
			setup(t)
			test.prepare(t)
			before := accountStoreBytes(t)

			err := test.mutate(nil)
			require.ErrorContains(t, err, "owner validator is required")
			require.Equal(t, before, accountStoreBytes(t))

			checks := 0
			err = test.mutate(func() error {
				checks++
				if checks == 4 {
					return errors.New("owner changed before rename")
				}
				return nil
			})
			require.ErrorContains(t, err, "owner changed before rename")
			require.Equal(t, 4, checks)
			require.Equal(t, before, accountStoreBytes(t))
			path, pathErr := dbPath()
			require.NoError(t, pathErr)
			_, statErr := os.Stat(path + ".tmp")
			require.ErrorIs(t, statErr, os.ErrNotExist)

			mu.Lock()
			current := true
			entered := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				first := true
				result <- test.mutate(func() error {
					if first {
						first = false
						if !current {
							return errors.New("owner changed while waiting for lock")
						}
						close(entered)
						return nil
					}
					if !current {
						return errors.New("owner changed while waiting for lock")
					}
					return nil
				})
			}()
			select {
			case <-entered:
				current = false
				mu.Unlock()
			case <-time.After(time.Second):
				mu.Unlock()
				t.Fatal("owner-bound mutation did not reach the account lock")
			}
			err = <-result
			require.ErrorContains(t, err, "owner changed while waiting for lock")
			require.Equal(t, before, accountStoreBytes(t))

			checks = 0
			require.NoError(t, test.mutate(func() error {
				checks++
				return nil
			}))
			require.Equal(t, 4, checks)
			require.NotEqual(t, before, accountStoreBytes(t))
		})
	}
}

func TestSaveListActive(t *testing.T) {
	ctx := setup(t)
	if err := Save(ctx, ProviderCodex, Entry{ID: "a@x.com", DisplayName: "a@x.com", AccessToken: "tok-a"}); err != nil {
		t.Fatal(err)
	}
	if err := Save(ctx, ProviderCodex, Entry{ID: "b@x.com", DisplayName: "b@x.com", AccessToken: "tok-b"}); err != nil {
		t.Fatal(err)
	}

	list, err := List(ctx, ProviderCodex)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %v, err = %v", list, err)
	}
	active, err := Active(ctx, ProviderCodex)
	if err != nil || active == nil || active.ID != "b@x.com" {
		t.Fatalf("active = %+v, err = %v", active, err)
	}

	// Upsert updates in place without duplicating.
	if err := Save(ctx, ProviderCodex, Entry{ID: "a@x.com", DisplayName: "a@x.com", AccessToken: "tok-a2"}); err != nil {
		t.Fatal(err)
	}
	list, _ = List(ctx, ProviderCodex)
	if len(list) != 2 {
		t.Fatalf("upsert duplicated: %v", list)
	}
	active, _ = Active(ctx, ProviderCodex)
	if active.ID != "a@x.com" || active.AccessToken != "tok-a2" {
		t.Fatalf("active = %+v", active)
	}
}

func TestSetActiveAndRemove(t *testing.T) {
	ctx := setup(t)
	_ = Save(ctx, ProviderGemini, Entry{ID: "one", AccessToken: "t1"})
	_ = Save(ctx, ProviderGemini, Entry{ID: "two", AccessToken: "t2"})

	if err := SetActive(ctx, ProviderGemini, "one"); err != nil {
		t.Fatal(err)
	}
	if err := SetActive(ctx, ProviderGemini, "missing"); err == nil {
		t.Fatal("expected error for missing account")
	}

	if err := Remove(ctx, ProviderGemini, "one"); err != nil {
		t.Fatal(err)
	}
	active, _ := Active(ctx, ProviderGemini)
	if active == nil || active.ID != "two" {
		t.Fatalf("active after remove = %+v", active)
	}
	if err := Remove(ctx, ProviderGemini, "two"); err != nil {
		t.Fatal(err)
	}
	active, _ = Active(ctx, ProviderGemini)
	if active != nil {
		t.Fatalf("active should be nil, got %+v", active)
	}
}

func TestExpiredAndRefresh(t *testing.T) {
	ctx := setup(t)
	expired := Entry{
		ID:           "u@x.com",
		AccessToken:  "old",
		RefreshToken: "refresh-1",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}
	if !expired.Expired() {
		t.Fatal("entry should be expired")
	}
	_ = Save(ctx, "testprov", expired)

	RegisterRefresher("testprov", func(ctx context.Context, refreshToken string) (*oauth.Token, error) {
		if refreshToken != "refresh-1" {
			return nil, errors.New("wrong refresh token")
		}
		return &oauth.Token{
			AccessToken:  "new",
			RefreshToken: "refresh-2",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	})

	token, err := AccessToken(ctx, "testprov")
	if err != nil {
		t.Fatal(err)
	}
	if token != "new" {
		t.Fatalf("token = %q", token)
	}

	// Refresh must have been persisted with the rotated refresh token.
	active, _ := Active(ctx, "testprov")
	if active.AccessToken != "new" || active.RefreshToken != "refresh-2" {
		t.Fatalf("persisted = %+v", active)
	}
	if active.Expired() {
		t.Fatal("refreshed entry should not be expired")
	}
}

func TestAccessTokenNoAccount(t *testing.T) {
	ctx := setup(t)
	token, err := AccessToken(ctx, "missing-provider")
	if err != nil || token != "" {
		t.Fatalf("token = %q, err = %v", token, err)
	}
}

func TestRemoveProviderClearsAllAccounts(t *testing.T) {
	ctx := setup(t)
	require.NoError(t, Save(ctx, "remove-provider-test", Entry{ID: "one", AccessToken: "token-one"}))
	require.NoError(t, Save(ctx, "remove-provider-test", Entry{ID: "two", AccessToken: "token-two"}))

	require.NoError(t, RemoveProvider(ctx, "remove-provider-test"))
	entries, err := List(ctx, "remove-provider-test")
	require.NoError(t, err)
	require.Empty(t, entries)
	active, err := Active(ctx, "remove-provider-test")
	require.NoError(t, err)
	require.Nil(t, active)
}

func TestStoreKey(t *testing.T) {
	RegisterProvider("synthetic-oauth", "synthetic", 10)
	RegisterProvider("codex", ProviderCodex, 20)
	RegisterProvider("copilot", ProviderCopilot, 30)
	RegisterProvider("gemini-ag", ProviderGemini, 40)
	cases := map[string]string{
		"synthetic-oauth": "synthetic",
		"codex":           ProviderCodex,
		"copilot":         ProviderCopilot,
		"gemini-ag":       ProviderGemini,
		"openai":          "",
	}
	for in, want := range cases {
		if got := StoreKey(in); got != want {
			t.Errorf("StoreKey(%q) = %q, want %q", in, got, want)
		}
	}
}
