package config

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/stretchr/testify/require"
)

func localValidationOperation(ctx context.Context, digit string) (context.Context, AuthenticationJournalKey) {
	key := AuthenticationJournalKey{Kind: AuthenticationJournalLocal, WorkspaceID: "local-validation", OperationID: strings.Repeat(digit, 32)}
	return ContextWithAuthenticationOperation(ctx, key), key
}

func loadLocalValidation(t *testing.T, store *ConfigStore, key AuthenticationJournalKey) LocalAuthenticationChange {
	t.Helper()
	value, found, err := store.LoadAuthenticationLocalChange(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	require.NoError(t, value.Summary().Validate())
	return value
}

func TestAuthenticationLocalValidationCoherentAndNoEffects(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "coherent", true: "before-stage"}[fail], func(t *testing.T) {
			f := newAuthenticationMutationFixture(t, ScopeGlobal, false)
			before := f.capture(t)
			originalConfig, err := os.ReadFile(f.path)
			require.NoError(t, err)
			accountPath := filepath.Join(f.root, "accounts", "accounts.json")
			originalAccounts, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			models := f.store.Config().Models
			if fail {
				f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
					return RuntimeGenerationCandidate{}, errors.New("synthetic before stage")
				})
			}
			ctx, key := localValidationOperation(t.Context(), "a")
			outcome, err := f.store.SwitchAuthenticationAccount(ctx, f.scope, before, f.owner, "second")
			record := loadLocalValidation(t, f.store, key)
			entry, found, loadErr := record.journal.Load(t.Context(), key)
			require.NoError(t, loadErr)
			require.True(t, found)
			require.True(t, entry.Completed())
			require.Zero(t, entry.ReservedBytes())
			if fail {
				require.ErrorContains(t, err, "synthetic before stage")
				require.True(t, record.Summary().NoEffects)
				require.False(t, record.Summary().Abandoned)
				require.Equal(t, LocalAuthenticationProgress{}, record.Summary().Original)
				after, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.Equal(t, originalConfig, after)
				after, err = os.ReadFile(accountPath)
				require.NoError(t, err)
				require.Equal(t, originalAccounts, after)
				_, err = f.store.RepairAuthenticationLocalChange(t.Context(), record)
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				require.True(t, outcome.RuntimePublished)
				require.True(t, record.Summary().Coherent)
				repair, err := f.store.RepairAuthenticationLocalChange(t.Context(), record)
				require.NoError(t, err)
				require.True(t, repair.NeedsReload)
				require.False(t, repair.ConfigWritten || repair.AccountsWritten)
				same := loadLocalValidation(t, f.store, key)
				require.Equal(t, record.Summary(), same.Summary(), "completed repair read must not change its journal revision")
			}
			require.Equal(t, models, f.store.Config().Models)
		})
	}
}

func TestAuthenticationLocalValidationUnknownRefreshLeaseAndAbandon(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enteredOnce, releaseOnce sync.Once
	var calls atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "synthetic-inactive-refresh" {
			t.Error("unexpected synthetic refresh request")
		}
		enteredOnce.Do(func() { close(entered) })
		<-release
		http.Error(w, "synthetic unknown refresh response", http.StatusBadGateway)
	}))
	defer endpoint.Close()
	defer releaseOnce.Do(func() { close(release) })
	prior := http.DefaultTransport
	http.DefaultTransport = endpoint.Client().Transport
	defer func() { http.DefaultTransport = prior }()
	f := newAuthenticationCandidateFixture(t, "example-responses", false, false, endpoint.URL+"/token")
	t.Setenv("AI_CLI_DIR", filepath.Join(f.root, "accounts"))
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, accounts.Entry{ID: "active", AccessToken: "synthetic-current", RefreshToken: "synthetic-current-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, accounts.Entry{ID: "inactive", AccessToken: "synthetic-expired", RefreshToken: "synthetic-inactive-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}))
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	ctx, key := localValidationOperation(t.Context(), "b")
	finished := make(chan error, 1)
	go func() {
		_, err := f.store.SwitchAuthenticationAccount(ctx, ScopeGlobal, before, f.owner, "inactive")
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not start")
	}
	// The read observes started/unknown while the actual HTTPS exchange holds
	// the producer lease. A bounded abandonment cannot overtake that producer.
	record := loadLocalValidation(t, f.store, key)
	require.True(t, record.Summary().RefreshStarted)
	require.False(t, record.Summary().RefreshObserved)
	limited, cancel := context.WithTimeout(t.Context(), 75*time.Millisecond)
	abandoned, err := f.store.AbandonAuthenticationLocalChange(limited, record, record.Summary().Revision)
	cancel()
	require.Error(t, err)
	require.False(t, abandoned.Abandoned)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-finished:
		require.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("refresh did not finish")
	}
	current := loadLocalValidation(t, f.store, key)
	require.True(t, current.Summary().RefreshStarted)
	require.False(t, current.Summary().RefreshObserved)
	require.False(t, current.Summary().NoEffects)
	_, err = f.store.AbandonAuthenticationLocalChange(t.Context(), record, record.Summary().Revision)
	require.ErrorContains(t, err, "revision changed")
	_, err = f.store.RepairAuthenticationLocalChange(t.Context(), current)
	require.ErrorIs(t, err, ErrLocalAuthenticationRefreshUnresolved)
	original := current.Summary().Original
	abandoned, err = f.store.AbandonAuthenticationLocalChange(t.Context(), current, current.Summary().Revision)
	require.NoError(t, err)
	require.True(t, abandoned.Abandoned)
	require.Equal(t, original, abandoned.Summary.Original)
	require.False(t, abandoned.AccountsWritten || abandoned.ConfigWritten || abandoned.NeedsReload)
	retry, err := f.store.AbandonAuthenticationLocalChange(t.Context(), current, current.Summary().Revision)
	require.NoError(t, err)
	require.Equal(t, abandoned, retry)
	terminal := loadLocalValidation(t, f.store, key)
	entry, _, err := terminal.journal.Load(t.Context(), key)
	require.NoError(t, err)
	require.True(t, entry.Completed())
	require.Zero(t, entry.ReservedBytes())
	require.EqualValues(t, 1, calls.Load(), "repair and abandonment never repeat the unknown exchange")
	active, err := accounts.Active(t.Context(), f.owner.AccountNamespace)
	require.NoError(t, err)
	require.Equal(t, "active", active.ID)
	_, err = f.store.RepairAuthenticationLocalChange(t.Context(), terminal)
	require.Error(t, err)
}

func TestAuthenticationLocalValidationObservedRefreshRepairsExactOriginal(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires non-root POSIX directory write permissions")
	}
	var accountPath string
	var calls atomic.Int32
	endpoint := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// Deny only the disposable directory write. This leaves the complete
		// original account inode, timestamps and bytes untouched. The returned
		// token and fixed successor are journaled before account staging fails.
		if err := os.Chmod(filepath.Dir(accountPath), 0o500); err != nil {
			t.Error(err)
			http.Error(w, "fixture permission change failed", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"synthetic-observed-successor","refresh_token":"synthetic-rotated-refresh","expires_in":3600}`))
	}))
	defer endpoint.Close()
	prior := http.DefaultTransport
	http.DefaultTransport = endpoint.Client().Transport
	defer func() { http.DefaultTransport = prior }()
	f := newAuthenticationCandidateFixture(t, "example-responses", false, false, endpoint.URL+"/token")
	accountPath = filepath.Join(f.root, "accounts", "accounts.json")
	defer func() { _ = os.Chmod(filepath.Dir(accountPath), 0o700) }()
	t.Setenv("AI_CLI_DIR", filepath.Dir(accountPath))
	require.NoError(t, accounts.Save(t.Context(), f.owner.AccountNamespace, accounts.Entry{ID: "active", AccessToken: "synthetic-active", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, accounts.Entry{ID: "inactive", AccessToken: "synthetic-expired", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}))
	before, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	originalModels := f.store.Config().Models
	ctx, key := localValidationOperation(t.Context(), "c")
	original, err := f.store.SwitchAuthenticationAccount(ctx, ScopeGlobal, before, f.owner, "inactive")
	require.Error(t, err)
	require.False(t, original.ConfigSaved || original.RuntimePublished || original.AccountsSaved)
	record := loadLocalValidation(t, f.store, key)
	require.True(t, record.Summary().RefreshObserved)
	require.True(t, record.Summary().RepairReady)
	require.False(t, record.Summary().NoEffects)
	require.NoError(t, os.Chmod(filepath.Dir(accountPath), 0o700))
	revision := record.Summary().Revision
	repaired, err := f.store.RepairAuthenticationLocalChange(t.Context(), record, revision)
	require.NoError(t, err)
	require.True(t, repaired.AccountsWritten)
	require.True(t, repaired.NeedsReload)
	require.False(t, repaired.ConfigWritten)
	require.Equal(t, record.Summary().Original, repaired.Summary.Original)
	active, err := accounts.Active(t.Context(), f.owner.AccountNamespace)
	require.NoError(t, err)
	require.Equal(t, "active", active.ID)
	entries, err := accounts.List(t.Context(), f.owner.AccountNamespace)
	require.NoError(t, err)
	foundInactive := false
	for _, entry := range entries {
		if entry.ID == "inactive" {
			foundInactive = true
			require.Equal(t, "synthetic-observed-successor", entry.AccessToken)
			require.Equal(t, "synthetic-rotated-refresh", entry.RefreshToken)
		}
	}
	require.True(t, foundInactive)
	retry, err := f.store.RepairAuthenticationLocalChange(t.Context(), record, revision)
	require.NoError(t, err)
	require.Equal(t, repaired, retry)
	require.EqualValues(t, 1, calls.Load(), "repair and exact retry must never consume the refresh token again")
	require.Equal(t, originalModels, f.store.Config().Models)
	// Newer user account state cannot be overwritten by replaying this original
	// repair receipt. The receipt still describes its original fixed writes.
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, accounts.Entry{ID: "unrelated", AccessToken: "synthetic-new-user-state"}))
	retry, err = f.store.RepairAuthenticationLocalChange(t.Context(), record, revision)
	require.NoError(t, err)
	require.Equal(t, repaired, retry)
	entries, err = accounts.List(t.Context(), f.owner.AccountNamespace)
	require.NoError(t, err)
	found := false
	for _, entry := range entries {
		found = found || entry.ID == "unrelated"
	}
	require.True(t, found)
}
