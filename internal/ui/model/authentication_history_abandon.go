package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type authenticationHistoryAbandonPreparedMsg struct {
	state               *authenticationHistoryUI
	entry               *authenticationHistoryEntry
	attempt, generation uint64
	request             workspace.ProviderAuthenticationAbandonRequest
	err                 error
}
type authenticationHistoryAbandonedMsg struct {
	state   *authenticationHistoryUI
	entry   *authenticationHistoryEntry
	attempt uint64
	request workspace.ProviderAuthenticationAbandonRequest
	outcome workspace.ProviderAuthenticationAbandonOutcome
	err     error
}

func (m *UI) prepareHistoricalPublicationAbandon(s *authenticationHistoryUI, e *authenticationHistoryEntry) tea.Cmd {
	if !m.authenticationHistoryCurrent(s) || e.operation == nil || e.operation.Abandoned || e.superseded() || e.operation.Adopted || e.operation.JournalRevision == 0 {
		return nil
	}
	if _, ok := s.workspace.(workspace.ProviderAuthenticationAbandoner); !ok {
		return util.ReportError(errors.New("original publication abandonment is unavailable"))
	}
	e.preparing = true
	e.attempt++
	attempt := e.attempt
	request := workspace.ProviderAuthenticationAbandonRequest{WorkspaceID: s.sourceID, Target: e.operation.Target, OperationID: e.operation.OperationID, Revision: e.operation.JournalRevision}
	e.message = "Preparing an explicit decision to stop original publication recovery. An earlier PUT may still have been accepted."
	m.showAuthenticationHistory(s)
	generation := s.dialog.Generation()
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		request.AbandonID = hex.EncodeToString(id[:])
		return authenticationHistoryAbandonPreparedMsg{s, e, attempt, generation, request, err}
	}
}

func (m *UI) completeHistoricalPublicationAbandonPreparation(msg authenticationHistoryAbandonPreparedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.preparing || e.attempt != msg.attempt {
		return nil
	}
	e.preparing = false
	if !m.authenticationHistoryCurrent(s) || s.dialog.Generation() != msg.generation || s.dialog.SelectedKey() != e.key {
		e.message = "Publication abandonment preparation canceled; no request was dispatched."
		m.showAuthenticationHistory(s)
		return nil
	}
	if msg.err != nil {
		e.message = msg.err.Error()
		m.showAuthenticationHistory(s)
		return util.ReportError(msg.err)
	}
	if e.operation == nil || e.operation.Target != msg.request.Target || e.operation.OperationID != msg.request.OperationID || e.operation.JournalRevision != msg.request.Revision {
		return util.ReportError(providerauth.ErrStale)
	}
	return m.dispatchHistoricalPublicationAbandon(s, e, msg.request)
}

func (m *UI) dispatchHistoricalPublicationAbandon(s *authenticationHistoryUI, e *authenticationHistoryEntry, request workspace.ProviderAuthenticationAbandonRequest) tea.Cmd {
	if !m.authenticationHistoryCurrent(s) || e.operation == nil || request.WorkspaceID != s.sourceID || request.Target != e.operation.Target || request.OperationID != e.operation.OperationID {
		return util.ReportError(providerauth.ErrStale)
	}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	capability, ok := s.workspace.(workspace.ProviderAuthenticationAbandoner)
	if !ok {
		return util.ReportError(errors.New("original publication abandonment is unavailable"))
	}
	e.publicationAbandon = &request
	e.pending = true
	e.attempt++
	attempt := e.attempt
	e.message = "Retiring the exact original publication recovery. Earlier local writes and any accepted PUT remain unchanged."
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.cancel = cancel
	m.showAuthenticationHistory(s)
	return func() tea.Msg {
		defer cancel()
		outcome, err := capability.AbandonProviderAuthentication(ctx, request)
		return authenticationHistoryAbandonedMsg{s, e, attempt, request, outcome, err}
	}
}

func (m *UI) completeHistoricalPublicationAbandon(msg authenticationHistoryAbandonedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.pending || e.attempt != msg.attempt || e.publicationAbandon == nil || *e.publicationAbandon != msg.request {
		return nil
	}
	e.pending, e.cancel = false, nil
	err := msg.err
	verified := false
	if msg.outcome.Abandoned || err == nil {
		validation := msg.outcome.Validate(msg.request)
		if validation == nil {
			validation = validateHistoricalAuthenticationOutcome(*e.operation, msg.outcome.Original)
		}
		if validation != nil {
			err = validation
		} else {
			verified = true
		}
	}
	if verified {
		// This is separate metadata. The original outcome is never overwritten.
		e.operation.Abandoned = true
		request := msg.request
		e.operation.AbandonRequest = &request
		e.message = fmt.Sprintf("Original publication recovery was explicitly abandoned. Recorded receiver acknowledgement=%t; adoption=%t. An earlier PUT may have been accepted. This action neither reverses it nor publishes a new runtime. Ctrl+L opens a separate saved-state choice.", msg.outcome.RemoteAcknowledged, msg.outcome.Adopted)
	} else {
		e.message = "Publication abandonment remains unresolved. Use Actions to retry this exact request; refresh history before choosing a new revision."
	}
	if err != nil {
		e.message += " " + err.Error()
	}
	m.retireHistoricalReconciliations(s)
	m.showAuthenticationHistory(s)
	notice := historyAuthenticationNotice(s, m.com.Workspace, msg.request.WorkspaceID, e.message)
	if err != nil {
		return util.ReportError(errors.New(notice))
	}
	return util.CmdHandler(util.NewInfoMsg(notice))
}

// A refreshed history marker fences already-restored dialogs too. Admitted
// replies remain deliverable; retirement never replaces their actual outcome.
func (m *UI) retireHistoricalReconciliations(history *authenticationHistoryUI) {
	for _, state := range m.authenticationReconciliations {
		if state.operation.workspace != history.workspace {
			continue
		}
		retired := false
		if !state.fresh {
			entry := history.entries[historyOperationKey(state.operation.row.Target.WorkspaceID, state.operation.id)]
			retired = entry != nil && entry.operation != nil && entry.operation.Abandoned
		}
		if state.request != nil {
			entry := history.entries[historyReviewKey(historyReviewTarget(*state.request).WorkspaceID, state.request.ReviewID)]
			retired = retired || entry != nil && entry.review != nil && entry.review.Request == *state.request && entry.superseded()
		}
		if retired && !state.historyRetired {
			state.historyRetired = true
			state.preparing = nil
			state.message = "This retained review was superseded or original recovery was abandoned. Its existing outcome remains unchanged; Saved Authentication starts a separate explicit choice."
			m.showAuthenticationReconciliation(state)
		}
	}
}
