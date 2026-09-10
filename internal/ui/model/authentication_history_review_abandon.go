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

type authenticationHistoryReviewAbandonPreparedMsg struct {
	state               *authenticationHistoryUI
	entry               *authenticationHistoryEntry
	attempt, generation uint64
	request             workspace.ProviderAuthenticationReviewAbandonRequest
	err                 error
}
type authenticationHistoryReviewAbandonedMsg struct {
	state   *authenticationHistoryUI
	entry   *authenticationHistoryEntry
	attempt uint64
	request workspace.ProviderAuthenticationReviewAbandonRequest
	outcome workspace.ProviderAuthenticationReviewAbandonOutcome
	err     error
}

func (m *UI) prepareHistoricalReviewAbandon(s *authenticationHistoryUI, e *authenticationHistoryEntry) tea.Cmd {
	if !m.authenticationHistoryCurrent(s) || e.review == nil || s.retired(e) || !e.review.ApplyAttempted || e.review.ApplyRequest == nil || e.review.JournalRevision == 0 || e.review.ApplyOutcome != nil && e.review.ApplyOutcome.Adopted {
		return nil
	}
	if _, ok := s.workspace.(workspace.ProviderAuthenticationReviewAbandoner); !ok {
		return util.ReportError(errors.New("attempted review abandonment is unavailable"))
	}
	e.preparing = true
	e.attempt++
	attempt := e.attempt
	request := workspace.ProviderAuthenticationReviewAbandonRequest{WorkspaceID: s.sourceID, Review: e.review.Request, PreviewID: e.review.ApplyRequest.PreviewID, Revision: e.review.JournalRevision}
	e.message = "Preparing an explicit decision to stop recovery of this attempted review. An earlier PUT may still have been accepted."
	m.showAuthenticationHistory(s)
	generation := s.dialog.Generation()
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		request.AbandonID = hex.EncodeToString(id[:])
		return authenticationHistoryReviewAbandonPreparedMsg{s, e, attempt, generation, request, err}
	}
}

func (m *UI) completeHistoricalReviewAbandonPreparation(msg authenticationHistoryReviewAbandonPreparedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.preparing || e.attempt != msg.attempt {
		return nil
	}
	e.preparing = false
	if !m.authenticationHistoryCurrent(s) || s.dialog.Generation() != msg.generation || s.dialog.SelectedKey() != e.key {
		e.message = "Review abandonment preparation canceled; no request was dispatched."
		m.showAuthenticationHistory(s)
		return nil
	}
	if msg.err != nil {
		e.message = msg.err.Error()
		m.showAuthenticationHistory(s)
		return util.ReportError(msg.err)
	}
	if e.review == nil || e.review.Request != msg.request.Review || e.review.JournalRevision != msg.request.Revision {
		return util.ReportError(providerauth.ErrStale)
	}
	return m.dispatchHistoricalReviewAbandon(s, e, msg.request)
}

func (m *UI) dispatchHistoricalReviewAbandon(s *authenticationHistoryUI, e *authenticationHistoryEntry, request workspace.ProviderAuthenticationReviewAbandonRequest) tea.Cmd {
	if !m.authenticationHistoryCurrent(s) || e.review == nil || e.review.ApplyRequest == nil || request.WorkspaceID != s.sourceID || request.Review != e.review.Request || request.PreviewID != e.review.ApplyRequest.PreviewID {
		return util.ReportError(providerauth.ErrStale)
	}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	capability, ok := s.workspace.(workspace.ProviderAuthenticationReviewAbandoner)
	if !ok {
		return util.ReportError(errors.New("attempted review abandonment is unavailable"))
	}
	e.reviewAbandon = &request
	e.pending = true
	e.attempt++
	attempt := e.attempt
	e.message = "Retiring recovery of the exact attempted review. Its original Apply outcome and any accepted PUT remain unchanged."
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.cancel = cancel
	m.showAuthenticationHistory(s)
	return func() tea.Msg {
		defer cancel()
		outcome, err := capability.AbandonProviderAuthenticationReview(ctx, request)
		return authenticationHistoryReviewAbandonedMsg{s, e, attempt, request, outcome, err}
	}
}

func (m *UI) completeHistoricalReviewAbandon(msg authenticationHistoryReviewAbandonedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.pending || e.attempt != msg.attempt || e.reviewAbandon == nil || *e.reviewAbandon != msg.request {
		return nil
	}
	e.pending, e.cancel = false, nil
	err := msg.err
	verified := false
	if msg.outcome.Abandoned || err == nil {
		validation := msg.outcome.Validate(msg.request)
		if validation == nil {
			if e.review.ApplyRequest == nil {
				validation = providerauth.ErrReceiptUnverified
			} else {
				validation = msg.outcome.Original.Validate(*e.review.ApplyRequest)
			}
		}
		if validation != nil {
			err = validation
		} else {
			verified = true
		}
	}
	if verified {
		e.review.Abandoned = true
		request := msg.request
		e.review.AbandonRequest = &request
		e.message = fmt.Sprintf("Recovery of the attempted review was explicitly abandoned. Its original Apply outcome is unchanged: recorded acknowledgement=%t; adoption=%t. An earlier PUT may have been accepted. Ctrl+L opens a separate saved-state choice.", msg.outcome.Original.RemoteAcknowledged, msg.outcome.Original.Adopted)
	} else {
		e.message = "Attempted-review abandonment remains unresolved. Use Actions to retry this exact request; refresh history before selecting a new revision."
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
