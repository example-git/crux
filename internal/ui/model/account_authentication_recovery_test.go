package model

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authenticationRecoveryUIWorkspace struct {
	*authenticationUIWorkspace
	recoveries []workspace.ProviderAuthenticationRecoveryRequest
}

func (w *authenticationRecoveryUIWorkspace) CanRecoverProviderAuthentication() bool { return true }
func (w *authenticationRecoveryUIWorkspace) RecoverProviderAuthentication(ctx context.Context, request workspace.ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "recovery ran outside a command")
	require.NoError(w.t, request.Validate())
	w.recoveries = append(w.recoveries, request)
	if len(w.logouts) > 0 {
		return w.result(request.OperationID, request.Target, "", true)
	}
	return w.result(request.OperationID, request.Target, w.switches[0].AccountID, false)
}

func newAuthenticationRecoveryUI(t *testing.T) (*UI, *authenticationRecoveryUIWorkspace) {
	ui, base := newAuthenticationUI(t)
	ws := &authenticationRecoveryUIWorkspace{authenticationUIWorkspace: base}
	ui.com.Workspace = ws
	return ui, ws
}

func dispatchRecoveryFromPicker(t *testing.T, ui *UI, ws *authenticationUIWorkspace, d *dialog.AccountAuthentication, retry bool) authenticationCompletedMsg {
	t.Helper()
	key := 'r'
	if retry {
		key = 't'
	}
	action := d.HandleMsg(tea.KeyPressMsg{Code: key, Mod: tea.ModAlt})
	require.Equal(t, dialog.ActionAuthenticationRecover{Dialog: d, Retry: retry}, action)
	messages := runAuthenticationCmd(ws, ui.handleDialogAction(action))
	require.Len(t, messages, 1)
	if !retry {
		require.IsType(t, authenticationRecoveryPreparedMsg{}, messages[0])
		_, cmd := ui.Update(messages[0])
		messages = runAuthenticationCmd(ws, cmd)
	}
	for _, msg := range messages {
		if completed, ok := msg.(authenticationCompletedMsg); ok {
			return completed
		}
	}
	t.Fatal("recovery command produced no completion")
	return authenticationCompletedMsg{}
}

func TestAuthenticationUIRecoveryRetainsOriginalAndExactActions(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch", true: "logout"}[logout], func(t *testing.T) {
			ui, ws := newAuthenticationRecoveryUI(t)
			base := ws.authenticationUIWorkspace
			ws.mode = "partial"
			d := openLoadedAuthentication(t, ui, base, logout)
			original := prepareAndDispatchAuthentication(t, ui, base, d)
			_, cmd := ui.Update(original)
			require.Contains(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "Alt+R")
			require.True(t, original.operation.blockNew)
			// Reload preserves the original operation and cannot make a new row
			// replace the only retry/recovery identity.
			loaded := runAuthenticationCmd(base, ui.loadAuthenticationAccounts(d))
			_, cmd = ui.Update(loaded[0])
			runAuthenticationCmd(base, cmd)
			selection := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
			require.IsType(t, dialog.ActionAuthenticationSelect{}, selection)
			require.Contains(t, authenticationInfo(runAuthenticationCmd(base, ui.handleDialogAction(selection))), "Resolve the original")
			require.Same(t, original.operation, ui.authenticationOperations[ws])
			retry := d.HandleMsg(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
			messages := runAuthenticationCmd(base, ui.handleDialogAction(retry))
			require.Len(t, messages, 1)
			_, cmd = ui.Update(messages[0])
			runAuthenticationCmd(base, cmd)
			if logout {
				require.Len(t, ws.logouts, 2)
				require.Equal(t, ws.logouts[0], ws.logouts[1])
			} else {
				require.Len(t, ws.switches, 2)
				require.Equal(t, ws.switches[0], ws.switches[1])
			}
			first := dispatchRecoveryFromPicker(t, ui, base, d, false)
			require.Equal(t, original.operation.id, first.recovery.OperationID)
			require.Equal(t, original.operation.row.Target, first.recovery.Target)
			require.EqualValues(t, 1, first.recovery.RecoverySequence)
			_, cmd = ui.Update(first)
			runAuthenticationCmd(base, cmd)
			second := dispatchRecoveryFromPicker(t, ui, base, d, true)
			require.Equal(t, ws.recoveries[0], ws.recoveries[1], "retry must retain every original recovery field")
			_, cmd = ui.Update(second)
			runAuthenticationCmd(base, cmd)
			ws.mode = ""
			third := dispatchRecoveryFromPicker(t, ui, base, d, false)
			require.EqualValues(t, 2, third.recovery.RecoverySequence)
			require.NotEqual(t, first.recovery.RecoveryID, third.recovery.RecoveryID)
			_, cmd = ui.Update(first)
			require.Empty(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "old recovery completion cannot finish a newer attempt")
			wrong := third
			changed := *third.recovery
			changed.RecoverySequence++
			wrong.recovery = &changed
			_, cmd = ui.Update(wrong)
			require.Empty(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "a foreign recovery identity cannot finish this attempt")
			require.True(t, original.operation.pending)
			ui.dialog.CloseDialog(d.ID())
			_, cmd = ui.Update(third)
			text := authenticationInfo(runAuthenticationCmd(base, cmd))
			if logout {
				require.Contains(t, text, "Logged out")
				require.Len(t, ws.logouts, 2)
			} else {
				require.Contains(t, text, "Switched account")
				require.Len(t, ws.switches, 2)
			}
			require.False(t, original.operation.blockNew)
			require.False(t, original.operation.retry)
			require.Zero(t, ws.updateAgentCalls)
		})
	}
}

func TestAuthenticationUIRecoveryPreparationPreservesOriginal(t *testing.T) {
	for _, mode := range []string{"closed", "reloaded", "replaced", "workspace changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws := newAuthenticationRecoveryUI(t)
			base := ws.authenticationUIWorkspace
			ws.mode = "partial"
			d := openLoadedAuthentication(t, ui, base, false)
			original := prepareAndDispatchAuthentication(t, ui, base, d)
			_, cmd := ui.Update(original)
			runAuthenticationCmd(base, cmd)
			prepared := runAuthenticationCmd(base, ui.beginAuthenticationRecovery(dialog.ActionAuthenticationRecover{Dialog: d}))
			require.Len(t, prepared, 1)
			switch mode {
			case "closed":
				ui.dialog.CloseDialog(d.ID())
			case "reloaded":
				ui.loadAuthenticationAccounts(d)
			case "replaced":
				ui.openAuthenticationAccounts(false)
			case "workspace changed":
				_, other := newAuthenticationUI(t)
				ui.com.Workspace = other
			}
			_, cmd = ui.Update(prepared[0])
			runAuthenticationCmd(base, cmd)
			require.Empty(t, ws.recoveries)
			require.Same(t, original.operation, ui.authenticationOperations[ws])
			require.True(t, original.operation.retry)
			require.Nil(t, original.operation.recoveryPreparing)
			require.Nil(t, original.operation.recovery)
			require.Len(t, ws.switches, 1)
		})
	}
}

func TestAuthenticationUIPrewriteRejectionAllowsNewSelection(t *testing.T) {
	ui, ws := newAuthenticationRecoveryUI(t)
	base := ws.authenticationUIWorkspace
	ws.mode = "prewrite"
	d := openLoadedAuthentication(t, ui, base, false)
	failed := prepareAndDispatchAuthentication(t, ui, base, d)
	_, cmd := ui.Update(failed)
	runAuthenticationCmd(base, cmd)
	require.False(t, failed.operation.blockNew)
	prepared := runAuthenticationCmd(base, ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
	require.Len(t, prepared, 1)
	require.IsType(t, authenticationPreparedMsg{}, prepared[0])
	require.NotSame(t, failed.operation, ui.authenticationOperations[ws])
}
