package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConditionalAccountVerifyCommittedRetainsExactLease(t *testing.T) {
	for _, check := range []bool{false, true} {
		name := "switch"
		if check {
			name = "check"
		}
		t.Run(name, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			var change *PendingChange
			var err error
			if check {
				change, err = before.BeginCheck(t.Context())
			} else {
				change, err = before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			}
			require.NoError(t, err)
			defer change.Close()
			_, err = change.VerifyCommitted(t.Context())
			require.Error(t, err)
			copied := *change
			result, err := copied.Commit(t.Context())
			require.NoError(t, err)
			require.Equal(t, !check, result.Written)
			_, err = change.Commit(t.Context())
			require.ErrorContains(t, err, "already used")
			live := filepath.Join(t.TempDir(), "unused")
			t.Setenv("AI_CLI_DIR", live)
			for range 2 {
				verified, err := change.VerifyCommitted(t.Context())
				require.NoError(t, err)
				require.True(t, result.Snapshot.SameObservation(verified))
				entries := verified.Entries(snapshotNamespace)
				for i := range entries {
					entries[i].AccessToken = "caller mutation"
					if len(entries[i].Raw) > 0 {
						entries[i].Raw[0] = '['
					}
				}
			}
			assertConditionalLeaseHeld(t, path)
			_, err = os.Stat(live)
			require.ErrorIs(t, err, os.ErrNotExist)
			var wait sync.WaitGroup
			for range 4 {
				wait.Go(change.Close)
				wait.Go(copied.Close)
			}
			wait.Wait()
			for _, handle := range []*PendingChange{change, &copied, nil, {}} {
				verified, err := handle.VerifyCommitted(t.Context())
				require.Error(t, err)
				require.False(t, verified.valid)
			}
			require.False(t, change.state.verified.valid)
			require.True(t, result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
		})
	}
}

func TestConditionalAccountVerifyCommittedRejectsDriftWithoutAdoption(t *testing.T) {
	for _, drift := range []string{"body", "metadata", "same bytes replacement", "removed", "invalid document"} {
		t.Run(drift, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			require.NoError(t, err)
			defer change.Close()
			result, err := change.Commit(t.Context())
			require.NoError(t, err)
			data := readConditionalDocument(t, path)
			switch drift {
			case "body":
				require.NoError(t, os.WriteFile(path, append(data, '\n'), 0o600))
			case "metadata":
				changed := time.Now().Add(time.Hour)
				require.NoError(t, os.Chtimes(path, changed, changed))
			case "same bytes replacement":
				replacement := path + ".replacement"
				require.NoError(t, os.WriteFile(replacement, data, 0o600))
				require.NoError(t, os.Rename(replacement, path))
			case "removed":
				require.NoError(t, os.Remove(path))
			case "invalid document":
				require.NoError(t, os.WriteFile(path, []byte(`{"synthetic-secret":`), 0o600))
			}
			for range 2 {
				verified, err := change.VerifyCommitted(t.Context())
				require.Error(t, err)
				require.False(t, verified.valid)
				require.NotContains(t, err.Error(), path)
				require.NotContains(t, err.Error(), "synthetic-secret")
				require.True(t, result.Snapshot.SameObservation(change.state.verified), "verification must never bless drift")
			}
		})
	}
	for _, exists := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing check", true: "existing check"}[exists], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			if exists {
				require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
			}
			before := captureAccountSnapshot(t, path)
			change, err := before.BeginCheck(t.Context())
			require.NoError(t, err)
			defer change.Close()
			result, err := change.Commit(t.Context())
			require.NoError(t, err)
			require.False(t, result.Written)
			require.NoError(t, os.WriteFile(path, []byte("{\n}"), 0o600))
			verified, err := change.VerifyCommitted(t.Context())
			require.ErrorIs(t, err, ErrStateChanged)
			require.False(t, verified.valid)
		})
	}
}

// Cancel immediately after the last pre-rename check has observed no error.
// This deterministically exercises cancellation between that check and the
// subsequent post-write capture without changing the production rename path.
type afterFinalAccountCheckContext struct {
	context.Context
	armed  bool
	cancel context.CancelFunc
}

func (ctx *afterFinalAccountCheckContext) Err() error {
	err := ctx.Context.Err()
	if ctx.armed {
		ctx.armed = false
		ctx.cancel()
	}
	return err
}

func TestConditionalAccountCommitCancellationBoundary(t *testing.T) {
	for _, boundary := range []string{"staging", "validator", "after final check"} {
		t.Run(boundary, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			original := readConditionalDocument(t, path)
			change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			require.NoError(t, err)
			defer change.Close()
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			ctx := base
			switch boundary {
			case "staging":
				ctx = &duringAccountStagingContext{Context: base, path: path, action: cancel}
			case "validator":
				change.state.validate = func() error { cancel(); return nil }
			case "after final check":
				last := &afterFinalAccountCheckContext{Context: base, cancel: cancel}
				ctx = last
				change.state.validate = func() error { last.armed = true; return nil }
			}
			result, err := change.Commit(ctx)
			require.ErrorIs(t, base.Err(), context.Canceled)
			if boundary == "after final check" {
				require.NoError(t, err)
				require.True(t, result.Written)
				verified, err := change.VerifyCommitted(t.Context())
				require.NoError(t, err)
				require.True(t, result.Snapshot.SameObservation(verified))
				require.Equal(t, "second", verified.ActiveID(snapshotNamespace))
			} else {
				require.ErrorIs(t, err, context.Canceled)
				require.False(t, result.Written)
				require.Equal(t, original, readConditionalDocument(t, path))
				_, err = change.VerifyCommitted(t.Context())
				require.Error(t, err)
			}
			temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".accounts-change-*"))
			require.NoError(t, err)
			require.Empty(t, temporary)
		})
	}
}

func TestConditionalAccountCommitCompletionDeadline(t *testing.T) {
	path, _, before := conditionalAccountFixture(t)
	change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
	require.NoError(t, err)
	defer change.Close()
	change.state.validate = func() error {
		file, err := openAccountSnapshotFile(path)
		if err != nil {
			return err
		}
		observed, err := observeAccountFile(file)
		_ = file.Close()
		if err == nil && observed != before.file {
			// Simulate a slow synchronous validator. Commit cannot interrupt it,
			// but must not publish a successful snapshot after its deadline.
			time.Sleep(accountCommitCompletionTimeout + 10*time.Millisecond)
		}
		return err
	}
	result, err := change.Commit(t.Context())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, result.Written)
	require.False(t, result.Snapshot.valid)
	_, err = change.VerifyCommitted(t.Context())
	require.Error(t, err)
	var saved store
	require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &saved))
	require.Equal(t, "second", saved.Active[snapshotNamespace])
}

func TestConditionalAccountVerifyCommittedCancellation(t *testing.T) {
	_, _, before := conditionalAccountFixture(t)
	change, err := before.BeginCheck(t.Context())
	require.NoError(t, err)
	defer change.Close()
	result, err := change.Commit(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	verified, err := change.VerifyCommitted(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, verified.valid)
	verified, err = change.VerifyCommitted(t.Context())
	require.NoError(t, err)
	require.True(t, result.Snapshot.SameObservation(verified))
}
