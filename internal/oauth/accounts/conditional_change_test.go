package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func conditionalAccountFixture(t *testing.T) (string, Entry, Snapshot) {
	t.Helper()
	path, first := accountSnapshotFixture(t)
	require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "second", AccessToken: "synthetic-second", Raw: json.RawMessage(`{"number":1.0}`)}))
	return path, first, captureAccountSnapshot(t, path)
}

func readConditionalDocument(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func decodeConditionalObject(t *testing.T, data []byte) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(data, &object))
	return object
}

func assertConditionalLeaseHeld(t *testing.T, path string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	_, err := CaptureStateAt(ctx, path, []string{snapshotNamespace})
	require.ErrorIs(t, err, context.DeadlineExceeded, "lease must retain the in-process account mutex")
	release, err := lock.TryFile(path + ".lock")
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, lock.ErrContended, "lease must retain the cross-process account flock")
}

func TestConditionalAccountSwitchCommitsExactTargetAndRetainsLease(t *testing.T) {
	path, _, before := conditionalAccountFixture(t)
	original := readConditionalDocument(t, path)
	change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
	require.NoError(t, err)
	defer change.Close()
	require.Equal(t, original, readConditionalDocument(t, path), "Begin only stages the fixed change")
	entry, ok := change.SelectedEntry()
	require.True(t, ok)
	require.Equal(t, "second", entry.ID)
	entry.Raw[0] = '['
	entry.AccessToken = "caller mutation"
	unchanged, ok := change.SelectedEntry()
	require.True(t, ok)
	require.Equal(t, "synthetic-second", unchanged.AccessToken)
	require.JSONEq(t, `{"number":1.0}`, string(unchanged.Raw))
	assertConditionalLeaseHeld(t, path)
	result, err := change.Commit(t.Context())
	require.NoError(t, err)
	require.True(t, result.Written)
	require.Equal(t, "second", result.Snapshot.ActiveID(snapshotNamespace))
	require.Len(t, result.Snapshot.Entries(snapshotNamespace), 2)
	require.False(t, before.SameObservation(result.Snapshot))
	assertConditionalLeaseHeld(t, path)
	_, err = change.Commit(t.Context())
	require.ErrorContains(t, err, "already used")
	change.Close()
	change.Close()
	_, ok = change.SelectedEntry()
	require.False(t, ok)
	require.True(t, result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".accounts-change-*"))
	require.NoError(t, err)
	require.Empty(t, temporary)
}

func TestConditionalAccountCheckAndCloseNeverWrite(t *testing.T) {
	for _, operation := range []string{"check", "switch close", "logout close"} {
		t.Run(operation, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			original := readConditionalDocument(t, path)
			var change *PendingChange
			var err error
			switch operation {
			case "check":
				change, err = before.BeginCheck(t.Context())
			case "switch close":
				change, err = before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			case "logout close":
				change, err = before.BeginLogout(t.Context(), snapshotNamespace)
			}
			require.NoError(t, err)
			defer change.Close()
			if operation == "check" {
				_, ok := change.SelectedEntry()
				require.False(t, ok)
				result, err := change.Commit(t.Context())
				require.NoError(t, err)
				require.False(t, result.Written)
				require.True(t, before.SameObservation(result.Snapshot))
			}
			change.Close()
			require.Equal(t, original, readConditionalDocument(t, path))
			require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
			_, err = change.Commit(t.Context())
			require.Error(t, err)
		})
	}
	t.Run("missing check", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "accounts.json")
		before := captureAccountSnapshot(t, path)
		change, err := before.BeginCheck(t.Context())
		require.NoError(t, err)
		defer change.Close()
		result, err := change.Commit(t.Context())
		require.NoError(t, err)
		require.False(t, result.Written)
		require.True(t, before.SameObservation(result.Snapshot))
		_, err = os.Stat(path)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestConditionalAccountCopiedHandlesShareOneLease(t *testing.T) {
	path, _, before := conditionalAccountFixture(t)
	change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
	require.NoError(t, err)
	defer change.Close()
	copied := *change
	result, err := copied.Commit(t.Context())
	require.NoError(t, err)
	require.True(t, result.Written)
	_, err = change.Commit(t.Context())
	require.ErrorContains(t, err, "already used")
	assertConditionalLeaseHeld(t, path)
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(change.Close)
		wait.Go(copied.Close)
	}
	wait.Wait()
	_, ok := copied.SelectedEntry()
	require.False(t, ok)
	_, ok = change.SelectedEntry()
	require.False(t, ok)
	_, err = copied.Commit(t.Context())
	require.ErrorContains(t, err, "closed")
	require.True(t, result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
}

func TestConditionalAccountChangesPreserveForeignDataAndCounterHistory(t *testing.T) {
	const document = `{
  "foreign_harness": { "exact": 9007199254740993, "spelling": 1.00e+0, "label": "preserve <this>" },
  "active": { "owner.ns": "first", "other": "elsewhere" },
  "accounts": {
    "owner.ns": [{"id":"first","accessToken":"one","foreign_entry":{"retain":true}}, {"id":"second","accessToken":"two","raw":{"number":1.0},"foreign_entry":[1e0]}],
    "other": [{ "id":"elsewhere", "accessToken":"other", "foreign_entry": {"exact": 9007199254740993} }]
  },
  "mutations": {"owner.ns":{"first":4,"second":8,"removed-long-ago":12},"other":{"elsewhere":9}},
  "selections": {"owner.ns":6,"other":7},
  "rotations": {"owner.ns":{"first":[{"Before":"before","After":"after"}]},"other":{"elsewhere":[]}}
}`
	for _, operation := range []string{"switch", "same selected", "logout"} {
		t.Run(operation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
			before, err := CaptureStateAt(t.Context(), path, []string{"owner.ns"})
			require.NoError(t, err)
			var change *PendingChange
			switch operation {
			case "switch":
				change, err = before.BeginSwitch(t.Context(), "owner.ns", "second")
			case "same selected":
				change, err = before.BeginSwitch(t.Context(), "owner.ns", "first")
			case "logout":
				change, err = before.BeginLogout(t.Context(), "owner.ns")
			}
			require.NoError(t, err)
			defer change.Close()
			result, err := change.Commit(t.Context())
			require.NoError(t, err)
			require.True(t, result.Written)
			after := decodeConditionalObject(t, readConditionalDocument(t, path))
			original := decodeConditionalObject(t, []byte(document))
			require.Equal(t, original["foreign_harness"], after["foreign_harness"], "foreign top-level data must remain byte-for-byte intact")
			for _, field := range []string{"active", "accounts", "mutations", "selections", "rotations"} {
				require.Equal(t, decodeConditionalObject(t, original[field])["other"], decodeConditionalObject(t, after[field])["other"], "unrelated %s namespace was changed", field)
			}
			var actual store
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &actual))
			if operation == "logout" {
				require.Empty(t, actual.Accounts["owner.ns"])
				require.Empty(t, actual.Active["owner.ns"])
				require.NotContains(t, actual.Rotations, "owner.ns")
				require.Equal(t, map[string]uint64{"first": 5, "second": 9, "removed-long-ago": 12}, actual.Mutations["owner.ns"])
				require.EqualValues(t, 7, actual.Selections["owner.ns"])
			} else {
				require.Equal(t, original["accounts"], after["accounts"], "switch must preserve all entry data, including foreign fields and Raw number spelling")
				require.Equal(t, original["mutations"], after["mutations"])
				require.Equal(t, original["rotations"], after["rotations"])
				if operation == "same selected" {
					require.EqualValues(t, 6, actual.Selections["owner.ns"])
					require.False(t, before.SameObservation(result.Snapshot), "same-ID switch must retain existing rewrite behavior")
				} else {
					require.Equal(t, "second", actual.Active["owner.ns"])
					require.EqualValues(t, 7, actual.Selections["owner.ns"])
				}
			}
		})
	}
}

func TestConditionalAccountExactPathKeysAndEmptyLogout(t *testing.T) {
	for _, namespace := range []string{"0", "a.b\\:#@?*|", "other"} {
		t.Run(namespace, func(t *testing.T) {
			setup(t)
			path, err := Path()
			require.NoError(t, err)
			id := "0.a\\:#@?*|"
			require.NoError(t, Save(t.Context(), namespace, Entry{ID: "first"}))
			require.NoError(t, SaveWithoutActivating(t.Context(), namespace, Entry{ID: id}))
			before, err := CaptureStateAt(t.Context(), path, []string{namespace})
			require.NoError(t, err)
			change, err := before.BeginSwitch(t.Context(), namespace, id)
			require.NoError(t, err)
			result, err := change.Commit(t.Context())
			change.Close()
			require.NoError(t, err)
			require.Equal(t, id, result.Snapshot.ActiveID(namespace))
			change, err = result.Snapshot.BeginLogout(t.Context(), namespace)
			require.NoError(t, err)
			result, err = change.Commit(t.Context())
			change.Close()
			require.NoError(t, err)
			require.Empty(t, result.Snapshot.Entries(namespace))
			var state store
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
			require.EqualValues(t, 2, state.Mutations[namespace][id])
			selection := state.Selections[namespace]
			change, err = result.Snapshot.BeginLogout(t.Context(), namespace)
			require.NoError(t, err)
			_, err = change.Commit(t.Context())
			change.Close()
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
			require.Equal(t, selection+1, state.Selections[namespace], "empty logout still invalidates earlier intent")
		})
	}
}

func TestConditionalAccountEmptyDocumentsCaseFieldsAndCounterBoundaries(t *testing.T) {
	for _, document := range []string{"", "null", `{"active":null,"accounts":null,"mutations":null,"rotations":null,"selections":null}`} {
		t.Run("empty-"+document, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "accounts.json")
			if document != "" {
				require.NoError(t, os.WriteFile(path, []byte(document), 0o600))
			}
			before := captureAccountSnapshot(t, path)
			change, err := before.BeginLogout(t.Context(), snapshotNamespace)
			require.NoError(t, err)
			result, err := change.Commit(t.Context())
			change.Close()
			require.NoError(t, err)
			require.True(t, result.Written)
			var state store
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
			require.EqualValues(t, 1, state.Selections[snapshotNamespace])
			require.Empty(t, state.Accounts)
		})
	}
	t.Run("preserve known field case", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "accounts.json")
		document := []byte(`{"ACTIVE":{"owner":"first"},"ACCOUNTS":{"owner":[{"id":"first"},{"id":"second"}]},"Selections":{"owner":1},"Foreign":{"keep":1.0}}`)
		require.NoError(t, os.WriteFile(path, document, 0o600))
		before, err := CaptureStateAt(t.Context(), path, []string{"owner"})
		require.NoError(t, err)
		change, err := before.BeginSwitch(t.Context(), "owner", "second")
		require.NoError(t, err)
		_, err = change.Commit(t.Context())
		change.Close()
		require.NoError(t, err)
		fields := decodeConditionalObject(t, readConditionalDocument(t, path))
		require.Contains(t, fields, "ACTIVE")
		require.NotContains(t, fields, "active")
		require.Equal(t, decodeConditionalObject(t, document)["ACCOUNTS"], fields["ACCOUNTS"])
	})
	for _, operation := range []string{"same-ID max selection", "logout max selection", "unused max tombstone"} {
		t.Run(operation, func(t *testing.T) {
			path, first, _ := conditionalAccountFixture(t)
			var state store
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
			if operation == "unused max tombstone" {
				state.Mutations[snapshotNamespace]["removed"] = math.MaxUint64
			} else {
				state.Selections[snapshotNamespace] = math.MaxUint64
			}
			data, err := json.Marshal(state)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(path, data, 0o600))
			before := captureAccountSnapshot(t, path)
			var change *PendingChange
			if operation == "same-ID max selection" {
				change, err = before.BeginSwitch(t.Context(), snapshotNamespace, first.ID)
			} else {
				change, err = before.BeginLogout(t.Context(), snapshotNamespace)
			}
			if operation == "logout max selection" {
				require.ErrorContains(t, err, "counter exhausted")
				require.Equal(t, data, readConditionalDocument(t, path))
				return
			}
			require.NoError(t, err)
			result, err := change.Commit(t.Context())
			change.Close()
			require.NoError(t, err)
			require.True(t, result.Written)
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
			if operation == "same-ID max selection" {
				require.EqualValues(t, uint64(math.MaxUint64), state.Selections[snapshotNamespace])
			} else {
				require.EqualValues(t, uint64(math.MaxUint64), state.Mutations[snapshotNamespace]["removed"])
			}
		})
	}
}

func TestConditionalAccountRejectsStaleAcrossProcesses(t *testing.T) {
	const childEnv = "CRUX_CONDITIONAL_ACCOUNT_CHILD"
	if action := os.Getenv(childEnv); action != "" {
		entry, err := Active(t.Context(), snapshotNamespace)
		require.NoError(t, err)
		require.NotNil(t, entry)
		switch action {
		case "same save":
			require.NoError(t, Save(t.Context(), snapshotNamespace, *entry))
		case "selection ABA":
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, "second"))
			require.NoError(t, SetActive(t.Context(), snapshotNamespace, entry.ID))
		case "inactive addition":
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "third"}))
		case "other namespace":
			require.NoError(t, Save(t.Context(), "other", Entry{ID: "other"}))
		}
		return
	}
	for _, action := range []string{"same save", "selection ABA", "inactive addition", "other namespace"} {
		t.Run(action, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			binary, err := os.Executable()
			require.NoError(t, err)
			cmd := exec.CommandContext(t.Context(), binary, "-test.run=^TestConditionalAccountRejectsStaleAcrossProcesses$", "-test.count=1")
			cmd.Env = append(os.Environ(), childEnv+"="+action)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", output)
			assertConditionalStale(t, path, before)
		})
	}
}

func assertConditionalStale(t *testing.T, path string, before Snapshot) {
	t.Helper()
	unchanged := captureAccountSnapshot(t, path)
	original := readConditionalDocument(t, path)
	for _, begin := range []func() (*PendingChange, error){
		func() (*PendingChange, error) { return before.BeginSwitch(t.Context(), snapshotNamespace, "second") },
		func() (*PendingChange, error) { return before.BeginLogout(t.Context(), snapshotNamespace) },
		func() (*PendingChange, error) { return before.BeginCheck(t.Context()) },
	} {
		change, err := begin()
		if change != nil {
			change.Close()
		}
		require.ErrorIs(t, err, ErrStateChanged)
		require.Nil(t, change)
	}
	require.Equal(t, original, readConditionalDocument(t, path))
	require.True(t, unchanged.SameObservation(captureAccountSnapshot(t, path)), "stale requests must cause zero account writes")
}

func TestConditionalAccountRejectsRefreshDisplayAndReplacement(t *testing.T) {
	for _, action := range []string{"refresh", "display", "identical replacement"} {
		t.Run(action, func(t *testing.T) {
			path, entry, before := conditionalAccountFixture(t)
			switch action {
			case "refresh":
				_, err := RefreshSelectedForOwner(t.Context(), snapshotNamespace, &entry, func(context.Context, string) (*oauth.Token, error) { return rotatedToken(), nil }, func() error { return nil }, true)
				require.NoError(t, err)
			case "display":
				require.NoError(t, mutateStore(t.Context(), nil, func(state *store) error {
					state.Accounts[snapshotNamespace][0].DisplayName = "Changed display"
					return nil
				}))
			case "identical replacement":
				info, err := os.Stat(path)
				require.NoError(t, err)
				replacement := path + ".replacement"
				require.NoError(t, os.WriteFile(replacement, readConditionalDocument(t, path), 0o600))
				require.NoError(t, os.Chtimes(replacement, info.ModTime(), info.ModTime()))
				require.NoError(t, os.Rename(replacement, path))
			}
			assertConditionalStale(t, path, before)
		})
	}
}

func TestConditionalAccountRejectsInvalidTargetsAndOverflowBeforeWrite(t *testing.T) {
	for _, mode := range []string{"namespace", "empty namespace", "missing target", "duplicate target", "selection overflow", "mutation overflow", "duplicate field", "case collision"} {
		t.Run(mode, func(t *testing.T) {
			path, _, _ := conditionalAccountFixture(t)
			data := readConditionalDocument(t, path)
			var state store
			require.NoError(t, json.Unmarshal(data, &state))
			switch mode {
			case "duplicate target":
				state.Accounts[snapshotNamespace] = append(state.Accounts[snapshotNamespace], Entry{ID: "second"})
			case "selection overflow":
				state.Selections[snapshotNamespace] = math.MaxUint64
			case "mutation overflow":
				state.Mutations[snapshotNamespace]["second"] = math.MaxUint64
			}
			if mode == "duplicate target" || mode == "selection overflow" || mode == "mutation overflow" {
				data, _ = json.Marshal(state)
			} else if mode == "duplicate field" {
				data = append([]byte(`{"active":{},`), bytes.TrimSpace(data)[1:]...)
			} else if mode == "case collision" {
				data = append([]byte(`{"ACTIVE":{},`), bytes.TrimSpace(data)[1:]...)
			}
			require.NoError(t, os.WriteFile(path, data, 0o600))
			before := captureAccountSnapshot(t, path)
			var change *PendingChange
			var err error
			switch mode {
			case "namespace":
				change, err = before.BeginLogout(t.Context(), "other")
			case "empty namespace":
				change, err = before.BeginLogout(t.Context(), "")
			case "missing target":
				change, err = before.BeginSwitch(t.Context(), snapshotNamespace, "missing")
			case "mutation overflow":
				change, err = before.BeginLogout(t.Context(), snapshotNamespace)
			default:
				change, err = before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			}
			if change != nil {
				change.Close()
			}
			require.Error(t, err)
			require.Nil(t, change)
			require.Equal(t, data, readConditionalDocument(t, path))
			require.True(t, before.SameObservation(captureAccountSnapshot(t, path)))
		})
	}
	_, err := (Snapshot{}).BeginCheck(t.Context())
	require.Error(t, err)
}

func TestConditionalAccountCancellationAndCapturedPath(t *testing.T) {
	for _, boundary := range []string{"before", "mutex", "flock", "commit"} {
		t.Run(boundary, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			original := readConditionalDocument(t, path)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if boundary == "commit" {
				change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
				require.NoError(t, err)
				defer change.Close()
				cancel()
				result, err := change.Commit(ctx)
				require.ErrorIs(t, err, context.Canceled)
				require.False(t, result.Written)
				require.Equal(t, original, readConditionalDocument(t, path))
				assertConditionalLeaseHeld(t, path)
				return
			}
			if boundary == "before" {
				cancel()
			} else if boundary == "mutex" {
				mu.Lock()
				defer mu.Unlock()
			} else {
				release, err := lock.File(t.Context(), path+".lock")
				require.NoError(t, err)
				defer release()
			}
			result := make(chan error, 1)
			go func() {
				change, err := before.BeginCheck(ctx)
				if change != nil {
					change.Close()
				}
				result <- err
			}()
			if boundary != "before" {
				select {
				case err := <-result:
					t.Fatalf("lease did not wait for held lock: %v", err)
				case <-time.After(25 * time.Millisecond):
				}
				cancel()
			}
			select {
			case err := <-result:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(time.Second):
				t.Fatal("canceled account lease remained blocked")
			}
			require.Equal(t, original, readConditionalDocument(t, path))
		})
	}
	t.Run("captured path", func(t *testing.T) {
		path, _, before := conditionalAccountFixture(t)
		live := filepath.Join(t.TempDir(), "unused")
		t.Setenv("AI_CLI_DIR", live)
		change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
		require.NoError(t, err)
		result, err := change.Commit(t.Context())
		change.Close()
		require.NoError(t, err)
		require.Equal(t, "second", result.Snapshot.ActiveID(snapshotNamespace))
		require.True(t, result.Snapshot.SameObservation(captureAccountSnapshot(t, path)))
		_, err = os.Stat(live)
		require.ErrorIs(t, err, os.ErrNotExist)
	})
}

func TestConditionalAccountCommitReportsPostRenameOutcomes(t *testing.T) {
	for _, failure := range []string{"cancellation", "invalid document"} {
		t.Run(failure, func(t *testing.T) {
			path, _, before := conditionalAccountFixture(t)
			change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
			require.NoError(t, err)
			defer change.Close()
			base, cancel := context.WithCancel(t.Context())
			defer cancel()
			change.state.validate = func() error {
				file, err := openAccountSnapshotFile(path)
				if err != nil {
					return err
				}
				observed, err := observeAccountFile(file)
				_ = file.Close()
				if err != nil || observed == before.file {
					return err
				}
				if failure == "cancellation" {
					cancel()
				} else {
					require.NoError(t, os.WriteFile(path, []byte("{invalid"), 0o600))
				}
				return nil
			}
			result, err := change.Commit(base)
			require.True(t, result.Written, "the successful rename must remain observable")
			assertConditionalLeaseHeld(t, path)
			if failure == "cancellation" {
				require.NoError(t, err)
				require.ErrorIs(t, base.Err(), context.Canceled)
				verified, err := change.VerifyCommitted(t.Context())
				require.NoError(t, err)
				require.True(t, result.Snapshot.SameObservation(verified))
				var state store
				require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &state))
				require.Equal(t, "second", state.Active[snapshotNamespace])
			} else {
				require.Error(t, err)
				require.False(t, result.Snapshot.valid)
				_, err = change.VerifyCommitted(t.Context())
				require.Error(t, err)
			}
		})
	}
}

func TestConditionalAccountCommitRechecksPathAndPrivateFormatting(t *testing.T) {
	path, _, before := conditionalAccountFixture(t)
	change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
	require.NoError(t, err)
	defer change.Close()
	for _, value := range []any{change, *change, change.state, CommitResult{Snapshot: before, Written: true}} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%d", "%f"} {
			formatted := fmt.Sprintf(format, value)
			require.Contains(t, formatted, "(private)")
			require.NotContains(t, formatted, "synthetic")
			require.NotContains(t, formatted, snapshotNamespace)
			require.NotContains(t, formatted, path)
		}
	}
	replacement := []byte(`{"active":{},"accounts":{},"foreign":"new writer"}`)
	require.NoError(t, os.WriteFile(path, replacement, 0o600))
	result, err := change.Commit(t.Context())
	require.ErrorIs(t, err, ErrStateChanged)
	require.False(t, result.Written)
	require.Equal(t, replacement, readConditionalDocument(t, path))
	change.Close()
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(change.Close)
	}
	wait.Wait()
	var zero PendingChange
	zero.Close()
	_, err = zero.Commit(t.Context())
	require.Error(t, err)
}

type duringAccountStagingContext struct {
	context.Context
	path   string
	once   sync.Once
	action func()
}

func (ctx *duringAccountStagingContext) Err() error {
	temporary, _ := filepath.Glob(filepath.Join(filepath.Dir(ctx.path), ".accounts-change-*"))
	if len(temporary) > 0 {
		ctx.once.Do(ctx.action)
	}
	return ctx.Context.Err()
}

func TestConditionalAccountCommitRejectsReplacementAfterStaging(t *testing.T) {
	path, _, before := conditionalAccountFixture(t)
	change, err := before.BeginSwitch(t.Context(), snapshotNamespace, "second")
	require.NoError(t, err)
	defer change.Close()
	replacement := []byte(`{"active":{},"accounts":{},"foreign":"replacement during staging"}`)
	ctx := &duringAccountStagingContext{Context: t.Context(), path: path, action: func() {
		other := path + ".foreign"
		require.NoError(t, os.WriteFile(other, replacement, 0o600))
		require.NoError(t, os.Rename(other, path))
	}}
	result, err := change.Commit(ctx)
	require.ErrorIs(t, err, ErrStateChanged)
	require.False(t, result.Written)
	require.Equal(t, replacement, readConditionalDocument(t, path))
	temporary, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".accounts-change-*"))
	require.NoError(t, err)
	require.Empty(t, temporary)
}
