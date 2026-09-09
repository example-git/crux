package accounts

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func selectedSnapshotFixture(t *testing.T) (Entry, Snapshot) {
	t.Helper()
	entry := rotationFixture(t)
	before, err := CaptureStateAt(t.Context(), filepath.Join(os.Getenv("AI_CLI_DIR"), "accounts.json"), []string{"rotation"})
	require.NoError(t, err)
	return entry, before
}

func TestSelectedSnapshotRefreshPreservesMutationAndSelectionFences(t *testing.T) {
	for _, action := range []string{"replace", "replace-identical", "logout", "switch", "switch-back", "owner"} {
		t.Run(action, func(t *testing.T) {
			entry, before := selectedSnapshotFixture(t)
			var revoked bool
			validate := func() error {
				if revoked {
					return errors.New("owner changed")
				}
				return nil
			}
			fresh, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
				switch action {
				case "replace":
					require.NoError(t, Save(t.Context(), "rotation", Entry{ID: entry.ID, AccessToken: "manual"}))
				case "replace-identical":
					require.NoError(t, Save(t.Context(), "rotation", entry))
				case "logout":
					require.NoError(t, RemoveProvider(t.Context(), "rotation"))
				case "switch", "switch-back":
					require.NoError(t, Save(t.Context(), "rotation", Entry{ID: "other", AccessToken: "other"}))
					if action == "switch-back" {
						require.NoError(t, SetActive(t.Context(), "rotation", entry.ID))
					}
				case "owner":
					revoked = true
				}
				return rotatedToken(), nil
			}, validate, true)
			require.Nil(t, fresh)
			if action == "owner" {
				require.ErrorContains(t, err, "owner changed")
			} else {
				require.ErrorIs(t, err, ErrCredentialChanged)
			}
			after, err := CaptureStateAt(t.Context(), before.path, []string{"rotation"})
			require.NoError(t, err)
			for _, account := range after.Entries("rotation") {
				require.NotEqual(t, "synthetic-new", account.AccessToken)
			}
		})
	}
}

func TestSelectedSnapshotCancellationKeepsConsumedRotationAndCapturedCommit(t *testing.T) {
	entry, before := selectedSnapshotFixture(t)
	ambient := filepath.Join(t.TempDir(), "uncreated")
	t.Setenv("AI_CLI_DIR", ambient)
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	refresh := func(context.Context, string) (*oauth.Token, error) {
		calls.Add(1)
		close(started)
		<-release
		return rotatedToken(), nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		_, err := before.RefreshSelectedForOwner(ctx, "rotation", &entry, refresh, inactiveOwnerValid, true)
		done <- err
	}()
	<-started
	cancel()
	_, err := before.RefreshSelectedForOwner(ctx, "rotation", &entry, refresh, inactiveOwnerValid, true)
	require.ErrorIs(t, err, context.Canceled)
	close(release)
	require.NoError(t, <-done, "caller disconnection must not discard a consumed rotation")
	adopted, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, refresh, inactiveOwnerValid, true)
	require.NoError(t, err)
	require.Equal(t, "synthetic-new-refresh", adopted.RefreshToken)
	require.EqualValues(t, 1, calls.Load())
	commits := 0
	require.NoError(t, before.WithSelectedForOwner(t.Context(), "rotation", *adopted, inactiveOwnerValid, func() error { commits++; return nil }))
	require.Equal(t, 1, commits)
	require.ErrorIs(t, before.WithSelectedForOwner(t.Context(), "rotation", entry, inactiveOwnerValid, func() error { commits++; return nil }), ErrCredentialChanged)
	require.ErrorIs(t, before.WithSelectedForOwner(ctx, "rotation", *adopted, inactiveOwnerValid, func() error { commits++; return nil }), context.Canceled)
	require.Equal(t, 1, commits)
	_, err = os.Stat(ambient)
	require.True(t, os.IsNotExist(err), "captured refresh and local commit must create no ambient state")
}

func TestSelectedSnapshotRequiresCapturedNamespaceAndOwner(t *testing.T) {
	entry, before := selectedSnapshotFixture(t)
	ambient := filepath.Join(t.TempDir(), "uncreated")
	t.Setenv("AI_CLI_DIR", ambient)
	for _, invalid := range []Snapshot{{}, before} {
		_, err := invalid.RefreshSelectedForOwner(t.Context(), "uncaptured", &entry, nil, inactiveOwnerValid, true)
		require.ErrorContains(t, err, "captured database and namespace")
		require.ErrorContains(t, invalid.WithSelectedForOwner(t.Context(), "uncaptured", entry, inactiveOwnerValid, func() error { t.Fatal("invalid snapshot committed"); return nil }), "captured database and namespace")
	}
	_, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, nil, nil, true)
	require.ErrorContains(t, err, "owner validator")
	require.ErrorContains(t, before.WithSelectedForOwner(t.Context(), "rotation", entry, nil, func() error { t.Fatal("unowned snapshot committed"); return nil }), "owner validation")
	_, err = os.Stat(ambient)
	require.True(t, os.IsNotExist(err))
}

func TestSelectedSnapshotReportsSavedRotationAfterFinalOwnerFailure(t *testing.T) {
	entry, before := selectedSnapshotFixture(t)
	ownerErr := errors.New("owner detached after account write")
	fresh, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
		return rotatedToken(), nil
	}, func() error {
		data, err := os.ReadFile(before.path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, []byte("synthetic-new-refresh")) {
			return ownerErr
		}
		return nil
	}, true)
	require.ErrorIs(t, err, ownerErr)
	require.NotNil(t, fresh, "a post-write failure must retain the saved successor")
	after, err := CaptureStateAt(t.Context(), before.path, []string{"rotation"})
	require.NoError(t, err)
	require.Equal(t, CredentialID(*fresh), CredentialID(after.Entries("rotation")[0]))
}

func TestSelectedSnapshotFilesystemErrorsKeepCapturedPathsPrivate(t *testing.T) {
	entry, before := selectedSnapshotFixture(t)
	root := filepath.Dir(before.path)
	require.NoError(t, os.WriteFile(filepath.Join(root, "locks"), []byte("blocked"), 0600))
	_, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, nil, inactiveOwnerValid, true)
	require.Error(t, err)
	require.NotContains(t, err.Error(), root)
	err = before.WithSelectedForOwner(t.Context(), "rotation", entry, inactiveOwnerValid, func() error {
		return &os.PathError{Op: "write", Path: before.path, Err: os.ErrPermission}
	})
	require.ErrorIs(t, err, os.ErrPermission)
	require.NotContains(t, err.Error(), root)
}

func TestSelectedSnapshotRejectsUnwritableStructureBeforeExchange(t *testing.T) {
	for _, malformed := range []string{"duplicate-account", "ambiguous-root", "ambiguous-entry"} {
		t.Run(malformed, func(t *testing.T) {
			entry, before := selectedSnapshotFixture(t)
			document, err := os.ReadFile(before.path)
			require.NoError(t, err)
			switch malformed {
			case "duplicate-account":
				document, err = sjson.SetRawBytes(document, "accounts.rotation.-1", []byte(gjson.GetBytes(document, "accounts.rotation.0").Raw))
			case "ambiguous-root":
				document, err = sjson.SetRawBytes(document, "ACCOUNTS", []byte(gjson.GetBytes(document, "accounts").Raw))
			case "ambiguous-entry":
				document, err = sjson.SetBytes(document, "accounts.rotation.0.AccessToken", entry.AccessToken)
			}
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(before.path, document, 0600))
			calls := 0
			fresh, err := before.RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
				calls++
				return rotatedToken(), nil
			}, inactiveOwnerValid, true)
			require.Error(t, err)
			require.Nil(t, fresh)
			require.Zero(t, calls, "structural validation must precede consuming a rotating token")
			after, err := os.ReadFile(before.path)
			require.NoError(t, err)
			require.Equal(t, document, after)
		})
	}
}
