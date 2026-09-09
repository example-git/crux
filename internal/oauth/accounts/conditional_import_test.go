package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestConditionalImportPreservesForeignFieldsAndExactReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	beforeData := []byte(`{"foreign":{"large":9007199254740993},"Active":{"owner.ns":"other"},"Accounts":{"owner.ns":[{"id":"target","displayName":"Old","accessToken":"old","refreshToken":"old-refresh","expiresAt":17,"raw":{"old":true},"foreign":{"precise":9007199254740993}},{"id":"other","displayName":"Other","accessToken":"other","foreign":[false,0,""]}],"untouched":[{"id":"else","accessToken":"else","foreign":1.00}]},"Mutations":{"owner.ns":{"target":3,"other":8}},"Selections":{"owner.ns":4},"Rotations":{"owner.ns":{"target":[{"before":"old","after":"peer"}],"other":[{"before":"keep","after":"keep-too"}]}}}`)
	require.NoError(t, os.WriteFile(path, beforeData, 0o600))
	before, err := CaptureStateAt(t.Context(), path, []string{"owner.ns"})
	require.NoError(t, err)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	entry := Entry{ID: "target", DisplayName: "Imported", AccessToken: "synthetic-import", Raw: json.RawMessage(`{"precise":9007199254740993}`)}
	pending, err := before.BeginImport(t.Context(), "owner.ns", entry, func() error { return nil })
	require.NoError(t, err)
	defer pending.Close()
	selected, ok := pending.SelectedEntry()
	require.True(t, ok)
	require.Equal(t, entry, selected)
	selected.Raw[0] = 'x'
	again, ok := pending.SelectedEntry()
	require.True(t, ok)
	require.Equal(t, entry, again)
	result, err := pending.Commit(t.Context())
	require.NoError(t, err)
	require.True(t, result.Written)
	require.False(t, result.CompletionDeadline().IsZero())
	require.Equal(t, "target", result.Snapshot.ActiveID("owner.ns"))
	verified, err := pending.VerifyCommitted(t.Context())
	require.NoError(t, err)
	require.True(t, result.Snapshot.SameObservation(verified))
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, field := range []string{"foreign", "Accounts.untouched", `Accounts.owner\.ns.0.foreign`, `Accounts.owner\.ns.1`, `Rotations.owner\.ns.other`} {
		require.Equal(t, gjson.GetBytes(beforeData, field).Raw, gjson.GetBytes(disk, field).Raw, field)
	}
	require.Equal(t, uint64(4), gjson.GetBytes(disk, `Mutations.owner\.ns.target`).Uint())
	require.Equal(t, uint64(5), gjson.GetBytes(disk, `Selections.owner\.ns`).Uint())
	require.False(t, gjson.GetBytes(disk, `Rotations.owner\.ns.target`).Exists())
	for _, field := range []string{"refreshToken", "expiresAt"} {
		require.False(t, gjson.GetBytes(disk, `Accounts.owner\.ns.0.`+field).Exists())
	}
	for _, value := range []any{pending, pending.state, result, result.Snapshot} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, format := range []string{"%v", "%+v", "%#v", "%s", "%d"} {
			require.NotContains(t, fmt.Sprintf(format, value), "synthetic-import")
			require.NotContains(t, fmt.Sprintf(format, value), path)
		}
	}
}

func TestConditionalImportRejectsStaleObservationAndOverflow(t *testing.T) {
	for _, mode := range []string{"identical-save", "selection-aba", "inactive-addition", "other-namespace", "replacement", "mutation-overflow", "selection-overflow"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AI_CLI_DIR", dir)
			entry := Entry{ID: "target", AccessToken: "synthetic-before"}
			require.NoError(t, Save(t.Context(), "owner", entry))
			path := filepath.Join(dir, "accounts.json")
			if strings.Contains(mode, "overflow") {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var state store
				require.NoError(t, json.Unmarshal(data, &state))
				if mode == "mutation-overflow" {
					state.Mutations["owner"][entry.ID] = math.MaxUint64
				} else {
					state.Selections["owner"] = math.MaxUint64
					state.Active["owner"] = "other"
				}
				data, err = json.Marshal(state)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path, data, 0o600))
			}
			before, err := CaptureStateAt(t.Context(), path, []string{"owner"})
			require.NoError(t, err)
			switch mode {
			case "identical-save":
				require.NoError(t, Save(t.Context(), "owner", entry))
			case "selection-aba":
				require.NoError(t, Save(t.Context(), "owner", Entry{ID: "other", AccessToken: "other"}))
				require.NoError(t, SetActive(t.Context(), "owner", entry.ID))
			case "inactive-addition":
				require.NoError(t, SaveWithoutActivating(t.Context(), "owner", Entry{ID: "other", AccessToken: "other"}))
			case "other-namespace":
				require.NoError(t, Save(t.Context(), "else", Entry{ID: "other", AccessToken: "other"}))
			case "replacement":
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(path+".replacement", data, 0o600))
				require.NoError(t, os.Rename(path+".replacement", path))
			}
			disk, err := os.ReadFile(path)
			require.NoError(t, err)
			pending, err := before.BeginImport(t.Context(), "owner", Entry{ID: "target", AccessToken: "imported"}, func() error { return nil })
			require.Error(t, err)
			require.Nil(t, pending)
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, disk, after)
		})
	}
}

func TestConditionalImportSameSelectionAndCancellation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AI_CLI_DIR", dir)
	entry := Entry{ID: "target", AccessToken: "before"}
	require.NoError(t, Save(t.Context(), "owner", entry))
	path := filepath.Join(dir, "accounts.json")
	before, err := CaptureStateAt(t.Context(), path, []string{"owner"})
	require.NoError(t, err)
	pending, err := before.BeginImport(t.Context(), "owner", Entry{ID: "target", AccessToken: "after"}, func() error { return nil })
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	result, err := pending.Commit(ctx)
	pending.Close()
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, result.Written)
	pending, err = before.BeginImport(t.Context(), "owner", entry, func() error { return nil })
	require.NoError(t, err)
	result, err = pending.Commit(t.Context())
	pending.Close()
	require.NoError(t, err)
	require.True(t, result.Written)
	require.False(t, before.SameObservation(result.Snapshot), "same-value imports remain observable writes")
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, uint64(2), gjson.GetBytes(disk, "mutations.owner.target").Uint())
	require.Equal(t, uint64(1), gjson.GetBytes(disk, "selections.owner").Uint())
}

func TestConditionalImportLateOwnerFailureRetainsWrittenOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	before, err := CaptureStateAt(t.Context(), path, []string{"owner"})
	require.NoError(t, err)
	ownerChanged := errors.New("synthetic owner changed")
	validate := func() error {
		data, err := os.ReadFile(path)
		if err == nil && gjson.GetBytes(data, "active.owner").String() == "target" {
			return ownerChanged
		}
		return nil
	}
	pending, err := before.BeginImport(t.Context(), "owner", Entry{ID: "target", AccessToken: "saved"}, validate)
	require.NoError(t, err)
	result, err := pending.Commit(t.Context())
	pending.Close()
	require.ErrorIs(t, err, ownerChanged)
	require.True(t, result.Written)
	require.False(t, result.CompletionDeadline().IsZero())
	require.False(t, result.Snapshot.valid)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "saved", gjson.GetBytes(disk, "accounts.owner.0.accessToken").String())
}
