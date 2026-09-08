package accounts

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestSelectedAccountRefreshAcrossProcesses(t *testing.T) {
	const childEnv = "CRUX_ACCOUNT_ROTATION_CHILD"
	if os.Getenv(childEnv) == "1" {
		entry := Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh"}
		_, err := RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
			file, err := os.OpenFile(filepath.Join(os.Getenv("AI_CLI_DIR"), "exchanges"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = file.WriteString("exchange\n")
			closeErr := file.Close()
			if err != nil || closeErr != nil {
				return nil, errors.Join(err, closeErr)
			}
			return rotatedToken(), nil
		}, func() error { return nil }, true)
		require.NoError(t, err)
		return
	}
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	require.NoError(t, Save(t.Context(), "rotation", Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh"}))
	binary, err := os.Executable()
	require.NoError(t, err)
	results := make(chan error, 2)
	for range 2 {
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestSelectedAccountRefreshAcrossProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		go func() {
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Log(string(output))
			}
			results <- err
		}()
	}
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	exchanges, err := os.ReadFile(filepath.Join(root, "exchanges"))
	require.NoError(t, err)
	require.Equal(t, "exchange\n", string(exchanges), "separate processes must consume the refresh token only once")
	stored, err := Active(t.Context(), "rotation")
	require.NoError(t, err)
	require.Equal(t, "synthetic-new-refresh", stored.RefreshToken)
}

func rotationFixture(t *testing.T) Entry {
	t.Helper()
	t.Setenv("AI_CLI_DIR", t.TempDir())
	entry := Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}
	require.NoError(t, Save(t.Context(), "rotation", entry))
	return entry
}

func rotatedToken() *oauth.Token {
	return &oauth.Token{AccessToken: "synthetic-new", RefreshToken: "synthetic-new-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
}

func TestSelectedAccountRefreshSharesExactCompletedRotation(t *testing.T) {
	entry := rotationFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	refresh := func(context.Context, string) (*oauth.Token, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return rotatedToken(), nil
	}
	type result struct {
		entry *Entry
		err   error
	}
	results := make(chan result, 2)
	run := func() {
		fresh, err := RefreshSelectedForOwner(t.Context(), "rotation", &entry, refresh, func() error { return nil }, true)
		results <- result{fresh, err}
	}
	go run()
	<-started
	go run()
	close(release)
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.Equal(t, CredentialID(*first.entry), CredentialID(*second.entry))
	require.EqualValues(t, 1, calls.Load(), "concurrent callers must not exchange an already consumed refresh token")
	stored, err := Active(t.Context(), "rotation")
	require.NoError(t, err)
	require.Equal(t, "synthetic-new-refresh", stored.RefreshToken)
	// A manual replacement is not a peer refresh, even at the same account ID.
	require.NoError(t, Save(t.Context(), "rotation", Entry{ID: entry.ID, AccessToken: "synthetic-manual", RefreshToken: "synthetic-manual-refresh"}))
	_, err = RefreshSelectedForOwner(t.Context(), "rotation", &entry, refresh, func() error { return nil }, true)
	require.ErrorIs(t, err, ErrCredentialChanged)
	require.EqualValues(t, 1, calls.Load())
}

func TestSelectedAccountRefreshCannotResurrectOrReactivate(t *testing.T) {
	for _, action := range []string{"replace", "replace-identical", "logout", "switch", "switch-back", "owner"} {
		t.Run(action, func(t *testing.T) {
			entry := rotationFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var replaced atomic.Bool
			result := make(chan error, 1)
			go func() {
				_, err := RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
					close(started)
					<-release
					return rotatedToken(), nil
				}, func() error {
					if replaced.Load() {
						return errors.New("owner changed")
					}
					return nil
				}, true)
				result <- err
			}()
			<-started
			switch action {
			case "replace":
				require.NoError(t, Save(t.Context(), "rotation", Entry{ID: entry.ID, AccessToken: "manual"}))
			case "replace-identical":
				require.NoError(t, Save(t.Context(), "rotation", entry))
			case "logout":
				require.NoError(t, RemoveProvider(t.Context(), "rotation"))
			case "switch":
				require.NoError(t, Save(t.Context(), "rotation", Entry{ID: "other", AccessToken: "other-token"}))
			case "switch-back":
				require.NoError(t, Save(t.Context(), "rotation", Entry{ID: "other", AccessToken: "other-token"}))
				require.NoError(t, SetActive(t.Context(), "rotation", entry.ID))
			case "owner":
				replaced.Store(true)
			}
			close(release)
			err := <-result
			if action == "owner" {
				require.ErrorContains(t, err, "owner changed")
			} else {
				require.ErrorIs(t, err, ErrCredentialChanged)
			}
			stored, err := Active(t.Context(), "rotation")
			require.NoError(t, err)
			switch action {
			case "replace":
				require.Equal(t, "manual", stored.AccessToken)
			case "logout":
				require.Nil(t, stored)
			case "switch":
				require.Equal(t, "other", stored.ID)
			case "owner", "replace-identical", "switch-back":
				require.Equal(t, entry.AccessToken, stored.AccessToken)
			}
			all, err := List(t.Context(), "rotation")
			require.NoError(t, err)
			for _, account := range all {
				require.NotEqual(t, "synthetic-new", account.AccessToken)
			}
		})
	}
}

func TestSelectedAccountRefreshPersistsRotationAfterCallerDisconnect(t *testing.T) {
	entry := rotationFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	started, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	go func() {
		_, err := RefreshSelectedForOwner(ctx, "rotation", &entry, func(exchangeCtx context.Context, _ string) (*oauth.Token, error) {
			close(started)
			<-release
			if err := exchangeCtx.Err(); err != nil {
				return nil, err
			}
			return rotatedToken(), nil
		}, func() error { return nil }, true)
		result <- err
	}()
	<-started
	cancel()
	close(release)
	require.NoError(t, <-result)
	stored, err := Active(t.Context(), "rotation")
	require.NoError(t, err)
	require.Equal(t, "synthetic-new-refresh", stored.RefreshToken)
	_, err = RefreshSelectedForOwner(ctx, "rotation", stored, func(context.Context, string) (*oauth.Token, error) {
		t.Error("cancelled caller started an exchange")
		return nil, nil
	}, func() error { return nil }, true)
	require.ErrorIs(t, err, context.Canceled)
}
