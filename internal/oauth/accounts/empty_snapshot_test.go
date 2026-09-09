package accounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmptySnapshotOnlyChecksWithoutAccountStoreAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	t.Setenv("AI_CLI_DIR", root)
	t.Setenv("HOME", root)
	before, err := EmptySnapshot(t.Context())
	require.NoError(t, err)
	after, err := EmptySnapshot(t.Context())
	require.NoError(t, err)
	require.True(t, before.SameObservation(after))
	require.False(t, before.SameObservation(Snapshot{}))
	pending, err := before.BeginCheck(t.Context())
	require.NoError(t, err)
	committed, err := pending.Commit(t.Context())
	require.NoError(t, err)
	require.False(t, committed.Written)
	require.True(t, before.SameObservation(committed.Snapshot))
	verified, err := pending.VerifyCommitted(t.Context())
	require.NoError(t, err)
	require.True(t, before.SameObservation(verified))
	_, err = pending.Commit(t.Context())
	require.Error(t, err)
	pending.Close()
	_, err = pending.VerifyCommitted(t.Context())
	require.Error(t, err)
	_, err = before.beginChange(t.Context(), accountRefresh, "invented", "invented")
	require.Error(t, err)
	_, err = before.RefreshSelectedForOwner(t.Context(), "invented", &Entry{ID: "invented"}, nil, inactiveOwnerValid, true)
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = EmptySnapshot(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = before.BeginCheck(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = os.Stat(root)
	require.True(t, os.IsNotExist(err))
}
