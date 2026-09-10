package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authenticationReconciliationUIWorkspace struct {
	*authenticationRecoveryUIWorkspace
	reviews               []workspace.ProviderAuthenticationReviewRequest
	applies               []workspace.ProviderAuthenticationApplyRequest
	reviewMode, applyMode string
	entered               chan context.Context
}

func (w *authenticationReconciliationUIWorkspace) CanReconcileProviderAuthentication() bool {
	return true
}

func (w *authenticationReconciliationUIWorkspace) ReviewProviderAuthentication(ctx context.Context, r workspace.ProviderAuthenticationReviewRequest) (workspace.ProviderAuthenticationReviewSummary, error) {
	require.True(w.t, w.allowIO, "review ran outside a command")
	w.reviews = append(w.reviews, r)
	if w.entered != nil {
		w.entered <- ctx
		<-ctx.Done()
		return workspace.ProviderAuthenticationReviewSummary{}, ctx.Err()
	}
	if w.reviewMode == "error" {
		return workspace.ProviderAuthenticationReviewSummary{}, config.ErrAuthenticationReconciliationReloadRequired
	}
	s := workspace.ProviderAuthenticationReviewSummary{OperationID: r.OperationID, ReviewID: r.ReviewID, ReviewSequence: r.ReviewSequence, PreviewID: strings.Repeat("c", 32), Choice: r.Choice, Owner: r.OriginalTarget.Owner, ActiveAccountID: r.Choice.AccountID, Configured: true, Receiver: config.RemoteAuthority{Mode: "client", Principal: "fixture", Revision: 3, Digest: strings.Repeat("d", 64)}, ChangedCategories: []string{"authentication"}}
	if w.reviewMode == "wrong" {
		s.ReviewID = strings.Repeat("e", 32)
	}
	return s, nil
}

func (w *authenticationReconciliationUIWorkspace) ApplyProviderAuthenticationReview(ctx context.Context, r workspace.ProviderAuthenticationApplyRequest) (workspace.ProviderAuthenticationReconciliationOutcome, error) {
	require.True(w.t, w.allowIO, "apply ran outside a command")
	w.applies = append(w.applies, r)
	o := workspace.ProviderAuthenticationReconciliationOutcome{OperationID: r.OperationID, ReviewID: r.ReviewID, PreviewID: r.PreviewID, ApplyID: r.ApplyID, OriginalDisposition: "unresolved"}
	if w.applyMode == "error" {
		return o, errors.New("synthetic lost acknowledgement")
	}
	o.RemoteAcknowledged, o.Adopted, o.OriginalDisposition = true, true, "runtime-reconciled"
	if w.applyMode == "wrong" {
		o.ApplyID = strings.Repeat("e", 32)
	}
	return o, ctx.Err()
}

func newAuthenticationReconciliationUI(t *testing.T) (*UI, *authenticationReconciliationUIWorkspace, *authenticationOperation, *dialog.AccountAuthentication) {
	t.Helper()
	ui, recovery := newAuthenticationRecoveryUI(t)
	w := &authenticationReconciliationUIWorkspace{authenticationRecoveryUIWorkspace: recovery}
	ui.com.Workspace = w
	w.mode = "partial"
	d := openLoadedAuthentication(t, ui, w.authenticationUIWorkspace, false)
	completed := prepareAndDispatchAuthentication(t, ui, w.authenticationUIWorkspace, d)
	_, cmd := ui.Update(completed)
	runAuthenticationCmd(w.authenticationUIWorkspace, cmd)
	return ui, w, completed.operation, d
}

func openAuthenticationReviewUI(t *testing.T, ui *UI, d *dialog.AccountAuthentication) *dialog.AuthenticationReconciliation {
	t.Helper()
	action := d.HandleMsg(tea.KeyPressMsg{Code: 'v', Mod: tea.ModAlt})
	require.IsType(t, dialog.ActionAuthenticationReviewOpen{}, action)
	require.Empty(t, collectCommandMessages(ui.handleDialogAction(action)))
	review, ok := ui.dialog.Dialog(dialog.AuthenticationReconciliationID).(*dialog.AuthenticationReconciliation)
	require.True(t, ok)
	return review
}

func runAuthenticationReconciliationKey(t *testing.T, ui *UI, w *authenticationUIWorkspace, d *dialog.AuthenticationReconciliation, key tea.KeyPressMsg) []tea.Msg {
	t.Helper()
	action := d.HandleMsg(key)
	require.IsType(t, dialog.ActionAuthenticationReconciliation{}, action)
	messages := runAuthenticationCmd(w, ui.handleDialogAction(action))
	if len(messages) == 1 {
		if _, ok := messages[0].(authenticationReconciliationPreparedMsg); ok {
			_, cmd := ui.Update(messages[0])
			messages = runAuthenticationCmd(w, cmd)
		}
	}
	return messages
}

func TestAuthenticationUIReconciliationRetainsSeparateIdentities(t *testing.T) {
	ui, w, original, picker := newAuthenticationReconciliationUI(t)
	base := w.authenticationUIWorkspace
	originalID, originalMessage := original.id, original.message
	d := openAuthenticationReviewUI(t, ui, picker)
	require.Empty(t, w.reviews)
	review := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0].(authenticationReviewCompletedMsg)
	require.Equal(t, originalID, review.request.OperationID)
	require.EqualValues(t, 1, review.request.ReviewSequence)
	require.NotEqual(t, originalID, review.request.ReviewID)
	_, cmd := ui.Update(review)
	runAuthenticationCmd(base, cmd)
	retryReview := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})[0].(authenticationReviewCompletedMsg)
	require.Equal(t, w.reviews[0], w.reviews[1])
	_, cmd = ui.Update(retryReview)
	runAuthenticationCmd(base, cmd)
	w.applyMode = "error"
	apply := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})[0].(authenticationApplyCompletedMsg)
	require.Equal(t, review.request.ReviewID, apply.request.ReviewID)
	require.Equal(t, review.summary.PreviewID, apply.request.PreviewID)
	require.NotEqual(t, originalID, apply.request.ApplyID)
	_, cmd = ui.Update(apply)
	runAuthenticationCmd(base, cmd)
	require.True(t, original.blockNew)
	ui.dialog.CloseDialog(d.ID())
	d = openAuthenticationReviewUI(t, ui, picker)
	w.applyMode = ""
	retryApply := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})[0].(authenticationApplyCompletedMsg)
	require.Equal(t, w.applies[0], w.applies[1])
	_, cmd = ui.Update(apply)
	require.Empty(t, runAuthenticationCmd(base, cmd), "late older attempt must not finish the retry")
	wrong := retryApply
	wrong.request.ApplyID = strings.Repeat("f", 32)
	_, cmd = ui.Update(wrong)
	require.Empty(t, runAuthenticationCmd(base, cmd))
	require.Equal(t, "apply", ui.authenticationReconciliations[original].pending)
	ui.dialog.CloseDialog(d.ID())
	_, cmd = ui.Update(retryApply)
	require.Contains(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "original operation result remains unchanged")
	require.False(t, original.blockNew)
	require.Equal(t, originalID, original.id)
	require.Equal(t, originalMessage, original.message)
	require.Len(t, w.switches, 1)
	require.Empty(t, w.recoveries)
	require.Zero(t, w.updateAgentCalls)
}

func TestAuthenticationUIReconciliationChoicesAndInvalidOutcomes(t *testing.T) {
	ui, w, original, picker := newAuthenticationReconciliationUI(t)
	base := w.authenticationUIWorkspace
	d := openAuthenticationReviewUI(t, ui, picker)
	for index, choiceKey := range []rune{'1', '2', '3'} {
		runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: choiceKey, Mod: tea.ModAlt})
		require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}), "changing the choice must remove apply authority")
		messages := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})
		msg := messages[0].(authenticationReviewCompletedMsg)
		require.EqualValues(t, index+1, msg.request.ReviewSequence)
		if choiceKey == '2' {
			require.Equal(t, "second", msg.request.Choice.AccountID, "known public account row is selectable")
		}
		_, cmd := ui.Update(msg)
		runAuthenticationCmd(base, cmd)
	}
	require.Empty(t, w.applies)
	w.reviewMode = "wrong"
	msg := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0]
	_, cmd := ui.Update(msg)
	runAuthenticationCmd(base, cmd)
	require.Nil(t, ui.authenticationReconciliations[original].summary)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}))
	w.reviewMode = "error"
	msg = runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0]
	_, cmd = ui.Update(msg)
	require.Contains(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "reload saved configuration")
	require.True(t, original.blockNew)
	require.Len(t, w.switches, 1)
}

func TestAuthenticationUIReconciliationPreparationCancellation(t *testing.T) {
	for _, change := range []string{"close", "replace", "workspace", "choice", "cancel"} {
		t.Run(change, func(t *testing.T) {
			ui, w, original, picker := newAuthenticationReconciliationUI(t)
			base := w.authenticationUIWorkspace
			d := openAuthenticationReviewUI(t, ui, picker)
			action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
			prepared := runAuthenticationCmd(base, ui.handleDialogAction(action))
			require.Len(t, prepared, 1)
			switch change {
			case "close":
				ui.dialog.CloseDialog(d.ID())
			case "replace":
				ui.dialog.CloseDialog(d.ID())
				openAuthenticationReviewUI(t, ui, picker)
			case "workspace":
				_, other := newAuthenticationUI(t)
				ui.com.Workspace = other
			case "choice":
				d.SetChoice(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-logout"})
			case "cancel":
				ui.handleDialogAction(dialog.ActionAuthenticationReconciliation{Dialog: d, Kind: "cancel"})
			}
			_, cmd := ui.Update(prepared[0])
			runAuthenticationCmd(base, cmd)
			require.Empty(t, w.reviews)
			require.Empty(t, w.applies)
			require.True(t, original.blockNew)
		})
	}
}

func TestAuthenticationUIReconciliationCancellationRetainsReview(t *testing.T) {
	ui, w, original, picker := newAuthenticationReconciliationUI(t)
	base := w.authenticationUIWorkspace
	d := openAuthenticationReviewUI(t, ui, picker)
	w.entered = make(chan context.Context, 1)
	prepared := runAuthenticationCmd(base, ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
	_, cmd := ui.Update(prepared[0])
	base.allowIO = true
	done := make(chan tea.Msg, 1)
	go func() { done <- cmd() }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("review command did not begin")
	}
	ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}))
	var msg authenticationReviewCompletedMsg
	select {
	case value := <-done:
		msg = value.(authenticationReviewCompletedMsg)
	case <-time.After(time.Second):
		t.Fatal("review command did not cancel")
	}
	base.allowIO = false
	require.ErrorIs(t, msg.err, context.Canceled)
	_, cmd = ui.Update(msg)
	runAuthenticationCmd(base, cmd)
	w.entered = nil
	retry := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'r', Mod: tea.ModCtrl})[0].(authenticationReviewCompletedMsg)
	require.Equal(t, msg.request, retry.request)
	require.True(t, original.blockNew)
	require.Len(t, w.switches, 1)
}

func TestAuthenticationUIReconciliationUnsupportedIsVisible(t *testing.T) {
	ui, w := newAuthenticationUI(t)
	w.mode = "partial"
	d := openLoadedAuthentication(t, ui, w, false)
	completed := prepareAndDispatchAuthentication(t, ui, w, d)
	_, cmd := ui.Update(completed)
	runAuthenticationCmd(w, cmd)
	action := d.HandleMsg(tea.KeyPressMsg{Code: 'v', Mod: tea.ModAlt})
	require.IsType(t, dialog.ActionAuthenticationReviewOpen{}, action)
	require.Contains(t, authenticationInfo(runAuthenticationCmd(w, ui.handleDialogAction(action))), "Server-owned and local workspaces are not supported")
	require.Len(t, w.switches, 1)
	require.Nil(t, ui.dialog.Dialog(dialog.AuthenticationReconciliationID))
}

func TestAuthenticationUIReconciliationDelayedPreviousWorkspace(t *testing.T) {
	ui, w, original, picker := newAuthenticationReconciliationUI(t)
	base := w.authenticationUIWorkspace
	d := openAuthenticationReviewUI(t, ui, picker)
	msg := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0].(authenticationReviewCompletedMsg)
	_, other := newAuthenticationUI(t)
	ui.com.Workspace = other
	_, cmd := ui.Update(msg)
	runAuthenticationCmd(base, cmd)
	require.Nil(t, ui.dialog.Dialog(dialog.AuthenticationReconciliationID), "old workspace review must not populate current workspace UI")
	require.NotNil(t, ui.authenticationReconciliations[original].summary, "completed preview remains retained for its original workspace")
	require.Empty(t, w.applies)
	ui.com.Workspace = w
	loaded := runAuthenticationCmd(base, ui.openAuthenticationAccounts(false))
	_, cmd = ui.Update(loaded[0])
	runAuthenticationCmd(base, cmd)
	picker = ui.dialog.Dialog(dialog.AccountSwitcherID).(interface {
		AuthenticationState() *dialog.AccountAuthentication
	}).AuthenticationState()
	d = openAuthenticationReviewUI(t, ui, picker)
	w.applyMode = "wrong"
	applied := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})[0].(authenticationApplyCompletedMsg)
	_, cmd = ui.Update(applied)
	text := authenticationInfo(runAuthenticationCmd(base, cmd))
	require.NotContains(t, text, "The receiver acknowledged", "a malformed acknowledgement is not remote proof")
	require.True(t, original.blockNew)
	require.False(t, ui.authenticationReconciliations[original].resolved)
	w.applyMode = ""
	retried := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})[0].(authenticationApplyCompletedMsg)
	ui.com.Workspace = other
	_, cmd = ui.Update(retried)
	require.Contains(t, authenticationInfo(runAuthenticationCmd(base, cmd)), "Previous workspace")
	require.Nil(t, ui.authenticationOperations[other])
	require.False(t, original.blockNew)
}

func TestAuthenticationUIReconciliationChoiceRoundTripRequiresFreshReview(t *testing.T) {
	for _, attempted := range []bool{false, true} {
		t.Run(map[bool]string{false: "preview", true: "attempted"}[attempted], func(t *testing.T) {
			ui, w, original, picker := newAuthenticationReconciliationUI(t)
			base := w.authenticationUIWorkspace
			d := openAuthenticationReviewUI(t, ui, picker)
			first := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0].(authenticationReviewCompletedMsg)
			_, cmd := ui.Update(first)
			runAuthenticationCmd(base, cmd)
			if attempted {
				w.applyMode = "error"
				applied := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})[0]
				_, cmd = ui.Update(applied)
				runAuthenticationCmd(base, cmd)
			}
			state := ui.authenticationReconciliations[original]
			retainedRequest, retainedApply := state.request, state.apply
			for _, choice := range []rune{'2', '1'} {
				runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: choice, Mod: tea.ModAlt})
			}
			require.Equal(t, first.request.Choice, d.Choice())
			require.Same(t, retainedRequest, state.request)
			require.Same(t, retainedApply, state.apply)
			ui.dialog.CloseDialog(d.ID())
			d = openAuthenticationReviewUI(t, ui, picker)
			for _, key := range []rune{'r', 'y', 't'} {
				require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: key, Mod: tea.ModCtrl}), "returning to a value and reopening cannot revive stale preview authority")
			}
			require.Nil(t, ui.handleDialogAction(dialog.ActionAuthenticationReconciliation{Dialog: d, Kind: "apply", Choice: d.Choice()}))
			second := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0].(authenticationReviewCompletedMsg)
			require.Equal(t, first.request.ReviewSequence+1, second.request.ReviewSequence)
			require.NotEqual(t, first.request.ReviewID, second.request.ReviewID)
			_, cmd = ui.Update(second)
			runAuthenticationCmd(base, cmd)
			require.IsType(t, dialog.ActionAuthenticationReconciliation{}, d.HandleMsg(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}))
		})
	}
}

func TestAuthenticationUIReconciliationRefreshesOnlyInitiatingPicker(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "original", true: "replacement"}[replacement], func(t *testing.T) {
			ui, w, _, picker := newAuthenticationReconciliationUI(t)
			base := w.authenticationUIWorkspace
			d := openAuthenticationReviewUI(t, ui, picker)
			msg := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: tea.KeyEnter})[0]
			_, cmd := ui.Update(msg)
			runAuthenticationCmd(base, cmd)
			applied := runAuthenticationReconciliationKey(t, ui, base, d, tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl})[0]
			ui.dialog.CloseDialog(d.ID())
			if replacement {
				ui.dialog.CloseDialog(picker.ID())
				ui.dialog.OpenDialog(dialog.NewAccountSwitcher(ui.com))
			}
			reads, usageGeneration := w.reads, ui.usageFetchGen
			_, cmd = ui.Update(applied)
			require.Equal(t, reads, w.reads, "Update must not read account rows")
			messages := runAuthenticationCmd(base, cmd)
			wantReads := reads + 1
			if replacement {
				wantReads = reads
			}
			require.Equal(t, wantReads, w.reads)
			require.Equal(t, usageGeneration+1, ui.usageFetchGen)
			for _, message := range messages {
				if loaded, ok := message.(authenticationLoadedMsg); ok {
					require.Same(t, picker, loaded.read.dialog)
				}
			}
		})
	}
}
