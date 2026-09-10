package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

const snapshotNamespace = "plugin:exact-snapshot-owner"

func accountSnapshotFixture(t *testing.T) (string, Entry) {
	t.Helper()
	ctx := setup(t)
	entry := Entry{ID: "selected", DisplayName: "Synthetic account", AccessToken: "synthetic-snapshot-access", RefreshToken: "synthetic-snapshot-refresh", Raw: json.RawMessage(`{"private":"synthetic-raw-value"}`)}
	require.NoError(t, Save(ctx, snapshotNamespace, entry))
	path, err := Path()
	require.NoError(t, err)
	stored, err := Active(ctx, snapshotNamespace)
	require.NoError(t, err)
	require.NotNil(t, stored)
	return path, *stored
}

func captureAccountSnapshot(t *testing.T, path string) Snapshot {
	t.Helper()
	snapshot, err := CaptureStateAt(t.Context(), path, []string{snapshotNamespace})
	require.NoError(t, err)
	return snapshot
}

func TestAccountSnapshotStablePrivateAndCloned(t *testing.T) {
	path, entry := accountSnapshotFixture(t)
	require.NoError(t, Save(t.Context(), "unrequested", Entry{ID: "other", AccessToken: "synthetic-hidden"}))
	before := captureAccountSnapshot(t, path)
	after, err := CaptureStateAt(t.Context(), path, []string{snapshotNamespace, snapshotNamespace})
	require.NoError(t, err)
	require.True(t, before.SameObservation(after))
	require.False(t, before.SameObservation(Snapshot{}))
	require.False(t, (Snapshot{}).SameObservation(Snapshot{}))
	require.Equal(t, entry.ID, before.ActiveID(snapshotNamespace))
	require.Equal(t, []Entry{entry}, before.Entries(snapshotNamespace))
	require.Empty(t, before.Entries("unrequested"))
	require.Empty(t, before.ActiveID("unrequested"))
	require.Empty(t, before.Entries("exact-snapshot-owner"), "namespace aliases must not be inferred")
	cloned := before.Entries(snapshotNamespace)
	cloned[0].AccessToken = "changed"
	cloned[0].Raw[0] = '['
	cloned = append(cloned, Entry{ID: "added"})
	require.Len(t, cloned, 2)
	require.Equal(t, []Entry{entry}, before.Entries(snapshotNamespace))
	_, err = json.Marshal(before)
	require.Error(t, err)
	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d"} {
		formatted := fmt.Sprintf(format, before)
		require.Equal(t, "accounts.Snapshot(private)", formatted)
	}

	withBoth, err := CaptureStateAt(t.Context(), path, []string{snapshotNamespace, "unrequested"})
	require.NoError(t, err)
	reordered, err := CaptureStateAt(t.Context(), path, []string{"unrequested", snapshotNamespace})
	require.NoError(t, err)
	require.True(t, withBoth.SameObservation(reordered))
	require.False(t, before.SameObservation(withBoth))
}

func TestAccountSnapshotUsesCapturedAbsolutePath(t *testing.T) {
	path, entry := accountSnapshotFixture(t)
	before := captureAccountSnapshot(t, path)
	otherRoot := filepath.Join(t.TempDir(), "must-not-be-created")
	t.Setenv("AI_CLI_DIR", otherRoot)
	after := captureAccountSnapshot(t, path)
	require.True(t, before.SameObservation(after))
	require.Equal(t, []Entry{entry}, after.Entries(snapshotNamespace))
	_, err := os.Stat(otherRoot)
	require.ErrorIs(t, err, os.ErrNotExist)
	for _, invalid := range []string{"", "accounts.json", "../accounts.json"} {
		_, err := CaptureStateAt(t.Context(), invalid, []string{snapshotNamespace})
		require.ErrorContains(t, err, "absolute database path")
	}
}

func TestAccountSnapshotInvalidatesSupportedChanges(t *testing.T) {
	for _, action := range []string{"same-value-save", "selection-aba", "inactive-addition", "display-name", "unrelated-namespace", "logout-empty"} {
		t.Run(action, func(t *testing.T) {
			path, entry := accountSnapshotFixture(t)
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "second"}))
			before := captureAccountSnapshot(t, path)
			switch action {
			case "same-value-save":
				require.NoError(t, Save(t.Context(), snapshotNamespace, entry))
			case "selection-aba":
				require.NoError(t, SetActive(t.Context(), snapshotNamespace, "second"))
				require.NoError(t, SetActive(t.Context(), snapshotNamespace, entry.ID))
			case "inactive-addition":
				require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "third"}))
			case "display-name":
				// Legacy display-only edits need detection even without counters.
				require.NoError(t, mutateStore(t.Context(), nil, func(state *store) error {
					state.Accounts[snapshotNamespace][0].DisplayName = "Renamed account"
					return nil
				}))
			case "unrelated-namespace":
				require.NoError(t, Save(t.Context(), "unrelated", Entry{ID: "elsewhere"}))
			case "logout-empty":
				require.NoError(t, RemoveProvider(t.Context(), "never-populated"))
			}
			after := captureAccountSnapshot(t, path)
			require.False(t, before.SameObservation(after))
			require.True(t, after.SameObservation(captureAccountSnapshot(t, path)))
		})
	}
}

func TestAccountSnapshotInvalidatesRefreshWithoutMutationCounter(t *testing.T) {
	path, entry := accountSnapshotFixture(t)
	before := captureAccountSnapshot(t, path)
	stateBefore, err := readStore()
	require.NoError(t, err)
	fresh, err := RefreshSelectedForOwner(t.Context(), snapshotNamespace, &entry, func(context.Context, string) (*oauth.Token, error) {
		return rotatedToken(), nil
	}, func() error { return nil }, true)
	require.NoError(t, err)
	stateAfter, err := readStore()
	require.NoError(t, err)
	require.Equal(t, stateBefore.Mutations, stateAfter.Mutations)
	require.NotEmpty(t, stateAfter.Rotations[snapshotNamespace][entry.ID])
	after := captureAccountSnapshot(t, path)
	require.False(t, before.SameObservation(after))
	require.Equal(t, fresh.AccessToken, after.Entries(snapshotNamespace)[0].AccessToken)
}

func TestAccountSnapshotInvalidatesAcrossProcesses(t *testing.T) {
	const childEnv = "CRUX_ACCOUNT_SNAPSHOT_CHILD"
	if action := os.Getenv(childEnv); action != "" {
		entry, err := Active(t.Context(), snapshotNamespace)
		require.NoError(t, err)
		require.NotNil(t, entry)
		switch action {
		case "save":
			require.NoError(t, Save(t.Context(), snapshotNamespace, *entry))
		case "selection-aba":
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, "second"))
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, entry.ID))
		default:
			t.Fatal("unexpected child action")
		}
		return
	}
	path, _ := accountSnapshotFixture(t)
	require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "second"}))
	binary, err := os.Executable()
	require.NoError(t, err)
	for _, action := range []string{"save", "selection-aba"} {
		before := captureAccountSnapshot(t, path)
		cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestAccountSnapshotInvalidatesAcrossProcesses$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"="+action)
		output, err := cmd.CombinedOutput()
		require.NoError(t, err, "%s", output)
		after := captureAccountSnapshot(t, path)
		require.Equal(t, before.Entries(snapshotNamespace), after.Entries(snapshotNamespace))
		require.Equal(t, before.ActiveID(snapshotNamespace), after.ActiveID(snapshotNamespace))
		require.False(t, before.SameObservation(after), action)
	}
}

func TestAccountSnapshotIdenticalReplacementAndPathRecheck(t *testing.T) {
	path, _ := accountSnapshotFixture(t)
	before := captureAccountSnapshot(t, path)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	opened, err := openAccountSnapshotFile(path)
	require.NoError(t, err)
	defer opened.Close()
	observation, err := observeAccountFile(opened)
	require.NoError(t, err)
	replacement := path + ".identical"
	require.NoError(t, os.WriteFile(replacement, data, 0o600))
	require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
	require.NoError(t, os.Rename(replacement, path))
	require.ErrorContains(t, verifyAccountFile(t.Context(), path, opened, observation), "changed during capture")
	after := captureAccountSnapshot(t, path)
	require.Equal(t, before.content, after.content)
	require.Equal(t, before.file.modified, after.file.modified)
	require.NotEqual(t, before.file.identity, after.file.identity)
	require.False(t, before.SameObservation(after))
}

func TestAccountSnapshotMissingExistenceAndInvalidData(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "captured", "accounts.json")
	before := captureAccountSnapshot(t, path)
	require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
	require.Empty(t, before.Entries(snapshotNamespace))
	require.NoError(t, os.WriteFile(path, []byte(`{"active":{},"accounts":{}}`), 0o600))
	present := captureAccountSnapshot(t, path)
	require.False(t, before.SameObservation(present))
	require.NoError(t, os.Remove(path))
	require.False(t, present.SameObservation(captureAccountSnapshot(t, path)))
	require.NoError(t, os.WriteFile(path, []byte(`{"accounts":{"private-namespace":"private-secret"}}`), 0o600))
	_, err := CaptureStateAt(t.Context(), path, []string{snapshotNamespace})
	require.EqualError(t, err, "invalid account database")
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Mkdir(path, 0o700))
	_, err = CaptureStateAt(t.Context(), path, []string{snapshotNamespace})
	require.ErrorContains(t, err, "not a regular file")
}

func TestAccountSnapshotCancellationBeforeAndDuringLocks(t *testing.T) {
	for _, boundary := range []string{"before", "process-mutex", "file-lock"} {
		t.Run(boundary, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "new", "accounts.json")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch boundary {
			case "before":
				cancel()
			case "process-mutex":
				mu.Lock()
				defer mu.Unlock()
			case "file-lock":
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				release, err := lock.File(t.Context(), path+".lock")
				require.NoError(t, err)
				defer release()
			}
			started := make(chan struct{})
			result := make(chan error, 1)
			go func() {
				close(started)
				_, err := CaptureStateAt(ctx, path, []string{snapshotNamespace})
				result <- err
			}()
			<-started
			if boundary != "before" {
				select {
				case err := <-result:
					t.Fatalf("capture did not wait for held lock: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("canceled capture remained blocked behind a held lock")
			}
			_, err := os.Stat(path)
			require.ErrorIs(t, err, os.ErrNotExist)
			if boundary != "file-lock" {
				_, err := os.Stat(filepath.Dir(path))
				require.ErrorIs(t, err, os.ErrNotExist, "cancellation before lock admission must cause no file I/O")
			}
		})
	}
}

func TestAccountSnapshotCancellationAfterLockAdmission(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "accounts.json")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	called := false
	err := withResolvedLock(ctx, func() (string, error) {
		cancel()
		return path + ".lock", nil
	}, func() error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, called)
	_, err = os.Stat(filepath.Dir(path))
	require.True(t, errors.Is(err, os.ErrNotExist))
}
