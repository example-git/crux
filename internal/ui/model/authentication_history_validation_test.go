package model

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/stretchr/testify/require"
)

// Drive the same identity-bearing messages as the top-level event loop, while
// leaving unrelated timer/cache messages out of this finite interaction.
func historyValidationDrive(t *testing.T, ui *UI, cmd tea.Cmd) []tea.Msg {
	t.Helper()
	queue := historyValidationCollect(ui, cmd)
	var seen []tea.Msg
	for len(queue) > 0 {
		require.Less(t, len(seen), 100, "authentication interaction did not settle")
		msg := queue[0]
		queue = queue[1:]
		seen = append(seen, msg)
		switch msg.(type) {
		case authenticationLoadedMsg, authenticationPreparedMsg, authenticationCompletedMsg,
			authenticationHistoryLoadedMsg, authenticationHistoryPreparedMsg, authenticationHistoryRecoveredMsg,
			authenticationHistoryRepairedMsg, authenticationHistoryAbandonPreparedMsg, authenticationHistoryAbandonedMsg,
			authenticationReconciliationPreparedMsg, authenticationReviewCompletedMsg, authenticationApplyCompletedMsg,
			savedAuthenticationReadMsg, savedAuthenticationReloadMsg:
			_, next := ui.Update(msg)
			queue = append(queue, historyValidationCollect(ui, next)...)
		}
	}
	return seen
}
func historyValidationCommand(t *testing.T, ui *UI, id string) tea.Cmd {
	t.Helper()
	commands, err := dialog.NewCommands(ui.com, "", false, false, false, nil, nil)
	require.NoError(t, err)
	for _, item := range commands.AllItems() {
		if item.ID() == id {
			return ui.handleDialogAction(item.Action())
		}
	}
	t.Fatalf("registered command %q absent", id)
	return nil
}
func historyValidationUI(f *authenticationReconciliationTLSFixture) (*UI, *authenticationReconciliationTLSWorkspace) {
	ws := &authenticationReconciliationTLSWorkspace{authenticationSDKUIWorkspace: &authenticationSDKUIWorkspace{ClientWorkspace: f.w}}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	ui.dialog = dialog.NewOverlay()
	return ui, ws
}
func historyValidationSwitch(t *testing.T, ui *UI, f *authenticationReconciliationTLSFixture) authenticationCompletedMsg {
	t.Helper()
	historyValidationDrive(t, ui, ui.openAuthenticationAccounts(false))
	picker := ui.dialog.Dialog(dialog.AccountSwitcherID).(interface {
		AuthenticationState() *dialog.AccountAuthentication
	}).AuthenticationState()
	for range 10 {
		action := picker.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionAuthenticationSelect)
		if action.Row.Target.Owner.ProviderID == "copilot" && action.Row.AccountID == f.second.ID {
			for _, msg := range historyValidationDrive(t, ui, ui.handleDialogAction(action)) {
				if completed, ok := msg.(authenticationCompletedMsg); ok {
					return completed
				}
			}
			t.Fatal("switch command produced no completion")
		}
		picker.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	t.Fatal("second account absent")
	return authenticationCompletedMsg{}
}
func historyValidationKey(t *testing.T, ui *UI, d dialog.Dialog, key tea.KeyPressMsg) []tea.Msg {
	t.Helper()
	action := d.HandleMsg(key)
	require.NotNil(t, action)
	return historyValidationDrive(t, ui, ui.handleDialogAction(action))
}
func TestAuthenticationHistoryValidationTLSRecoveryAndRetirement(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(map[bool]string{false: "recover", true: "retire"}[retire], func(t *testing.T) {
			f := newHistoryValidationTLSFixture(t)
			ui, ws := historyValidationUI(f)
			f.putMode.Store(1)
			original := historyValidationSwitch(t, ui, f)
			require.Error(t, original.err)
			require.True(t, original.outcome.Progress.RuntimePublished)
			require.EqualValues(t, 1, f.puts.Load())
			originalOutcome := original.outcome
			originalModels := f.store.Config().Models
			historyValidationDrive(t, ui, historyValidationCommand(t, ui, "authentication_history"))
			state := ui.authenticationHistories[ws]
			require.NotNil(t, state)
			entry := state.entries[historyOperationKey(f.id, original.operation.id)]
			require.NotNil(t, entry)
			require.Equal(t, originalOutcome, entry.operation.Outcome)
			require.Positive(t, entry.operation.JournalRevision)
			require.Equal(t, entry.key, state.dialog.SelectedKey())
			historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
			if retire {
				revision := entry.operation.JournalRevision
				seen := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'b', Mod: tea.ModAlt})
				var completed authenticationHistoryAbandonedMsg
				for _, msg := range seen {
					if v, ok := msg.(authenticationHistoryAbandonedMsg); ok {
						completed = v
					}
				}
				require.NoError(t, completed.err)
				require.True(t, completed.outcome.Abandoned)
				require.Equal(t, revision, completed.request.Revision)
				require.Equal(t, original.operation.id, completed.request.OperationID)
				require.Equal(t, originalOutcome, completed.outcome.Original)
				require.False(t, completed.outcome.Adopted)
				require.False(t, completed.outcome.RemoteAcknowledged)
				require.Contains(t, entry.message, "neither reverses it nor publishes")
				require.NotContains(t, entry.message, "acknowledged and adopted")
				retry := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'n', Mod: tea.ModAlt})
				for _, msg := range retry {
					if v, ok := msg.(authenticationHistoryAbandonedMsg); ok {
						require.Equal(t, completed.request, v.request)
						require.Equal(t, completed.outcome, v.outcome)
						require.NoError(t, v.err)
					}
				}
				require.Nil(t, state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'r', Mod: tea.ModAlt}))
				require.EqualValues(t, 1, f.puts.Load())
			} else {
				f.getMode.Store(1)
				first := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'r', Mod: tea.ModAlt})
				var failed authenticationHistoryRecoveredMsg
				for _, msg := range first {
					if v, ok := msg.(authenticationHistoryRecoveredMsg); ok {
						failed = v
					}
				}
				require.Error(t, failed.err)
				require.EqualValues(t, 1, failed.request.RecoverySequence)
				retry := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 't', Mod: tea.ModAlt})
				for _, msg := range retry {
					if v, ok := msg.(authenticationHistoryRecoveredMsg); ok {
						require.Equal(t, failed.request, v.request)
					}
				}
				require.EqualValues(t, 1, f.puts.Load())
				f.getMode.Store(0)
				f.putMode.Store(0)
				recovered := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'r', Mod: tea.ModAlt})
				var done authenticationHistoryRecoveredMsg
				for _, msg := range recovered {
					if v, ok := msg.(authenticationHistoryRecoveredMsg); ok {
						done = v
					}
				}
				require.NoError(t, done.err)
				require.NotNil(t, done.outcome.Change)
				require.EqualValues(t, 2, done.request.RecoverySequence)
				require.NotEqual(t, failed.request.RecoveryID, done.request.RecoveryID)
				require.Equal(t, original.operation.id, done.request.OperationID)
				require.Equal(t, original.operation.row.Target, done.request.Target)
				require.EqualValues(t, 2, f.puts.Load())
				require.Contains(t, entry.message, "acknowledged and adopted")
			}
			require.Equal(t, originalOutcome, entry.operation.Outcome, "history actions must not rewrite original progress")
			require.Equal(t, originalModels, f.store.Config().Models)
			_, err := os.Stat(f.marker)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestAuthenticationHistoryValidationSavedReloadTLS(t *testing.T) {
	f := newHistoryValidationTLSFixture(t)
	ui, _ := historyValidationUI(f)
	originalModels := f.store.Config().Models
	// Keep the account coherent while changing a real saved input that only an
	// explicit reload may introduce into the local runtime.
	raw, err := os.ReadFile(f.path)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(raw, &document))
	document["providers"].(map[string]any)["copilot"].(map[string]any)["extra_headers"] = map[string]string{"X-Reloaded": "synthetic-disk-only"}
	raw, err = json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, raw, 0600))
	historyValidationDrive(t, ui, historyValidationCommand(t, ui, "review_saved_authentication"))
	state := ui.savedAuthentication
	require.NotNil(t, state)
	before := state.snapshot.Generation
	seen := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'l', Mod: tea.ModCtrl})
	var reload savedAuthenticationReloadMsg
	for _, msg := range seen {
		if v, ok := msg.(savedAuthenticationReloadMsg); ok {
			reload = v
		}
	}
	require.NoError(t, reload.err)
	require.True(t, reload.outcome.Reloaded)
	require.NotEqual(t, before, state.snapshot.Generation)
	require.EqualValues(t, 0, f.puts.Load(), "local reload must not publish receiver runtime")
	provider, ok := f.store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "synthetic-disk-only", provider.ExtraHeaders["X-Reloaded"])
	retry := historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	for _, msg := range retry {
		if v, ok := msg.(savedAuthenticationReloadMsg); ok {
			require.Equal(t, reload.request, v.request)
			require.Equal(t, reload.outcome, v.outcome)
			require.NoError(t, v.err)
		}
	}
	require.EqualValues(t, 0, f.puts.Load())
	// Select the active account intent explicitly, then review and apply it.
	for range 10 {
		action := state.dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionSavedAuthentication)
		if action.Selection.Choice.Kind == "saved-account" {
			historyValidationDrive(t, ui, ui.handleDialogAction(action))
			break
		}
		state.dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	review := ui.dialog.Dialog(dialog.AuthenticationReconciliationID).(*dialog.AuthenticationReconciliation)
	reviewed := historyValidationKey(t, ui, review, tea.KeyPressMsg{Code: tea.KeyEnter})
	var preview authenticationReviewCompletedMsg
	for _, msg := range reviewed {
		if v, ok := msg.(authenticationReviewCompletedMsg); ok {
			preview = v
		}
	}
	require.NoError(t, preview.err)
	require.True(t, preview.request.FreshSaved)
	require.EqualValues(t, 0, f.puts.Load())
	applied := historyValidationKey(t, ui, review, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	var done authenticationApplyCompletedMsg
	for _, msg := range applied {
		if v, ok := msg.(authenticationApplyCompletedMsg); ok {
			done = v
		}
	}
	require.NoError(t, done.err)
	require.True(t, done.outcome.RemoteAcknowledged && done.outcome.Adopted)
	require.Equal(t, preview.summary.PreviewID, done.request.PreviewID)
	require.EqualValues(t, 1, f.puts.Load())
	require.Equal(t, originalModels, f.store.Config().Models)
}

// A delayed history read may finish after the UI selects another workspace.
// Its records must not be installed in that workspace's visible state.
func TestAuthenticationHistoryValidationStaleRead(t *testing.T) {
	f := newHistoryValidationTLSFixture(t)
	ui, ws := historyValidationUI(f)
	cmd := historyValidationCommand(t, ui, "authentication_history")
	messages := collectCommandMessages(cmd)
	require.Len(t, messages, 1)
	loaded := messages[0].(authenticationHistoryLoadedMsg)
	require.NoError(t, loaded.err)
	ui.com.Workspace = &testWorkspace{cfg: &config.Config{Options: &config.Options{}}}
	_, next := ui.Update(loaded)
	collectCommandMessages(next)
	require.Empty(t, ui.authenticationHistories[ws].sourceID)
	require.Empty(t, ui.authenticationHistories[ws].entries)
	require.Contains(t, ui.authenticationHistories[ws].message, "Workspace changed")
}

func historyValidationCollect(ui *UI, cmd tea.Cmd) []tea.Msg {
	if gate, ok := ui.com.Workspace.(interface{ historyValidationAllowIO(bool) }); ok {
		gate.historyValidationAllowIO(true)
		defer gate.historyValidationAllowIO(false)
	}
	return collectCommandMessages(cmd)
}

func newHistoryValidationTLSFixture(t *testing.T) *authenticationReconciliationTLSFixture {
	t.Helper()
	f := newAuthenticationReconciliationTLSFixture(t, false)
	// Join owned workers before the underlying fixture restores DefaultTransport.
	t.Cleanup(func() { f.w.Shutdown(); _ = f.s.Close() })
	return f
}

func TestAuthenticationHistoryValidationRestoresLostReviewTLS(t *testing.T) {
	f := newHistoryValidationTLSFixture(t)
	ui, _ := historyValidationUI(f)
	f.putMode.Store(1)
	original := historyValidationSwitch(t, ui, f)
	require.Error(t, original.err)
	historyValidationDrive(t, ui, historyValidationCommand(t, ui, "authentication_history"))
	state := ui.authenticationHistories[ui.com.Workspace]
	historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
	historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
	review := ui.dialog.Dialog(dialog.AuthenticationReconciliationID).(*dialog.AuthenticationReconciliation)
	seen := historyValidationKey(t, ui, review, tea.KeyPressMsg{Code: tea.KeyEnter})
	var preview authenticationReviewCompletedMsg
	for _, msg := range seen {
		if v, ok := msg.(authenticationReviewCompletedMsg); ok {
			preview = v
		}
	}
	require.NoError(t, preview.err)
	require.NotEmpty(t, preview.summary.PreviewID)
	f.putMode.Store(2)
	loseGet := func() { f.getMode.Store(1) }
	f.afterPut.Store(&loseGet)
	seen = historyValidationKey(t, ui, review, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})
	var lost authenticationApplyCompletedMsg
	for _, msg := range seen {
		if v, ok := msg.(authenticationApplyCompletedMsg); ok {
			lost = v
		}
	}
	require.Error(t, lost.err)
	require.False(t, lost.outcome.Adopted)
	require.EqualValues(t, 2, f.puts.Load())
	// Throw away all UI controllers. Only the public history can reconstruct
	// this exact original review and admitted Apply request.
	ui, _ = historyValidationUI(f)
	f.afterPut.Store(nil)
	f.getMode.Store(0)
	historyValidationDrive(t, ui, historyValidationCommand(t, ui, "authentication_history"))
	state = ui.authenticationHistories[ui.com.Workspace]
	key := historyReviewKey(f.id, preview.request.ReviewID)
	entry := state.entries[key]
	require.NotNil(t, entry)
	require.NotNil(t, entry.review)
	require.Equal(t, preview.request, entry.review.Request)
	require.NotNil(t, entry.review.ApplyRequest)
	require.Equal(t, lost.request, *entry.review.ApplyRequest)
	for range len(state.order) {
		if state.dialog.SelectedKey() == key {
			break
		}
		state.dialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
	}
	require.Equal(t, key, state.dialog.SelectedKey())
	historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
	historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
	review = ui.dialog.Dialog(dialog.AuthenticationReconciliationID).(*dialog.AuthenticationReconciliation)
	seen = historyValidationKey(t, ui, review, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
	var done authenticationApplyCompletedMsg
	for _, msg := range seen {
		if v, ok := msg.(authenticationApplyCompletedMsg); ok {
			done = v
		}
	}
	require.NoError(t, done.err)
	require.Equal(t, lost.request, done.request)
	require.True(t, done.outcome.RemoteAcknowledged && done.outcome.Adopted)
	require.EqualValues(t, 2, f.puts.Load(), "restored lost Apply must resolve by receipt read, never a second PUT")
	require.Equal(t, original.outcome, state.entries[historyOperationKey(f.id, original.operation.id)].operation.Outcome)
}
