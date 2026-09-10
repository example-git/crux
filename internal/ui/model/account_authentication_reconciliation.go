package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type authenticationReconciliation struct {
	fresh             bool
	historyRetired    bool
	historySourceID   string // Set only for a controller restored from retained history.
	operation         *authenticationOperation
	oauthLogin        *oauthLoginOperation
	dialog            *dialog.AuthenticationReconciliation
	capability        workspace.ProviderAuthenticationReconciler
	sequence, attempt uint64
	preparing         *authenticationReconciliationPreparation
	request           *workspace.ProviderAuthenticationReviewRequest
	summary           *workspace.ProviderAuthenticationReviewSummary
	apply             *workspace.ProviderAuthenticationApplyRequest
	outcome           *workspace.ProviderAuthenticationReconciliationOutcome
	pending           string
	delivered         bool
	resolved          bool
	choiceChanged     bool
	cancel            context.CancelFunc
	message           string
}
type authenticationReconciliationPreparation struct {
	dialog     *dialog.AuthenticationReconciliation
	generation uint64
	kind       string
	choice     workspace.ProviderAuthenticationReviewChoice
}
type authenticationReconciliationPreparedMsg struct {
	state       *authenticationReconciliation
	preparation *authenticationReconciliationPreparation
	id          string
	err         error
}
type authenticationReviewCompletedMsg struct {
	state   *authenticationReconciliation
	attempt uint64
	request workspace.ProviderAuthenticationReviewRequest
	summary workspace.ProviderAuthenticationReviewSummary
	err     error
}
type authenticationApplyCompletedMsg struct {
	state   *authenticationReconciliation
	attempt uint64
	request workspace.ProviderAuthenticationApplyRequest
	outcome workspace.ProviderAuthenticationReconciliationOutcome
	err     error
}

func (m *UI) authenticationReconciliationBusy(operation *authenticationOperation) bool {
	s := m.authenticationReconciliations[operation]
	return s != nil && (s.preparing != nil || s.pending != "")
}

func (m *UI) authenticationReconciliationOpen(s *authenticationReconciliation) bool {
	return s != nil && s.dialog != nil && m.dialog.Dialog(dialog.AuthenticationReconciliationID) == s.dialog
}

func (m *UI) authenticationReconciliationCurrent(s *authenticationReconciliation) bool {
	return s != nil && s.operation.workspace == m.com.Workspace && (s.historySourceID == "" || s.operation.workspace.AuthenticationWorkspaceID() == s.historySourceID)
}

func (m *UI) pruneAuthenticationReconciliations() {
	for _, s := range m.authenticationReconciliations {
		if !m.authenticationReconciliationCurrent(s) && m.authenticationReconciliationOpen(s) {
			m.dialog.CloseDialog(dialog.AuthenticationReconciliationID)
		}
		if s.preparing != nil && (!m.authenticationReconciliationCurrent(s) || !m.authenticationReconciliationOpen(s)) {
			s.preparing = nil
			s.message = "Preparation cancelled; no review or apply was dispatched. The original receipt remains available."
			m.showAuthenticationReconciliation(s)
		}
	}
}

func (m *UI) openAuthenticationReconciliation(action dialog.ActionAuthenticationReviewOpen) tea.Cmd {
	if !m.authenticationDialogOpen(action.Dialog) {
		return nil
	}
	operation := m.authenticationOperations[m.com.Workspace]
	if operation == nil || operation.pending || operation.preparing || operation.recoveryPreparing != nil {
		return nil
	}
	return m.openAuthenticationReconciliationOperation(operation)
}

func (m *UI) openOAuthLoginReconciliation(action dialog.ActionOAuthLoginReview) tea.Cmd {
	op := m.oauthLogins[m.com.Workspace]
	if op == nil || op.dialog != action.Dialog || !m.oauthDialogOpen(action.Dialog) || !op.completeSent || op.busy || op.preparing || op.relayBusy || oauthLoginReviewBusy(op) || oauthLoginReviewResolved(op) {
		return nil
	}
	var operation *authenticationOperation
	if op.review != nil {
		operation = op.review.operation
	} else {
		// This coordinator carries the original login identity only. It is
		// never installed in authenticationOperations or dispatched as a switch.
		operation = &authenticationOperation{workspace: op.workspace, id: op.ref.OperationID, row: dialog.AuthenticationRow{Target: op.ref.Target}, message: op.message}
		if op.outcome.Change != nil {
			for _, account := range op.outcome.Change.Current.Accounts {
				operation.accountChoices = append(operation.accountChoices, dialog.AuthenticationRow{Target: op.ref.Target, AccountID: account.ID, Label: account.DisplayName})
			}
		}
	}
	command := m.openAuthenticationReconciliationOperation(operation)
	if state := m.authenticationReconciliations[operation]; state != nil {
		state.oauthLogin, op.review = op, state
		m.showOAuthLogin(op)
	}
	return command
}

func (m *UI) openAuthenticationReconciliationOperation(operation *authenticationOperation) tea.Cmd {
	capability, ok := operation.workspace.(workspace.ProviderAuthenticationReconciler)
	if !ok || !capability.CanReconcileProviderAuthentication() {
		return util.CmdHandler(util.InfoMsg{Type: util.InfoTypeError, Msg: "Saved authentication review/apply is available only in the owning client workspace. Server-owned and local workspaces are not supported; no alternative action was run."})
	}
	if m.authenticationReconciliations == nil {
		m.authenticationReconciliations = map[*authenticationOperation]*authenticationReconciliation{}
	}
	s := m.authenticationReconciliations[operation]
	if s == nil {
		s = &authenticationReconciliation{operation: operation, capability: capability, message: "Choose an explicit saved-state effect, then press Enter to review it. No files will be repaired or reloaded."}
		m.authenticationReconciliations[operation] = s
	}
	choice := workspace.ProviderAuthenticationReviewChoice{}
	if s.dialog != nil {
		choice = s.dialog.Choice()
	}
	d := dialog.NewAuthenticationReconciliation(m.com, operation.message, operation.accountChoices)
	d.SetChoice(choice)
	s.dialog = d
	m.dialog.CloseDialog(dialog.AuthenticationReconciliationID)
	m.dialog.OpenDialog(d)
	m.showAuthenticationReconciliation(s)
	return nil
}

func (m *UI) showAuthenticationReconciliation(s *authenticationReconciliation) {
	if s == nil || s.dialog == nil {
		return
	}
	matching := !s.choiceChanged && s.request != nil && s.request.Choice == s.dialog.Choice()
	preview := ""
	if matching && s.summary != nil {
		preview = authenticationReviewSummaryText(*s.summary)
	}
	adopted := s.resolved
	s.dialog.SetFinished(adopted)
	s.dialog.SetRetired(s.historyRetired)
	s.dialog.SetState(s.message, preview, s.preparing != nil || s.pending != "", matching && !adopted, matching && s.summary != nil && s.apply == nil && !adopted, matching && s.apply != nil && !adopted)
	if s.oauthLogin != nil {
		m.showOAuthLogin(s.oauthLogin)
	}
	for _, history := range m.authenticationHistories {
		for _, entry := range history.entries {
			if entry.reconciliation == s {
				m.showAuthenticationHistory(history)
				break
			}
		}
	}
}

func authenticationReviewSummaryText(s workspace.ProviderAuthenticationReviewSummary) string {
	choice := "original intent"
	if s.Choice.Kind == "saved-account" {
		choice = "saved account " + s.Choice.AccountID
	} else if s.Choice.Kind == "saved-logout" {
		choice = "saved logout"
	} else if s.Choice.Kind == "saved-oauth-token" {
		choice = "saved OAuth credential"
	} else if s.Choice.Kind == "saved-credential" {
		choice = "saved credential slot " + s.Choice.CredentialID
	} else if s.OriginalOAuthToken {
		choice = "original complete OAuth credential"
	}
	var models []string
	for _, model := range s.Models {
		models = append(models, model.Kind+": "+model.Provider+" / "+model.Model)
	}
	changed := "none"
	if len(s.ChangedCategories) > 0 {
		changed = strings.Join(s.ChangedCategories, ", ")
	}
	active := s.ActiveAccountID
	if active == "" {
		active = "none"
	}
	credential := "Saved active account: " + active
	if s.Choice.Kind == "saved-credential" {
		credential = "Saved credential: exact source and literal captured; no historical connection probe asserted"
	}
	if s.SavedOAuthToken {
		credential = "Saved OAuth credential: present; no account selection"
	}
	return fmt.Sprintf("Preview %s\nProvider: %s\nExplicit choice: %s\n%s; configured: %t; disabled: %t\nModels: %s\nChanged sections: %s\nReceiver: %s, revision %d\nCtrl+Y applies only this preview. The original operation result remains unchanged.", s.PreviewID, s.Owner.ProviderID, choice, credential, s.Configured, s.Disabled, strings.Join(models, "; "), changed, s.Receiver.Principal, s.Receiver.Revision)
}

func (m *UI) handleAuthenticationReconciliation(action dialog.ActionAuthenticationReconciliation) tea.Cmd {
	var s *authenticationReconciliation
	for _, candidate := range m.authenticationReconciliations {
		if candidate.dialog == action.Dialog && candidate.operation.workspace == m.com.Workspace {
			s = candidate
			break
		}
	}
	if s == nil || s.dialog != action.Dialog || !m.authenticationReconciliationOpen(s) || !m.authenticationReconciliationCurrent(s) {
		return nil
	}
	operation := s.operation
	if action.Kind == "cancel" {
		if s.cancel != nil {
			s.cancel()
		}
		if s.preparing != nil {
			s.preparing = nil
		}
		s.message = "Cancellation requested. Retained review/apply identities remain available; cancellation does not establish rollback."
		m.showAuthenticationReconciliation(s)
		return nil
	}
	if s.preparing != nil || s.pending != "" || operation.pending || operation.preparing || operation.recoveryPreparing != nil || s.oauthLogin != nil && (s.oauthLogin.busy || s.oauthLogin.preparing) {
		return nil
	}
	if action.Kind == "choice" {
		s.choiceChanged = true
		s.message = "Choice changed. Press Enter for a fresh review; the previous review and apply identities remain retained."
		m.showAuthenticationReconciliation(s)
		return nil
	}
	if s.resolved || s.historyRetired {
		return nil
	}
	if action.Choice != s.dialog.Choice() {
		return nil
	}
	switch action.Kind {
	case "retry-review":
		if s.choiceChanged || s.request == nil || s.request.Choice != action.Choice {
			return nil
		}
		return m.dispatchAuthenticationReview(s, *s.request, true)
	case "retry-apply":
		if s.choiceChanged || s.apply == nil || s.request == nil || s.request.Choice != action.Choice {
			return nil
		}
		return m.dispatchAuthenticationReviewedApply(s, *s.apply, true)
	case "review":
		for _, other := range m.authenticationReconciliations {
			if other.operation.workspace == s.operation.workspace && other.fresh == s.fresh && other.operation.row.Target == s.operation.row.Target && (s.fresh || other.operation.id == s.operation.id) {
				s.sequence = max(s.sequence, other.sequence)
			}
		}
		if s.sequence == math.MaxUint64 {
			return util.ReportError(errors.New("authentication review sequence exhausted; the original receipt remains retained"))
		}
	case "apply":
		if s.choiceChanged || s.summary == nil || s.apply != nil || s.request == nil || s.request.Choice != action.Choice {
			return nil
		}
	default:
		return nil
	}
	p := &authenticationReconciliationPreparation{dialog: s.dialog, generation: s.dialog.Generation(), kind: action.Kind, choice: action.Choice}
	s.preparing = p
	s.message = "Preparing explicit authentication " + action.Kind + "…"
	m.showAuthenticationReconciliation(s)
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		return authenticationReconciliationPreparedMsg{s, p, hex.EncodeToString(id[:]), err}
	}
}

func (m *UI) completeAuthenticationReconciliationPreparation(msg authenticationReconciliationPreparedMsg) tea.Cmd {
	s, p := msg.state, msg.preparation
	if s == nil || p == nil || m.authenticationReconciliations[s.operation] != s || s.preparing != p {
		return nil
	}
	s.preparing = nil
	if !m.authenticationReconciliationCurrent(s) || s.dialog != p.dialog || !m.authenticationReconciliationOpen(s) || p.generation != s.dialog.Generation() {
		s.message = "Preparation cancelled; the original receipt remains available."
		m.showAuthenticationReconciliation(s)
		return nil
	}
	if msg.err != nil {
		s.message = "Could not prepare authentication action: " + msg.err.Error()
		m.showAuthenticationReconciliation(s)
		return util.ReportError(errors.New(s.message))
	}
	if p.kind == "review" {
		request := workspace.ProviderAuthenticationReviewRequest{OperationID: s.operation.id, OriginalTarget: s.operation.row.Target, ReviewID: msg.id, ReviewSequence: s.sequence + 1, Choice: p.choice}
		if s.fresh {
			request.FreshSaved = true
			request.SavedTarget = s.operation.row.Target
			request.OriginalTarget = providerauth.Target{}
			request.OperationID = ""
		}
		if err := request.Validate(); err != nil {
			s.message = err.Error()
			m.showAuthenticationReconciliation(s)
			return util.ReportError(err)
		}
		s.sequence = request.ReviewSequence
		s.request = &request
		s.summary, s.apply, s.outcome = nil, nil, nil
		s.resolved = false
		s.choiceChanged = false
		return m.dispatchAuthenticationReview(s, request, false)
	}
	if s.request == nil || s.summary == nil || s.request.Choice != p.choice {
		return nil
	}
	request := workspace.ProviderAuthenticationApplyRequest{OperationID: s.request.OperationID, OriginalTarget: s.request.OriginalTarget, FreshSaved: s.request.FreshSaved, SavedTarget: s.request.SavedTarget, ReviewID: s.request.ReviewID, PreviewID: s.summary.PreviewID, ApplyID: msg.id}
	if err := request.Validate(); err != nil {
		return util.ReportError(err)
	}
	s.apply = &request
	return m.dispatchAuthenticationReviewedApply(s, request, false)
}

func (m *UI) dispatchAuthenticationReview(s *authenticationReconciliation, request workspace.ProviderAuthenticationReviewRequest, retry bool) tea.Cmd {
	if !m.authenticationReconciliationCurrent(s) || s.historyRetired {
		return util.ReportError(providerauth.ErrStale)
	}
	s.pending, s.delivered = "review", false
	s.attempt++
	s.message = "Reviewing the exact current saved state…"
	if retry {
		s.message = "Retrieving the retained review result without another collection…"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.cancel = cancel
	m.showAuthenticationReconciliation(s)
	m.updateAuthenticationDialogs()
	capability, attempt := s.capability, s.attempt
	return func() tea.Msg {
		defer cancel()
		summary, err := capability.ReviewProviderAuthentication(ctx, request)
		return authenticationReviewCompletedMsg{s, attempt, request, summary, err}
	}
}

func (m *UI) dispatchAuthenticationReviewedApply(s *authenticationReconciliation, request workspace.ProviderAuthenticationApplyRequest, retry bool) tea.Cmd {
	if !m.authenticationReconciliationCurrent(s) || s.historyRetired {
		return util.ReportError(providerauth.ErrStale)
	}
	s.pending, s.delivered = "apply", false
	s.attempt++
	s.operation.blockNew = true
	s.message = "Applying the reviewed runtime; waiting for exact acknowledgement…"
	if retry {
		s.message = "Checking the retained apply acknowledgement without another PUT…"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	s.cancel = cancel
	m.showAuthenticationReconciliation(s)
	m.updateAuthenticationDialogs()
	capability, attempt := s.capability, s.attempt
	return func() tea.Msg {
		defer cancel()
		outcome, err := capability.ApplyProviderAuthenticationReview(ctx, request)
		return authenticationApplyCompletedMsg{s, attempt, request, outcome, err}
	}
}

func (m *UI) completeAuthenticationReview(msg authenticationReviewCompletedMsg) tea.Cmd {
	s := msg.state
	if s == nil || m.authenticationReconciliations[s.operation] != s || s.request == nil || *s.request != msg.request || s.pending != "review" || s.attempt != msg.attempt || s.delivered {
		return nil
	}
	s.pending, s.delivered, s.cancel = "", true, nil
	err := msg.err
	if err == nil {
		err = msg.summary.Validate(msg.request)
	}
	if err != nil {
		s.summary = nil
		s.message = "Review failed: " + err.Error() + ". Enter creates a new review; Ctrl+R retrieves this same result. No reload or repair was run."
	} else {
		s.summary = &msg.summary
		s.message = "Review ready. Inspect the explicit choice and changes, then Ctrl+Y applies this exact preview."
	}
	m.showAuthenticationReconciliation(s)
	m.updateAuthenticationDialogs()
	if err != nil {
		return util.ReportError(errors.New(m.authenticationReconciliationNotice(s)))
	}
	return nil
}

func (m *UI) completeAuthenticationReviewedApply(msg authenticationApplyCompletedMsg) tea.Cmd {
	s := msg.state
	if s == nil || m.authenticationReconciliations[s.operation] != s || s.apply == nil || *s.apply != msg.request || s.pending != "apply" || s.attempt != msg.attempt || s.delivered {
		return nil
	}
	s.pending, s.delivered, s.cancel = "", true, nil
	validationErr := msg.outcome.Validate(msg.request)
	err := validationErr
	if err == nil {
		s.outcome = &msg.outcome
		err = msg.err
	}
	if err == nil && !msg.outcome.Adopted {
		err = errors.New("the reviewed runtime was not adopted")
	}
	if err != nil {
		s.message = "Reviewed publication is unresolved: " + err.Error() + ". Ctrl+T checks this exact apply receipt; Enter creates a new explicit review."
		if validationErr == nil && msg.outcome.RemoteAcknowledged {
			s.message += " The receiver acknowledged the proposal; local adoption still needs confirmation."
		}
	} else {
		s.resolved = true
		s.operation.blockNew, s.operation.retry = false, false
		s.message = "Reviewed saved authentication was acknowledged and adopted. The original operation result remains unchanged."
		if s.oauthLogin != nil {
			s.oauthLogin.message = s.message
		}
	}
	m.showAuthenticationReconciliation(s)
	m.updateAuthenticationDialogs()
	if err != nil {
		return util.ReportError(errors.New(m.authenticationReconciliationNotice(s)))
	}
	if !m.authenticationReconciliationCurrent(s) {
		return util.CmdHandler(util.NewInfoMsg("Previous workspace: " + s.message))
	}
	m.invalidateBusyCaches()
	m.providerUsage = nil
	m.usageFetchGen++
	cmds := []tea.Cmd{util.CmdHandler(util.NewInfoMsg(s.message)), m.dispatchBusyRefresh(), authenticationUsageCommand(s.operation.workspace, m.usageFetchGen)}
	if m.authenticationDialogOpen(s.operation.dialog) {
		cmds = append(cmds, m.loadAuthenticationAccounts(s.operation.dialog))
	}
	return tea.Batch(cmds...)
}

func (m *UI) authenticationReconciliationNotice(s *authenticationReconciliation) string {
	if !m.authenticationReconciliationCurrent(s) {
		return "Previous workspace: " + s.message
	}
	return s.message
}
