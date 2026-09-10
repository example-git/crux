package model

import (
	"context"
	"strings"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type oauthLoginRecordedMsg struct {
	read     *oauthLoginRead
	sequence uint64
	target   providerauth.Target
	list     providerauth.OAuthLoginRecoveryList
	err      error
}

func (m *UI) beginOAuthLogin(d *dialog.OAuthLogin, owner providerauth.Owner) tea.Cmd {
	r := m.oauthLoginReads[d]
	if r == nil || r.workspace != m.com.Workspace || !m.oauthDialogOpen(d) {
		return nil
	}
	if previous := m.oauthLogins[r.workspace]; previous != nil && !previous.resolved {
		return util.ReportWarn("The original login remains retained; retry it or reload after it ends.")
	}
	found := false
	for _, status := range r.snapshot.Providers {
		if status.Owner == owner && owner.HasOAuth {
			found = true
			break
		}
	}
	if !found || r.expected != nil && *r.expected != owner {
		return util.ReportError(providerauth.ErrOwner)
	}
	if r.selection != nil {
		if r.generation != m.modelSelectionGen || providerauth.PublicOwner(r.selection.ProviderOwner) != owner {
			return util.ReportError(providerauth.ErrStale)
		}
		if err := r.selection.ValidateProviderOwner(r.workspace.Config()); err != nil {
			return util.ReportError(err)
		}
	}
	r.recordedOwner, r.recorded, r.recordedLoaded = owner, nil, false
	r.recordedSequence++
	sequence := r.recordedSequence
	target := providerauth.Target{WorkspaceID: r.snapshot.WorkspaceID, Owner: owner, Generation: r.snapshot.Generation}
	d.SetProviders(nil)
	d.SetPresentation(dialog.OAuthLoginPresentation{Message: "Loading recorded OAuth results for " + owner.ProviderID + "…", Reload: true})
	capability, ok := r.workspace.(workspace.ProviderOAuthRecovery)
	if !ok {
		return m.startOAuthLogin(d, owner, "", "")
	}
	if r.cancel != nil {
		r.cancel()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	r.cancel = cancel
	return func() tea.Msg {
		defer cancel()
		list, err := capability.ListProviderOAuthLoginResults(ctx, target)
		return oauthLoginRecordedMsg{read: r, sequence: sequence, target: target, list: list, err: err}
	}
}

func (m *UI) completeOAuthLoginRecorded(msg oauthLoginRecordedMsg) tea.Cmd {
	r := msg.read
	if r == nil || m.oauthLoginReads[r.dialog] != r || r.workspace != m.com.Workspace || !m.oauthDialogOpen(r.dialog) || r.recordedSequence != msg.sequence || r.recordedOwner != msg.target.Owner {
		return nil
	}
	err := msg.err
	if err == nil {
		err = msg.list.Validate()
	}
	if err == nil && (msg.list.Target != msg.target || msg.target.WorkspaceID != r.snapshot.WorkspaceID || msg.target.Generation != r.snapshot.Generation) {
		err = providerauth.ErrStale
	}
	choices := []dialog.OAuthLoginResultChoice{{Label: "Start a new login"}}
	lines := []string{"Choose a new login, recover and save an observed token, or explicitly abandon a tokenless preparation or unknown exchange. Abandonment frees its reservation while preserving its original not-started or unknown state; it cannot discard a recorded token and does not acknowledge authentication."}
	r.recorded = nil
	if r.recordedNotice != "" {
		lines = append(lines, r.recordedNotice)
	}
	if err != nil {
		lines = append(lines, "Recorded results could not be loaded: "+safeOAuthLoginError(err))
	} else {
		r.recorded = append([]providerauth.OAuthLoginRecordedResult(nil), msg.list.Results...)
		for _, result := range r.recorded {
			if result.Abandoned {
				if result.State == "token-result-recorded" {
					lines = append(lines, "Token recovery was explicitly abandoned for workspace "+result.OriginalWorkspaceID+", operation "+result.OperationID+". Its observed result remains recorded until bounded history pruning.")
					continue
				}
				lines = append(lines, "Workspace "+result.OriginalWorkspaceID+", operation "+result.OperationID+": abandoned; original state: "+result.State+". Evidence is retained until bounded history pruning.")
				continue
			}
			if result.State == "token-result-recorded" {
				choices = append(choices, dialog.OAuthLoginResultChoice{OriginalWorkspaceID: result.OriginalWorkspaceID, OperationID: result.OperationID, Label: "Recover and save " + result.OriginalWorkspaceID + " / " + result.OperationID})
			} else {
				if result.State == "exchange-outcome-unknown" || result.State == "not-started" {
					if _, ok := r.workspace.(workspace.ProviderOAuthAbandoner); ok {
						choices = append(choices, dialog.OAuthLoginResultChoice{OriginalWorkspaceID: result.OriginalWorkspaceID, OperationID: result.OperationID, Label: "Abandon " + result.State + " operation " + result.OriginalWorkspaceID + " / " + result.OperationID, Abandon: true})
					}
				}
				lines = append(lines, "Workspace "+result.OriginalWorkspaceID+", operation "+result.OperationID+": "+result.State+"; no observed token is available for recovery.")
			}
		}
		if len(r.recorded) == 0 {
			lines = append(lines, "No pending recorded results for this owner.")
		}
	}
	r.recordedLoaded = true
	r.dialog.SetRecordedResults(choices)
	r.dialog.SetPresentation(dialog.OAuthLoginPresentation{Message: strings.Join(lines, "\n"), Reload: true})
	return nil
}

func (m *UI) chooseOAuthLoginRecorded(action dialog.ActionOAuthLoginResult) tea.Cmd {
	r := m.oauthLoginReads[action.Dialog]
	if r == nil || r.workspace != m.com.Workspace || !m.oauthDialogOpen(action.Dialog) || !r.recordedLoaded {
		return nil
	}
	if (action.OriginalWorkspaceID == "") != (action.OriginalOperationID == "") {
		return util.CmdHandler(util.InfoMsg{Type: util.InfoTypeError, Msg: "The recorded OAuth identity is incomplete."})
	}
	if action.Abandon {
		return m.abandonOAuthLoginRecorded(action, r)
	}
	if action.OriginalOperationID != "" {
		found := false
		for _, result := range r.recorded {
			if result.OriginalWorkspaceID == action.OriginalWorkspaceID && result.OperationID == action.OriginalOperationID && result.State == "token-result-recorded" && !result.Abandoned {
				found = true
				break
			}
		}
		if !found {
			return util.CmdHandler(util.InfoMsg{Type: util.InfoTypeError, Msg: "The selected operation has no observed OAuth result to recover."})
		}
	}
	return m.startOAuthLogin(action.Dialog, r.recordedOwner, action.OriginalWorkspaceID, action.OriginalOperationID)
}
