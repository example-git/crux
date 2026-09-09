package config

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestAuthenticationJournalEntriesCaptureScopeAndFreshRevisions(t *testing.T) {
	journal := oauthValidationJournal(t)
	key := AuthenticationJournalKey{Kind: AuthenticationJournalClient, WorkspaceID: "workspace", OperationID: "first"}
	second := key
	second.OperationID = "second"
	foreignWorkspace := key
	foreignWorkspace.WorkspaceID = "other-workspace"
	foreignKind := key
	foreignKind.Kind = AuthenticationJournalOAuth
	original, err := journal.Store(t.Context(), key, 0, json.RawMessage(`{"value":"original"}`), false)
	require.NoError(t, err)
	for _, other := range []AuthenticationJournalKey{second, foreignWorkspace, foreignKind} {
		_, err := journal.Store(t.Context(), other, 0, json.RawMessage(`{"value":"other"}`), false)
		require.NoError(t, err)
	}
	before, err := os.Stat(journal.path)
	require.NoError(t, err)
	beforeBytes, err := os.ReadFile(journal.path)
	require.NoError(t, err)
	entries, err := journal.Entries(t.Context(), key.Kind, key.WorkspaceID)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.Equal(t, key, entries[0].Key())
	require.Equal(t, second, entries[1].Key())
	require.Equal(t, original.Revision(), entries[0].Revision())
	require.False(t, entries[0].Completed())
	payload := entries[0].Payload()
	payload[0] = '['
	require.JSONEq(t, `{"value":"original"}`, string(entries[0].Payload()), "returned bytes cannot mutate the captured entry")
	after, err := os.Stat(journal.path)
	require.NoError(t, err)
	afterBytes, err := os.ReadFile(journal.path)
	require.NoError(t, err)
	require.True(t, os.SameFile(before, after))
	require.Equal(t, before.ModTime(), after.ModTime())
	require.Equal(t, beforeBytes, afterBytes)

	peer, err := OpenOAuthLoginJournalScope(t.Context(), journal.store.globalDataPath, "", journal.store.workingDir)
	require.NoError(t, err)
	updated, err := peer.journal.Store(t.Context(), key, original.Revision(), json.RawMessage(`{"value":"peer"}`), true)
	require.NoError(t, err)
	require.JSONEq(t, `{"value":"original"}`, string(entries[0].Payload()), "a peer write must not change an old snapshot")
	fresh, err := journal.Entries(t.Context(), key.Kind, key.WorkspaceID)
	require.NoError(t, err)
	require.Len(t, fresh, 2)
	require.Equal(t, second, fresh[0].Key())
	require.Equal(t, key, fresh[1].Key())
	require.Equal(t, updated.Revision(), fresh[1].Revision())
	require.True(t, fresh[1].Completed())
	require.JSONEq(t, `{"value":"peer"}`, string(fresh[1].Payload()))
	_, err = journal.Store(t.Context(), key, entries[0].Revision(), json.RawMessage(`{"value":"stale"}`), true)
	require.ErrorContains(t, err, "revision changed", "a snapshot cannot bypass write-time CAS")
}

func TestAuthenticationJournalEntriesRejectInvalidScopeCancellationAndForeignCorruption(t *testing.T) {
	journal := oauthValidationJournal(t)
	key := AuthenticationJournalKey{Kind: AuthenticationJournalClient, WorkspaceID: "workspace", OperationID: "first"}
	foreign := key
	foreign.WorkspaceID = "foreign"
	_, err := journal.Store(t.Context(), foreign, 0, json.RawMessage(`{"value":"retained"}`), false)
	require.NoError(t, err)
	_, err = journal.Entries(t.Context(), "unsupported", key.WorkspaceID)
	require.Error(t, err)
	_, err = journal.Entries(t.Context(), key.Kind, "")
	require.Error(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = journal.Entries(ctx, key.Kind, key.WorkspaceID)
	require.ErrorIs(t, err, context.Canceled)
	before, err := os.ReadFile(journal.path)
	require.NoError(t, err)
	invalid, err := sjson.SetBytes(before, "records."+foreign.id()+".revision", 0)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(journal.path, invalid, 0600))
	_, err = journal.Entries(t.Context(), key.Kind, key.WorkspaceID)
	require.ErrorContains(t, err, "record is invalid", "scope filtering must not bypass whole-journal validation")
	after, err := os.ReadFile(journal.path)
	require.NoError(t, err)
	require.Equal(t, invalid, after, "reading must not repair or discard malformed foreign records")
}
