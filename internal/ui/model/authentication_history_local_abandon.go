package model

import (
	"context"
	"errors"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/util"
)

func (m *UI) dispatchHistoricalLocalAbandon(s *authenticationHistoryUI, e *authenticationHistoryEntry, retry bool) tea.Cmd {
	if !m.authenticationHistoryCurrent(s) || e.pending || e.preparing {
		return nil
	}
	request := providerauth.LocalRepairRequest{}
	if retry {
		if e.localAbandon == nil {
			return nil
		}
		request = *e.localAbandon
	} else {
		if e.repaired == nil {
			return util.ReportError(errors.New("review the original local repair record before choosing abandonment"))
		}
		summary := e.repaired.Summary
		if summary.Abandoned || summary.NoEffects || summary.Coherent || summary.NeedsReload {
			return util.ReportError(errors.New("this local record is already terminal; no abandonment is needed"))
		}
		request = providerauth.LocalRepairRequest{WorkspaceID: s.sourceID, OperationWorkspaceID: e.target().WorkspaceID, OperationID: e.operationID(), Revision: summary.Revision, Abandon: true}
	}
	if request.WorkspaceID != s.sourceID || request.OperationWorkspaceID != e.target().WorkspaceID || request.OperationID != e.operationID() || !request.Abandon {
		return util.ReportError(providerauth.ErrStale)
	}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	e.localAbandon = &request
	e.localAbandonPending = true
	e.pending = true
	e.attempt++
	attempt := e.attempt
	e.message = "Retiring only the reviewed local recovery intent. Any unknown exchange and prior writes remain unchanged."
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.cancel = cancel
	m.showAuthenticationHistory(s)
	ws := s.workspace
	return func() tea.Msg {
		defer cancel()
		result, err := ws.RepairLocalAuthentication(ctx, request)
		return authenticationHistoryRepairedMsg{s, e, attempt, request, result, err}
	}
}
