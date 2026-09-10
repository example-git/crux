package model

import (
	"context"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type oauthLoginAbandonMsg struct {
	read     *oauthLoginRead
	sequence uint64
	request  providerauth.OAuthLoginAbandonRequest
	outcome  providerauth.OAuthLoginAbandonOutcome
	err      error
}

func (m *UI) abandonOAuthLoginRecorded(action dialog.ActionOAuthLoginResult, r *oauthLoginRead) tea.Cmd {
	capability, ok := r.workspace.(workspace.ProviderOAuthAbandoner)
	if !ok {
		return util.CmdHandler(util.InfoMsg{Type: util.InfoTypeError, Msg: "OAuth abandonment is unavailable in this workspace."})
	}
	found := false
	for _, result := range r.recorded {
		if result.OriginalWorkspaceID == action.OriginalWorkspaceID && result.OperationID == action.OriginalOperationID && (result.State == "exchange-outcome-unknown" || result.State == "not-started") && !result.Abandoned {
			found = true
			break
		}
	}
	if !found {
		return util.CmdHandler(util.InfoMsg{Type: util.InfoTypeError, Msg: "Only the selected preparation or unknown exchange without a recorded token can be abandoned."})
	}
	request := providerauth.OAuthLoginAbandonRequest{Target: providerauth.Target{WorkspaceID: r.snapshot.WorkspaceID, Generation: r.snapshot.Generation, Owner: r.recordedOwner}, OriginalWorkspaceID: action.OriginalWorkspaceID, OriginalOperationID: action.OriginalOperationID}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	r.abandonRequest = &request
	r.recordedLoaded = false
	r.recordedSequence++
	sequence := r.recordedSequence
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	r.cancel = cancel
	r.dialog.SetProviders(nil)
	r.dialog.SetPresentation(dialog.OAuthLoginPresentation{Message: "Abandoning tokenless operation " + request.OriginalWorkspaceID + " / " + request.OriginalOperationID + ". Waiting for any active exchange or result write to finish. A recorded token will cause this action to be refused; its original not-started or unknown state stays unchanged."})
	return func() tea.Msg {
		defer cancel()
		outcome, err := capability.AbandonProviderOAuthLoginResult(ctx, request)
		return oauthLoginAbandonMsg{read: r, sequence: sequence, request: request, outcome: outcome, err: err}
	}
}

func (m *UI) completeOAuthLoginAbandon(msg oauthLoginAbandonMsg) tea.Cmd {
	r := msg.read
	if r == nil || m.oauthLoginReads[r.dialog] != r || r.workspace != m.com.Workspace || !m.oauthDialogOpen(r.dialog) || r.recordedSequence != msg.sequence || r.abandonRequest == nil || *r.abandonRequest != msg.request {
		return nil
	}
	err := msg.err
	if msg.outcome.Validate(msg.request) != nil || err == nil && !msg.outcome.Abandoned {
		err = providerauth.ErrReceiptUnverified
	}
	if msg.outcome.Validate(msg.request) == nil && msg.outcome.Abandoned {
		r.recordedNotice = "Abandoned " + msg.request.OriginalWorkspaceID + " / " + msg.request.OriginalOperationID + ". Its reservation was released; original exchange outcome: " + msg.outcome.ExchangeOutcome + ". Retained evidence is eligible for bounded pruning."
	} else {
		r.recordedNotice = "Abandonment was not verified: " + safeOAuthLoginError(err) + " Reload the recorded status or explicitly retry the same original operation."
	}
	return m.beginOAuthLogin(r.dialog, r.recordedOwner)
}
