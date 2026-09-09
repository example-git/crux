package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type authenticationHistoryUI struct {
	workspace workspace.Workspace
	historian workspace.ProviderAuthenticationHistorian
	dialog    *dialog.AuthenticationHistory
	sourceID  string
	attempt   uint64
	loading   bool
	cancel    context.CancelFunc
	entries   map[string]*authenticationHistoryEntry
	order     []string
	message   string
}
type authenticationHistoryEntry struct {
	key                 string
	operation           *workspace.ProviderAuthenticationHistoryOperation
	review              *workspace.ProviderAuthenticationHistoryReview
	coordinator         *authenticationOperation
	reconciliation      *authenticationReconciliation
	recovery            *workspace.ProviderAuthenticationRecoveryRequest
	recoverySequence    uint64
	repair              *providerauth.LocalRepairRequest
	repaired            *config.LocalAuthenticationRepairResult
	localAbandon        *providerauth.LocalRepairRequest
	localAbandonPending bool
	publicationAbandon  *workspace.ProviderAuthenticationAbandonRequest
	reviewAbandon       *workspace.ProviderAuthenticationReviewAbandonRequest
	preparing, pending  bool
	attempt             uint64
	cancel              context.CancelFunc
	message             string
}
type authenticationHistoryLoadedMsg struct {
	state    *authenticationHistoryUI
	attempt  uint64
	sourceID string
	history  workspace.ProviderAuthenticationHistory
	err      error
}
type authenticationHistoryPreparedMsg struct {
	state                         *authenticationHistoryUI
	entry                         *authenticationHistoryEntry
	attempt, generation, sequence uint64
	id                            string
	err                           error
}
type authenticationHistoryRecoveredMsg struct {
	state   *authenticationHistoryUI
	entry   *authenticationHistoryEntry
	attempt uint64
	request workspace.ProviderAuthenticationRecoveryRequest
	outcome providerauth.MutationOutcome
	err     error
}
type authenticationHistoryRepairedMsg struct {
	state   *authenticationHistoryUI
	entry   *authenticationHistoryEntry
	attempt uint64
	request providerauth.LocalRepairRequest
	result  config.LocalAuthenticationRepairResult
	err     error
}

func (m *UI) authenticationHistoryOpen(s *authenticationHistoryUI) bool {
	return s != nil && s.workspace == m.com.Workspace && m.dialog.Dialog(dialog.AuthenticationHistoryID) == s.dialog
}
func (m *UI) authenticationHistoryCurrent(s *authenticationHistoryUI) bool {
	return m.authenticationHistoryOpen(s) && s.sourceID != "" && s.workspace.AuthenticationWorkspaceID() == s.sourceID
}
func (m *UI) openAuthenticationHistory() tea.Cmd {
	historian, ok := m.com.Workspace.(workspace.ProviderAuthenticationHistorian)
	if !ok {
		return util.ReportError(errors.New("retained authentication publication history requires the owning client workspace"))
	}
	if m.authenticationHistories == nil {
		m.authenticationHistories = map[workspace.Workspace]*authenticationHistoryUI{}
	}
	s := m.authenticationHistories[m.com.Workspace]
	if s == nil {
		s = &authenticationHistoryUI{workspace: m.com.Workspace, historian: historian, dialog: dialog.NewAuthenticationHistory(m.com), entries: map[string]*authenticationHistoryEntry{}}
		m.authenticationHistories[m.com.Workspace] = s
	}
	m.dialog.CloseDialog(dialog.AuthenticationHistoryID)
	m.dialog.OpenDialog(s.dialog)
	if s.loading {
		m.showAuthenticationHistory(s)
		return nil
	}
	return m.readAuthenticationHistory(s)
}
func (m *UI) pruneAuthenticationHistory() {
	for _, s := range m.authenticationHistories {
		if !m.authenticationHistoryOpen(s) {
			if s.loading && s.cancel != nil {
				s.cancel()
			}
			for _, entry := range s.entries {
				entry.preparing = false
			}
			if s.workspace != m.com.Workspace && m.dialog.Dialog(dialog.AuthenticationHistoryID) == s.dialog {
				m.dialog.CloseDialog(dialog.AuthenticationHistoryID)
			}
		}
	}
}
func (m *UI) readAuthenticationHistory(s *authenticationHistoryUI) tea.Cmd {
	if s.loading {
		return nil
	}
	s.loading = true
	s.attempt++
	attempt := s.attempt
	s.message = "Reading original operations and retained reviews…"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	s.cancel = cancel
	m.showAuthenticationHistory(s)
	historian, ws := s.historian, s.workspace
	return func() tea.Msg {
		defer cancel()
		id := ws.AuthenticationWorkspaceID()
		history, err := historian.ProviderAuthenticationHistory(ctx)
		if ws.AuthenticationWorkspaceID() != id {
			err = errors.Join(err, providerauth.ErrStale)
		}
		return authenticationHistoryLoadedMsg{s, attempt, id, history, err}
	}
}
func (m *UI) completeAuthenticationHistory(msg authenticationHistoryLoadedMsg) tea.Cmd {
	s := msg.state
	if s == nil || m.authenticationHistories[s.workspace] != s || !s.loading || s.attempt != msg.attempt {
		return nil
	}
	s.loading, s.cancel = false, nil
	if msg.err != nil {
		s.message = "History unavailable: " + msg.err.Error()
		m.showAuthenticationHistory(s)
		return util.ReportError(errors.New(s.message))
	}
	if !m.authenticationHistoryOpen(s) || s.workspace.AuthenticationWorkspaceID() != msg.sourceID {
		s.message = "Workspace changed while reading history. Reopen history to read its retained operations."
		m.showAuthenticationHistory(s)
		return nil
	}
	if err := validateAuthenticationHistory(msg.history, msg.sourceID); err != nil {
		s.message = "History response was inconsistent: " + err.Error()
		m.showAuthenticationHistory(s)
		return util.ReportError(errors.New(s.message))
	}
	s.sourceID = msg.sourceID
	order := []string{}
	seen := map[string]bool{}
	for _, operation := range msg.history.Operations {
		key := historyOperationKey(operation.Target.WorkspaceID, operation.OperationID)
		entry := s.entries[key]
		if entry == nil {
			entry = &authenticationHistoryEntry{key: key}
			s.entries[key] = entry
		}
		if !entry.pending && !entry.preparing {
			copy := operation
			entry.operation = &copy
			if operation.AbandonRequest != nil {
				request := *operation.AbandonRequest
				entry.publicationAbandon = &request
			}
			entry.recoverySequence = max(entry.recoverySequence, operation.RecoverySequence)
			if operation.RecoveryRequest != nil {
				request := *operation.RecoveryRequest
				entry.recovery = &request
			}
		}
		order = append(order, key)
		seen[key] = true
	}
	for _, review := range msg.history.Reviews {
		key := historyReviewKey(historyReviewTarget(review.Request).WorkspaceID, review.Request.ReviewID)
		entry := s.entries[key]
		if entry == nil {
			entry = &authenticationHistoryEntry{key: key}
			s.entries[key] = entry
		}
		if !entry.pending && !entry.preparing {
			copy := review
			copy.Summary.Models = slices.Clone(review.Summary.Models)
			copy.Summary.ChangedCategories = slices.Clone(review.Summary.ChangedCategories)
			entry.review = &copy
			if review.AbandonRequest != nil {
				request := *review.AbandonRequest
				entry.reviewAbandon = &request
			}
		}
		order = append(order, key)
		seen[key] = true
	}
	// Commands already admitted remain reachable even if another read omitted
	// their row. Their own exact completion, not a new list, owns their outcome.
	for _, key := range s.order {
		if entry := s.entries[key]; !seen[key] && entry != nil && (entry.pending || entry.preparing || entry.reconciliation != nil) {
			order = append(order, key)
			seen[key] = true
		}
	}
	s.order = order
	s.message = "Select an original operation or review. History is evidence, not current credential authority."
	if len(order) == 0 {
		s.message = "No retained authentication operations or reviews for this owning client."
	}
	m.retireHistoricalReconciliations(s)
	m.showAuthenticationHistory(s)
	return nil
}
func historyOperationKey(workspaceID, id string) string {
	return fmt.Sprintf("operation:%q:%q", workspaceID, id)
}
func historyReviewKey(workspaceID, id string) string {
	return fmt.Sprintf("review:%q:%q", workspaceID, id)
}
func validateAuthenticationHistory(history workspace.ProviderAuthenticationHistory, sourceID string) error {
	seen := map[string]bool{}
	for _, entry := range history.Operations {
		key := historyOperationKey(entry.Target.WorkspaceID, entry.OperationID)
		if entry.OperationID == "" || seen[key] || entry.Target.Validate() != nil || entry.HistoricalWorkspace != (entry.Target.WorkspaceID != sourceID) {
			return providerauth.ErrReceiptUnverified
		}
		seen[key] = true
		if err := validateHistoricalAuthenticationOutcome(entry, entry.Outcome); err != nil {
			return err
		}
		if entry.Abandoned != (entry.AbandonRequest != nil) {
			return providerauth.ErrReceiptUnverified
		}
		if request := entry.AbandonRequest; request != nil {
			if request.Validate() != nil || request.OperationID != entry.OperationID || request.Target != entry.Target || request.Revision >= entry.JournalRevision {
				return providerauth.ErrReceiptUnverified
			}
		}
		if entry.Adopted && !entry.RemoteAcknowledged {
			return providerauth.ErrReceiptUnverified
		}
		if entry.RecoveryRequest != nil {
			r := entry.RecoveryRequest
			if r.Validate() != nil || r.OperationID != entry.OperationID || r.Target != entry.Target || r.RecoverySequence != entry.RecoverySequence {
				return providerauth.ErrReceiptUnverified
			}
		}
	}
	for _, entry := range history.Reviews {
		r := entry.Request
		key := historyReviewKey(historyReviewTarget(r).WorkspaceID, r.ReviewID)
		if r.Validate() != nil || seen[key] || entry.HistoricalWorkspace != (historyReviewTarget(r).WorkspaceID != sourceID) {
			return providerauth.ErrReceiptUnverified
		}
		seen[key] = true
		if entry.Abandoned != (entry.AbandonRequest != nil) || entry.ApplyAttempted && entry.ApplyRequest == nil {
			return providerauth.ErrReceiptUnverified
		}
		if request := entry.AbandonRequest; request != nil {
			if request.Validate() != nil || request.Review != r || request.Revision >= entry.JournalRevision || entry.ApplyRequest == nil || request.PreviewID != entry.ApplyRequest.PreviewID {
				return providerauth.ErrReceiptUnverified
			}
		}
		if entry.Summary.PreviewID != "" {
			if err := entry.Summary.Validate(r); err != nil {
				return err
			}
		}
		if entry.ApplyRequest != nil {
			a := *entry.ApplyRequest
			if a.Validate() != nil || a.OperationID != r.OperationID || a.OriginalTarget != r.OriginalTarget || a.FreshSaved != r.FreshSaved || a.SavedTarget != r.SavedTarget || a.ReviewID != r.ReviewID || entry.Summary.PreviewID != "" && a.PreviewID != entry.Summary.PreviewID {
				return providerauth.ErrReceiptUnverified
			}
			if entry.ApplyOutcome != nil && entry.ApplyOutcome.ApplyID != "" {
				if err := entry.ApplyOutcome.Validate(a); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
func validateHistoricalAuthenticationOutcome(original workspace.ProviderAuthenticationHistoryOperation, outcome providerauth.MutationOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	if outcome.OperationID != original.OperationID || outcome.Previous != original.Target || outcome.CheckID != original.Outcome.CheckID || outcome.CredentialID != original.Outcome.CredentialID || outcome.LoginID != original.Outcome.LoginID || outcome.RemovedAccountID != original.Outcome.RemovedAccountID {
		return providerauth.ErrReceiptUnverified
	}
	return nil
}
func historyReviewTarget(r workspace.ProviderAuthenticationReviewRequest) providerauth.Target {
	if r.FreshSaved {
		return r.SavedTarget
	}
	return r.OriginalTarget
}
func (e *authenticationHistoryEntry) target() providerauth.Target {
	if e.operation != nil {
		return e.operation.Target
	}
	if e.review != nil {
		return historyReviewTarget(e.review.Request)
	}
	return providerauth.Target{}
}
func (e *authenticationHistoryEntry) historical(sourceID string) bool {
	return e.target().WorkspaceID != sourceID || e.operation != nil && e.operation.HistoricalWorkspace || e.review != nil && e.review.HistoricalWorkspace
}
func (e *authenticationHistoryEntry) superseded() bool {
	return e.operation != nil && (e.operation.Abandoned || e.operation.ReconciledBy != "" || e.operation.SavedStateSupersededBy != "") || e.review != nil && (e.review.Abandoned || e.review.SavedStateSupersededBy != "" || e.review.SupersededByReview != "" || e.review.OriginalAbandonedBy != "")
}
func (s *authenticationHistoryUI) retired(e *authenticationHistoryEntry) bool {
	if e.superseded() {
		return true
	}
	if e.review != nil && !e.review.Request.FreshSaved {
		original := s.entries[historyOperationKey(e.review.Request.OriginalTarget.WorkspaceID, e.review.Request.OperationID)]
		return original != nil && original.operation != nil && original.operation.Abandoned
	}
	return false
}
func (e *authenticationHistoryEntry) operationID() string {
	if e.operation != nil {
		return e.operation.OperationID
	}
	if e.review != nil {
		return e.review.Request.OperationID
	}
	return ""
}
func (m *UI) showAuthenticationHistory(s *authenticationHistoryUI) {
	if s == nil || s.dialog == nil {
		return
	}
	rows := make([]dialog.AuthenticationHistoryRow, 0, len(s.order))
	selectedBusy := false
	for _, key := range s.order {
		e := s.entries[key]
		if e == nil {
			continue
		}
		old := e.historical(s.sourceID)
		busy := e.pending || e.preparing
		if e.reconciliation != nil {
			busy = busy || e.reconciliation.pending != "" || e.reconciliation.preparing != nil
		}
		label, details := "", ""
		if o := e.operation; o != nil {
			kind := "account change"
			switch {
			case o.Outcome.LoginID != "":
				kind = "OAuth login"
			case o.Outcome.CheckID != "":
				kind = "credential save"
			case o.Outcome.RemovedAccountID != "":
				kind = "account removal"
			}
			label = o.Target.Owner.ProviderID + " · " + kind + " · " + o.OperationID
			p := o.Outcome.Progress
			details = fmt.Sprintf("Original workspace: %s\nOriginal operation: %s\nLocal finished: %t\nOriginal progress: refreshed=%t, accounts=%t, config=%t, local runtime=%t\nRemote acknowledged=%t; adopted=%t\nRecovery sequence=%d; review sequence=%d", o.Target.WorkspaceID, o.OperationID, o.LocalFinished, p.AccountRefreshed, p.AccountsSaved, p.ConfigSaved, p.RuntimePublished, o.RemoteAcknowledged, o.Adopted, max(o.RecoverySequence, e.recoverySequence), o.ReviewSequence)
			details += fmt.Sprintf("\nJournal revision=%d; original recovery abandoned=%t", o.JournalRevision, o.Abandoned)
			if o.ReconciledBy != "" {
				details += "\nSeparate reviewed action: " + o.ReconciledBy
			}
			if o.SavedStateSupersededBy != "" {
				details += "\nSuperseded by fresh saved action: " + o.SavedStateSupersededBy
			}
		} else if r := e.review; r != nil {
			label = e.target().Owner.ProviderID + " · retained review · " + r.Request.ReviewID
			details = fmt.Sprintf("Workspace: %s\nReview: %s\nOriginal operation: %s\nFresh saved intent: %t\nReview sequence: %d", e.target().WorkspaceID, r.Request.ReviewID, r.Request.OperationID, r.Request.FreshSaved, r.Request.ReviewSequence)
			details += fmt.Sprintf("\nJournal revision=%d; publication attempted=%t; review recovery abandoned=%t", r.JournalRevision, r.ApplyAttempted, r.Abandoned)
			if r.Summary.PreviewID != "" {
				details += "\n" + authenticationReviewSummaryText(r.Summary)
			}
			if r.ApplyRequest != nil {
				details += "\nApply request: " + r.ApplyRequest.ApplyID
			}
			if r.ApplyOutcome != nil && r.ApplyOutcome.ApplyID != "" {
				details += fmt.Sprintf("\nRecorded apply acknowledgement=%t; adoption=%t", r.ApplyOutcome.RemoteAcknowledged, r.ApplyOutcome.Adopted)
			} else if r.ApplyRequest != nil {
				details += "\nApply result was not recorded; receiver acknowledgement is unknown."
			}
			if r.SupersededByReview != "" {
				details += "\nSuperseded by retained review: " + r.SupersededByReview
			}
			if r.OriginalAbandonedBy != "" {
				details += "\nOriginal publication recovery abandoned by: " + r.OriginalAbandonedBy
			}
			if r.SavedStateSupersededBy != "" {
				details += "\nSuperseded by fresh saved action: " + r.SavedStateSupersededBy
			}
		}
		if old {
			label = "Previous workspace · " + label
			details += "\nPrevious workspace record: recovery and apply cannot target the new workspace. Original local repair and fresh saved-state review remain separate actions."
		}
		owner := e.target().Owner
		details += fmt.Sprintf("\nProvider: %s; construction: %s", owner.ProviderID, owner.Construction)
		if owner.HasOAuth {
			details += fmt.Sprintf("\nOAuth adapter: %s; flow: %s", owner.OAuthAdapter, owner.OAuthFlowID)
		}
		if owner.HasManifest {
			details += fmt.Sprintf("\nManifest: %s @ %s", owner.ManifestID, owner.ManifestVersion)
		}
		if owner.HasPreset {
			details += fmt.Sprintf("\nPreset: %s @ %s; digest: %s", owner.PresetID, owner.PresetVersion, owner.PresetDigest)
		}
		details += "\n" + e.message
		if e.reconciliation != nil {
			details += "\nReview controller: " + e.reconciliation.message
		}
		recover := e.operation != nil && !old && !e.superseded()
		repairable, abandonable := false, false
		if e.repaired != nil {
			summary := e.repaired.Summary
			repairable = !summary.Abandoned && !summary.NoEffects && (summary.Coherent || summary.NeedsReload || summary.RepairReady && (!summary.RefreshStarted || summary.RefreshObserved))
			abandonable = !summary.Abandoned && !summary.NoEffects && !summary.Coherent && !summary.NeedsReload
		}
		rows = append(rows, dialog.AuthenticationHistoryRow{Key: key, Label: label, Details: details, Review: !old && !s.retired(e) && !(e.operation != nil && e.operation.Adopted) && !busy, Recover: recover && !busy, RetryRecovery: recover && e.recovery != nil && !busy, Repair: e.operationID() != "" && !busy, ApplyRepair: e.repair != nil && repairable && !busy, AbandonLocal: abandonable && !busy, RetryAbandonLocal: e.localAbandon != nil && e.localAbandon.WorkspaceID == s.sourceID && !busy, AbandonPublication: e.operation != nil && !e.operation.Adopted && !e.superseded() && e.operation.JournalRevision != 0 && !busy, RetryAbandonPublication: e.publicationAbandon != nil && e.publicationAbandon.WorkspaceID == s.sourceID && !busy, AbandonReview: e.review != nil && e.review.ApplyAttempted && e.review.ApplyRequest != nil && e.review.JournalRevision != 0 && !(e.review.ApplyOutcome != nil && e.review.ApplyOutcome.Adopted) && !s.retired(e) && !busy, RetryAbandonReview: e.reviewAbandon != nil && e.reviewAbandon.WorkspaceID == s.sourceID && !busy})
		if key == s.dialog.SelectedKey() {
			selectedBusy = busy
		}
	}
	s.dialog.SetRows(rows)
	s.dialog.SetState(s.message, s.loading || selectedBusy)
}
func (m *UI) handleAuthenticationHistory(action dialog.ActionAuthenticationHistory) tea.Cmd {
	s := m.authenticationHistories[m.com.Workspace]
	if !m.authenticationHistoryOpen(s) || s.dialog != action.Dialog || s.dialog.Generation() != action.Generation {
		return nil
	}
	if action.Kind == "cancel" {
		if s.cancel != nil {
			s.cancel()
		}
		if e := s.entries[action.Key]; e != nil {
			e.preparing = false
			if e.cancel != nil {
				e.cancel()
			}
			e.message = "Wait cancellation requested. Any admitted write/publication retains its original receipt."
		}
		m.showAuthenticationHistory(s)
		return nil
	}
	if action.Kind == "refresh" {
		return m.readAuthenticationHistory(s)
	}
	if action.Kind == "saved" {
		return m.openSavedAuthentication()
	}
	if s.loading || !m.authenticationHistoryCurrent(s) {
		return util.ReportError(errors.New("workspace changed; read authentication history again"))
	}
	e := s.entries[action.Key]
	if e == nil || action.Key != s.dialog.SelectedKey() {
		return nil
	}
	if action.Kind == "open" {
		s.dialog.ShowDetails()
		return nil
	}
	if e.pending || e.preparing || e.reconciliation != nil && (e.reconciliation.pending != "" || e.reconciliation.preparing != nil) {
		return nil
	}
	switch action.Kind {
	case "review":
		return m.openHistoricalAuthenticationReview(s, e)
	case "recover":
		return m.prepareHistoricalAuthenticationRecovery(s, e)
	case "retry-recovery":
		if e.recovery != nil {
			return m.dispatchHistoricalAuthenticationRecovery(s, e, *e.recovery)
		}
	case "repair":
		return m.dispatchHistoricalAuthenticationRepair(s, e, false)
	case "abandon-review":
		return m.prepareHistoricalReviewAbandon(s, e)
	case "retry-abandon-review":
		if e.reviewAbandon != nil {
			return m.dispatchHistoricalReviewAbandon(s, e, *e.reviewAbandon)
		}
	case "abandon-publication":
		return m.prepareHistoricalPublicationAbandon(s, e)
	case "retry-abandon-publication":
		if e.publicationAbandon != nil {
			return m.dispatchHistoricalPublicationAbandon(s, e, *e.publicationAbandon)
		}
	case "abandon-local":
		return m.dispatchHistoricalLocalAbandon(s, e, false)
	case "retry-abandon-local":
		return m.dispatchHistoricalLocalAbandon(s, e, true)
	case "apply-repair":
		return m.dispatchHistoricalAuthenticationRepair(s, e, true)
	}
	return nil
}
func (m *UI) openHistoricalAuthenticationReview(s *authenticationHistoryUI, e *authenticationHistoryEntry) tea.Cmd {
	if e.historical(s.sourceID) {
		return util.ReportError(errors.New("the retained review belongs to the previous workspace; choose fresh saved authentication"))
	}
	if s.retired(e) {
		return util.ReportError(errors.New("a separate saved-state action superseded this record; its details remain available and Saved Authentication opens a fresh review"))
	}
	if e.operation != nil && e.operation.Adopted {
		return util.ReportError(errors.New("the original publication was already adopted; its receipt remains readable in history"))
	}
	capability, ok := s.workspace.(workspace.ProviderAuthenticationReconciler)
	if !ok || !capability.CanReconcileProviderAuthentication() {
		return util.ReportError(errors.New("saved authentication review is unavailable for this workspace"))
	}
	if e.coordinator == nil {
		e.coordinator = &authenticationOperation{workspace: s.workspace, id: e.operationID(), row: dialog.AuthenticationRow{Target: e.target()}, message: "Retained history: " + e.message}
	}
	if m.authenticationReconciliations == nil {
		m.authenticationReconciliations = map[*authenticationOperation]*authenticationReconciliation{}
	}
	state := e.reconciliation
	if state != nil && state.historyRetired && e.operation != nil && !s.retired(e) {
		// The old review remains in its own history row. Opening the original
		// operation is a separate choice to prepare another preview, preserving
		// sequence ordering and never replaying its account mutation.
		state = &authenticationReconciliation{operation: e.coordinator, capability: capability, historySourceID: s.sourceID, sequence: state.sequence, message: "The previous review was retired. Choose an explicit effect and press Enter for a separate new preview; the original operation is unchanged."}
		e.reconciliation = state
		m.authenticationReconciliations[e.coordinator] = state
	}
	if state == nil {
		state = &authenticationReconciliation{operation: e.coordinator, capability: capability, historySourceID: s.sourceID, message: "Original history restored. Review and Apply are explicit; no original account change is repeated."}
		if e.operation != nil {
			state.sequence = e.operation.ReviewSequence
			e.coordinator.message = authenticationProgressText(e.operation.Outcome.Progress)
		}
		if h := e.review; h != nil {
			request := h.Request
			state.request = &request
			state.fresh = request.FreshSaved
			state.sequence = request.ReviewSequence
			if original := s.entries[historyOperationKey(request.OriginalTarget.WorkspaceID, request.OperationID)]; original != nil && original.operation != nil {
				state.sequence = max(state.sequence, original.operation.ReviewSequence)
			}
			if h.Summary.Validate(request) == nil {
				summary := h.Summary
				state.summary = &summary
			}
			if h.ApplyRequest != nil {
				apply := *h.ApplyRequest
				state.apply = &apply
			}
			if h.ApplyOutcome != nil && state.apply != nil && h.ApplyOutcome.Validate(*state.apply) == nil {
				outcome := *h.ApplyOutcome
				state.outcome = &outcome
				state.resolved = outcome.Adopted
			}
			state.message = "Restored retained review/apply identities. Ctrl+R retries this review; Ctrl+T retries an existing apply. Enter creates a separate new review."
		}
		e.reconciliation = state
		m.authenticationReconciliations[e.coordinator] = state
	}
	for _, other := range s.entries {
		if other.target() != e.target() {
			continue
		}
		if state.fresh {
			if other.review != nil && other.review.Request.FreshSaved {
				state.sequence = max(state.sequence, other.review.Request.ReviewSequence)
			}
		} else if other.operationID() == e.operationID() {
			if other.operation != nil {
				state.sequence = max(state.sequence, other.operation.ReviewSequence)
			}
			if other.review != nil {
				state.sequence = max(state.sequence, other.review.Request.ReviewSequence)
			}
		}
	}
	if state.pending != "" || state.preparing != nil {
		return nil
	}
	choice := workspace.ProviderAuthenticationReviewChoice{}
	if state.request != nil {
		choice = state.request.Choice
	}
	if state.dialog != nil {
		choice = state.dialog.Choice()
	}
	d := dialog.NewAuthenticationReconciliation(m.com, e.coordinator.message, e.coordinator.accountChoices)
	if state.fresh {
		slots := []providerauth.CredentialSlot{}
		if choice.CredentialID != "" {
			slots = append(slots, providerauth.CredentialSlot{ID: choice.CredentialID, Configured: true})
		}
		d.SetFreshSaved(slots)
	}
	d.SetChoice(choice)
	state.dialog = d
	m.dialog.CloseDialog(dialog.AuthenticationReconciliationID)
	m.dialog.OpenDialog(d)
	m.showAuthenticationReconciliation(state)
	return nil
}
func (m *UI) prepareHistoricalAuthenticationRecovery(s *authenticationHistoryUI, e *authenticationHistoryEntry) tea.Cmd {
	if e.operation == nil || e.historical(s.sourceID) || e.superseded() {
		return util.ReportError(providerauth.ErrStale)
	}
	if current := m.authenticationOperations[s.workspace]; current != nil && current.id == e.operationID() && current.row.Target == e.target() && current.recovery != nil {
		e.recoverySequence = max(e.recoverySequence, current.recovery.RecoverySequence)
	}
	if e.recoverySequence == math.MaxUint64 {
		return util.ReportError(errors.New("authentication recovery sequence exhausted"))
	}
	e.preparing = true
	e.attempt++
	attempt, sequence := e.attempt, e.recoverySequence+1
	e.message = "Preparing an explicit recovery attempt for the original operation."
	m.showAuthenticationHistory(s)
	generation := s.dialog.Generation()
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		return authenticationHistoryPreparedMsg{s, e, attempt, generation, sequence, hex.EncodeToString(id[:]), err}
	}
}
func (m *UI) completeHistoricalAuthenticationPreparation(msg authenticationHistoryPreparedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.preparing || e.attempt != msg.attempt {
		return nil
	}
	e.preparing = false
	if !m.authenticationHistoryCurrent(s) || s.dialog.Generation() != msg.generation || s.dialog.SelectedKey() != e.key {
		e.message = "Recovery preparation canceled; no request was dispatched."
		m.showAuthenticationHistory(s)
		return nil
	}
	if msg.err != nil {
		e.message = msg.err.Error()
		m.showAuthenticationHistory(s)
		return util.ReportError(msg.err)
	}
	request := workspace.ProviderAuthenticationRecoveryRequest{RecoveryID: msg.id, OperationID: e.operation.OperationID, Target: e.operation.Target, RecoverySequence: msg.sequence}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	e.recovery = &request
	e.recoverySequence = request.RecoverySequence
	return m.dispatchHistoricalAuthenticationRecovery(s, e, request)
}
func (m *UI) dispatchHistoricalAuthenticationRecovery(s *authenticationHistoryUI, e *authenticationHistoryEntry, request workspace.ProviderAuthenticationRecoveryRequest) tea.Cmd {
	capability, ok := s.workspace.(workspace.ProviderAuthenticationRecoverer)
	if !ok || !capability.CanRecoverProviderAuthentication() {
		return util.ReportError(errors.New("original publication recovery is unavailable"))
	}
	if e.operation == nil || e.historical(s.sourceID) || e.superseded() || request.Validate() != nil || request.Target != e.operation.Target || request.OperationID != e.operation.OperationID || request.Target.WorkspaceID != s.sourceID {
		return util.ReportError(providerauth.ErrStale)
	}
	e.pending = true
	e.attempt++
	attempt := e.attempt
	e.message = "Waiting for the exact original publication recovery receipt."
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	e.cancel = cancel
	m.showAuthenticationHistory(s)
	return func() tea.Msg {
		defer cancel()
		outcome, err := capability.RecoverProviderAuthentication(ctx, request)
		return authenticationHistoryRecoveredMsg{s, e, attempt, request, outcome, err}
	}
}
func (m *UI) completeHistoricalAuthenticationRecovery(msg authenticationHistoryRecoveredMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.pending || e.attempt != msg.attempt || e.recovery == nil || *e.recovery != msg.request {
		return nil
	}
	e.pending, e.cancel = false, nil
	err := validateHistoricalAuthenticationOutcome(*e.operation, msg.outcome)
	if err == nil {
		err = msg.err
	}
	if err != nil {
		e.message = "Recovery remains unresolved: " + err.Error() + ". Alt+T retries this exact recovery; Enter opens saved-state review."
	} else if msg.outcome.Superseded {
		e.message = "The original result is historical. It does not authorize adopting the current saved state."
	} else if msg.outcome.Change == nil {
		err = providerauth.ErrReceiptUnverified
		e.message = "Recovery returned no complete original receipt."
	} else {
		e.message = "The original publication was acknowledged and adopted. No account mutation or model-selection continuation was repeated."
	}
	m.showAuthenticationHistory(s)
	if err != nil {
		return util.ReportError(errors.New(historyAuthenticationNotice(s, m.com.Workspace, msg.request.Target.WorkspaceID, e.message)))
	}
	if s.workspace != m.com.Workspace || s.workspace.AuthenticationWorkspaceID() != msg.request.Target.WorkspaceID {
		return util.CmdHandler(util.NewInfoMsg("Previous workspace: " + e.message))
	}
	if msg.outcome.Change != nil && !msg.outcome.Superseded {
		m.invalidateBusyCaches()
		m.providerUsage = nil
		m.usageFetchGen++
		return tea.Batch(util.CmdHandler(util.NewInfoMsg(e.message)), m.dispatchBusyRefresh(), authenticationUsageCommand(s.workspace, m.usageFetchGen))
	}
	return util.CmdHandler(util.NewInfoMsg(e.message))
}
func (m *UI) dispatchHistoricalAuthenticationRepair(s *authenticationHistoryUI, e *authenticationHistoryEntry, apply bool) tea.Cmd {
	if e.operationID() == "" {
		return util.ReportError(errors.New("fresh saved reviews have no original local operation to repair"))
	}
	request := providerauth.LocalRepairRequest{WorkspaceID: s.sourceID, OperationWorkspaceID: e.target().WorkspaceID, OperationID: e.operationID()}
	if apply {
		if e.repair == nil || e.repaired == nil {
			return nil
		}
		if e.repaired.Summary.Abandoned || e.repaired.Summary.NoEffects {
			return util.ReportError(errors.New("this original local recovery is terminal; choose fresh saved authentication"))
		}
		request = *e.repair
		if !request.Apply {
			request.Apply = true
			request.Revision = e.repaired.Summary.Revision
		}
	}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	if !apply {
		e.repaired = nil // Only this new explicit review may authorize its apply.
	}
	e.repair = &request
	e.localAbandonPending = false
	e.pending = true
	e.attempt++
	attempt := e.attempt
	e.message = "Reading the original local disk repair record."
	if apply {
		e.message = "Applying only the reviewed fixed local disk repair; no runtime is published."
	}
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
func (m *UI) completeHistoricalAuthenticationRepair(msg authenticationHistoryRepairedMsg) tea.Cmd {
	s, e := msg.state, msg.entry
	if s == nil || e == nil || m.authenticationHistories[s.workspace] != s || s.entries[e.key] != e || !e.pending || e.attempt != msg.attempt {
		return nil
	}
	expected := e.repair
	if e.localAbandonPending {
		expected = e.localAbandon
	}
	if expected == nil || *expected != msg.request {
		return nil
	}
	e.pending, e.cancel = false, nil
	response := proto.ProviderLocalRepairResponse{Request: msg.request, Result: msg.result}
	if msg.err != nil {
		response.Error = proto.NewProviderLocalRepairError(msg.err)
	}
	err := response.Validate(msg.request)
	summary := msg.result.Summary
	if err == nil && summary.OperationID != "" && summary.ProviderID != e.target().Owner.ProviderID {
		err = providerauth.ErrReceiptUnverified
	}
	if err == nil {
		if summary.OperationID != "" {
			result := msg.result
			e.repaired = &result
		}
		err = msg.err
	}
	if err != nil {
		e.message = "Local repair remains unresolved: " + err.Error() + ". Ctrl+Y retries the retained apply; Ctrl+P explicitly reviews current repair progress."
		if msg.request.Abandon {
			e.message = "Local abandonment remains unresolved: " + err.Error() + ". The Actions menu retries this exact abandonment; Ctrl+P explicitly reads the current local record."
		}
	} else if msg.result.Summary.Abandoned {
		e.message = "Original local recovery was explicitly abandoned. Unknown exchange and original progress remain unchanged; abandonment repaired no files and published no runtime. Ctrl+L opens a separate saved-state choice."
	} else if msg.result.Summary.NoEffects {
		e.message = "The local save attempt finished without an account refresh or staged disk write. No repair is needed. Ctrl+L opens a separate saved-state choice."
	} else if msg.result.NeedsReload || summary.NeedsReload {
		e.message = "Original disk progress is retained. Ctrl+L opens Saved Authentication for explicit Reload, then a fresh review and Apply. This is separate from the original operation."
	} else if summary.RefreshStarted && !summary.RefreshObserved {
		e.message = "The original account refresh may have consumed its token; no returned successor was recorded. It will not be exchanged again. The Actions menu can explicitly abandon original local recovery."
	} else if !summary.RepairReady {
		e.message = "No fixed local write was staged. The Actions menu can explicitly abandon this retained local recovery; Saved Authentication starts a separate choice."
	} else {
		e.message = fmt.Sprintf("Local repair revision %d reviewed. Ctrl+Y applies this exact fixed disk operation; original runtime/acknowledgement progress is unchanged.", summary.Revision)
	}
	if e.repaired != nil {
		r := e.repaired
		e.message += fmt.Sprintf("\nPrior repair writes: accounts=%t, config=%t. Original progress: refreshed=%t, accounts=%t, config=%t, runtime=%t.", r.Summary.RepairAccountsWritten, r.Summary.RepairConfigWritten, r.Summary.Original.AccountRefreshed, r.Summary.Original.AccountsSaved, r.Summary.Original.ConfigSaved, r.Summary.Original.RuntimePublished)
		e.message += fmt.Sprintf("\nThis action wrote: accounts=%t, config=%t; matching saved files: accounts=%t, config=%t.", r.AccountsWritten, r.ConfigWritten, r.AccountsMatched, r.ConfigMatched)
	}
	m.showAuthenticationHistory(s)
	if err != nil {
		return util.ReportError(errors.New(historyAuthenticationNotice(s, m.com.Workspace, msg.request.WorkspaceID, e.message)))
	}
	return util.CmdHandler(util.NewInfoMsg(historyAuthenticationNotice(s, m.com.Workspace, msg.request.WorkspaceID, e.message)))
}
func historyAuthenticationNotice(s *authenticationHistoryUI, current workspace.Workspace, sourceID, message string) string {
	if s.workspace != current || s.workspace.AuthenticationWorkspaceID() != sourceID {
		return "Previous workspace: " + message
	}
	return strings.TrimSpace(message)
}
