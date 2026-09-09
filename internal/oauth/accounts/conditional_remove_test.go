package accounts

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConditionalAccountRemoveExactSuccessorAndTombstone(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "inactive", true: "active"}[active], func(t *testing.T) {
			path, first, _ := conditionalAccountFixture(t)
			require.NoError(t, SaveWithoutActivating(t.Context(), snapshotNamespace, Entry{ID: "a-before-second-in-sorted-order", AccessToken: "third", Raw: json.RawMessage(`{"large":9007199254740993}`)}))
			require.NoError(t, Save(t.Context(), "foreign", Entry{ID: "other", AccessToken: "untouched", Raw: json.RawMessage(`{"n":1.0}`)}))
			before := captureAccountSnapshot(t, path)
			original := readConditionalDocument(t, path)
			var prior store
			require.NoError(t, json.Unmarshal(original, &prior))
			id := "second"
			if active {
				id = first.ID
			}
			change, err := before.BeginRemove(t.Context(), snapshotNamespace, id)
			require.NoError(t, err)
			defer change.Close()
			require.Equal(t, original, readConditionalDocument(t, path))
			assertConditionalLeaseHeld(t, path)
			result, err := change.Commit(t.Context())
			require.NoError(t, err)
			require.True(t, result.Written)
			expected := first.ID
			if active {
				expected = "second"
			}
			require.Equal(t, expected, result.Snapshot.ActiveID(snapshotNamespace))
			selected, ok := change.SelectedEntry()
			require.True(t, ok)
			require.Equal(t, expected, selected.ID)
			var actual store
			require.NoError(t, json.Unmarshal(readConditionalDocument(t, path), &actual))
			require.Equal(t, prior.Accounts["foreign"], actual.Accounts["foreign"])
			require.Equal(t, prior.Mutations[snapshotNamespace][id]+1, actual.Mutations[snapshotNamespace][id])
			increment := uint64(0)
			if active {
				increment = 1
			}
			require.Equal(t, prior.Selections[snapshotNamespace]+increment, actual.Selections[snapshotNamespace])
			for _, entry := range actual.Accounts[snapshotNamespace] {
				require.NotEqual(t, id, entry.ID)
			}
			require.Len(t, actual.Accounts[snapshotNamespace], 2)
			require.Contains(t, string(readConditionalDocument(t, path)), `9007199254740993`)
			_, err = change.Commit(t.Context())
			require.Error(t, err)
		})
	}
}
func TestConditionalAccountRemoveLastAndCapturedPath(t *testing.T) {
	path, first := accountSnapshotFixture(t)
	before := captureAccountSnapshot(t, path)
	other := filepath.Join(t.TempDir(), "unused")
	t.Setenv("AI_CLI_DIR", other)
	change, err := before.BeginRemove(t.Context(), snapshotNamespace, first.ID)
	require.NoError(t, err)
	defer change.Close()
	result, err := change.Commit(t.Context())
	require.NoError(t, err)
	require.Empty(t, result.Snapshot.Entries(snapshotNamespace))
	require.Empty(t, result.Snapshot.ActiveID(snapshotNamespace))
	_, ok := change.SelectedEntry()
	require.False(t, ok)
	require.NoDirExists(t, other)
}
func TestConditionalAccountRemoveRejectsMissingChangedAndCanceled(t *testing.T) {
	path, first, before := conditionalAccountFixture(t)
	original := readConditionalDocument(t, path)
	_, err := before.BeginRemove(t.Context(), snapshotNamespace, "missing")
	require.Error(t, err)
	require.Equal(t, original, readConditionalDocument(t, path))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = before.BeginRemove(ctx, snapshotNamespace, first.ID)
	require.ErrorIs(t, err, context.Canceled)
	change, err := before.BeginRemove(t.Context(), snapshotNamespace, first.ID)
	require.NoError(t, err)
	result, err := change.Commit(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, result.Written)
	change.Close()
	require.Equal(t, original, readConditionalDocument(t, path))
	require.NoError(t, os.WriteFile(path, append(original, ' '), 0600))
	_, err = before.BeginRemove(t.Context(), snapshotNamespace, first.ID)
	require.ErrorIs(t, err, ErrStateChanged)
}

func TestConditionalAccountRemoveRejectsAmbiguousIdentityFields(t *testing.T) {
	for _, entry := range []string{`{"id":"selected","id":"other"}`, `{"id":"selected","ID":"other"}`, `{"id":""}`} {
		t.Run(entry, func(t *testing.T) {
			path, _ := accountSnapshotFixture(t)
			document := []byte(`{"active":{"plugin:exact-snapshot-owner":"other"},"accounts":{"plugin:exact-snapshot-owner":[` + entry + `]}}`)
			require.NoError(t, os.WriteFile(path, document, 0600))
			before, err := CaptureStateAt(t.Context(), path, []string{snapshotNamespace})
			require.NoError(t, err)
			_, err = before.BeginRemove(t.Context(), snapshotNamespace, "other")
			require.Error(t, err)
			require.Equal(t, document, readConditionalDocument(t, path))
		})
	}
}
