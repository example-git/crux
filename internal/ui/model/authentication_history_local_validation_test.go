package model

import (
	"context"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type historyLocalValidationWorkspace struct {
	*authenticationUIWorkspace
	operation workspace.ProviderAuthenticationHistoryOperation
	local     config.LocalAuthenticationRepairResult
	calls     []providerauth.LocalRepairRequest
}

func (w *historyLocalValidationWorkspace) historyValidationAllowIO(v bool) { w.allowIO = v }
func (w *historyLocalValidationWorkspace) AuthenticationWorkspaceID() string {
	return w.snapshot.WorkspaceID
}
func (w *historyLocalValidationWorkspace) ProviderAuthenticationHistory(ctx context.Context) (workspace.ProviderAuthenticationHistory, error) {
	require.True(w.t, w.allowIO, "history IO ran outside Cmd")
	return workspace.ProviderAuthenticationHistory{Operations: []workspace.ProviderAuthenticationHistoryOperation{w.operation}}, ctx.Err()
}
func (w *historyLocalValidationWorkspace) RepairLocalAuthentication(ctx context.Context, r providerauth.LocalRepairRequest) (config.LocalAuthenticationRepairResult, error) {
	require.True(w.t, w.allowIO, "local recovery IO ran outside Cmd")
	require.NoError(w.t, r.Validate())
	w.calls = append(w.calls, r)
	result := w.local
	if r.Abandon {
		require.EqualValues(w.t, 7, r.Revision)
		result.Summary.Abandoned = true
		result.Summary.Revision = 8
		result.Summary.RepairReady = false
		result.Abandoned = true
	}
	return result, ctx.Err()
}
func TestAuthenticationHistoryValidationLocalRevisionAndUnknown(t *testing.T) {
	for _, noEffects := range []bool{false, true} {
		t.Run(map[bool]string{false: "unknown", true: "no-effects"}[noEffects], func(t *testing.T) {
			ui, base := newAuthenticationUI(t)
			base.cfg.Options.TUI = &config.TUIOptions{}
			target := base.accounts.Target
			id := strings.Repeat("b", 32)
			ws := &historyLocalValidationWorkspace{authenticationUIWorkspace: base,
				operation: workspace.ProviderAuthenticationHistoryOperation{OperationID: id, Target: target, Outcome: providerauth.MutationOutcome{OperationID: id, Previous: target}, LocalFinished: true, JournalRevision: 4},
				local:     config.LocalAuthenticationRepairResult{Summary: config.LocalAuthenticationSummary{WorkspaceID: target.WorkspaceID, OperationID: id, ProviderID: target.Owner.ProviderID, Action: "switch", AccountID: "second", Revision: 7, Finished: true, RefreshStarted: !noEffects, NoEffects: noEffects}},
			}
			ui.com.Workspace = ws
			historyValidationDrive(t, ui, historyValidationCommand(t, ui, "authentication_history"))
			state := ui.authenticationHistories[ws]
			entry := state.entries[state.dialog.SelectedKey()]
			historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Nil(t, state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'x', Mod: tea.ModAlt}), "abandon must require explicit review")
			historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'p', Mod: tea.ModCtrl})
			require.Len(t, ws.calls, 1)
			require.Zero(t, ws.calls[0].Revision)
			require.Equal(t, id, ws.calls[0].OperationID)
			require.Equal(t, ws.local.Summary.Original, entry.repaired.Summary.Original)
			require.Nil(t, state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}), "unknown or no-effects must never apply fixed writes")
			if noEffects {
				require.Contains(t, entry.message, "No repair is needed")
				require.Nil(t, state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'x', Mod: tea.ModAlt}))
				return
			}
			require.Contains(t, entry.message, "may have consumed its token")
			command := ui.handleDialogAction(state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'x', Mod: tea.ModAlt}))
			require.Len(t, ws.calls, 1, "Update must not perform IO")
			retained := *entry.localAbandon
			require.EqualValues(t, 7, retained.Revision)
			require.True(t, retained.Abandon)
			messages := historyValidationCollect(ui, command)
			require.Len(t, messages, 1)
			completed := messages[0].(authenticationHistoryRepairedMsg)
			require.True(t, entry.pending, "executing Cmd must not mutate UI state")
			wrong := completed
			wrong.request.Revision++
			_, next := ui.Update(wrong)
			historyValidationCollect(ui, next)
			require.True(t, entry.pending, "wrong revision cannot consume the original pending action")
			_, next = ui.Update(completed)
			historyValidationCollect(ui, next)
			require.False(t, entry.pending)
			require.True(t, entry.repaired.Summary.Abandoned)
			require.Equal(t, ws.local.Summary.Original, entry.repaired.Summary.Original)
			require.Contains(t, entry.message, "repaired no files and published no runtime")
			require.Contains(t, entry.message, "Prior repair writes: accounts=false, config=false")
			require.Nil(t, state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}))
			historyValidationKey(t, ui, state.dialog, tea.KeyPressMsg{Code: 'y', Mod: tea.ModAlt})
			require.Len(t, ws.calls, 3)
			require.Equal(t, retained, ws.calls[1])
			require.Equal(t, retained, ws.calls[2])
			// A later exact retry can complete after the user changes workspace.
			command = ui.handleDialogAction(state.dialog.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModAlt}))
			messages = historyValidationCollect(ui, command)
			require.Len(t, messages, 1)
			_, replacement := newAuthenticationUI(t)
			ui.com.Workspace = replacement
			_, next = ui.Update(messages[0])
			require.Contains(t, authenticationInfo(collectCommandMessages(next)), "Previous workspace")
			require.Empty(t, replacement.switches)
			require.Zero(t, replacement.updateAgentCalls)
			require.Zero(t, ws.updateAgentCalls, "retiring unknown progress cannot resume model selection")
		})
	}
}
