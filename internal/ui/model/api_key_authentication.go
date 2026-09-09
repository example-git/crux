package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type apiKeySession struct {
	workspace                     workspace.Workspace
	dialog                        *dialog.APIKeyInput
	selection                     dialog.ActionSelectModel
	generation                    uint64
	target                        providerauth.Target
	slots                         []providerauth.CredentialSlot
	credentialID, credentialLabel string
	ready                         bool
	cancel                        context.CancelFunc
}
type apiKeyOperation struct {
	initialCheck                                        bool
	progress                                            providerauth.MutationProgress
	workspace                                           workspace.Workspace
	dialog                                              *dialog.APIKeyInput
	selection                                           dialog.ActionSelectModel
	generation                                          uint64
	check                                               providerauth.APIKeyCheckRequest
	save                                                providerauth.APIKeySaveRequest
	checked                                             providerauth.APIKeyCheckOutcome
	outcome                                             providerauth.MutationOutcome
	recoverer                                           workspace.ProviderAuthenticationRecoverer
	recovery                                            *workspace.ProviderAuthenticationRecoveryRequest
	recoverySequence                                    uint64
	attempt                                             uint64
	busy, preparing, attemptedSave, resolved, continued bool
	kind, message                                       string
	credentialLabel                                     string
	cancel                                              context.CancelFunc
}

// Diagnostic formatting must never traverse the privately retained source.
func (*apiKeyOperation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private checked key operation]"))
}

type apiKeyStatusMsg struct {
	session  *apiKeySession
	snapshot providerauth.Snapshot
	err      error
}
type apiKeyIDsMsg struct {
	operation       *apiKeyOperation
	attempt         uint64
	checkID, saveID string
	recovery        bool
	err             error
}
type apiKeyCheckMsg struct {
	operation *apiKeyOperation
	attempt   uint64
	outcome   providerauth.APIKeyCheckOutcome
	err       error
}
type apiKeySaveMsg struct {
	operation *apiKeyOperation
	attempt   uint64
	outcome   providerauth.MutationOutcome
	recovery  *workspace.ProviderAuthenticationRecoveryRequest
	err       error
}

func (m *UI) apiKeyDialogOpen(d *dialog.APIKeyInput) bool {
	return d != nil && m.dialog != nil && m.dialog.Dialog(dialog.APIKeyInputID) == d
}
func (m *UI) pruneAPIKeySessions() {
	for d, s := range m.apiKeySessions {
		if s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(d) {
			s.cancel()
			delete(m.apiKeySessions, d)
			if m.apiKeyDialogOpen(d) {
				d.SetPresentation(dialog.APIKeyPresentation{Message: "Workspace changed; reopen authentication for the selected workspace."})
			}
		}
	}
	for _, op := range m.apiKeyOperations {
		if (op.workspace != m.com.Workspace || !m.apiKeyDialogOpen(op.dialog)) && op.kind == "check" && op.cancel != nil {
			op.cancel()
		}
	}
}
func (m *UI) openAPIKeyAuthentication(selection dialog.ActionSelectModel) tea.Cmd {
	selection.Model = selection.Model.Clone()
	if previous := m.apiKeyOperations[m.com.Workspace]; previous != nil && previous.resolved {
		delete(m.apiKeyOperations, m.com.Workspace)
	}
	d, _ := dialog.NewAPIKeyInput(m.com, m.state == uiOnboarding, selection)
	m.dialog.CloseDialog(dialog.APIKeyInputID)
	m.dialog.OpenDialogWithGrace(d)
	m.pruneAPIKeySessions()
	if m.apiKeySessions == nil {
		m.apiKeySessions = make(map[*dialog.APIKeyInput]*apiKeySession)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	s := &apiKeySession{workspace: m.com.Workspace, dialog: d, selection: selection, generation: m.modelSelectionGen, cancel: cancel}
	m.apiKeySessions[d] = s
	m.showAPIKeyOperation(d)
	ws := s.workspace
	return func() tea.Msg {
		defer cancel()
		snapshot, err := ws.ProviderAuthentication(ctx)
		return apiKeyStatusMsg{s, snapshot, err}
	}
}
func (m *UI) completeAPIKeyStatus(msg apiKeyStatusMsg) tea.Cmd {
	s := msg.session
	if s == nil || m.apiKeySessions[s.dialog] != s || s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(s.dialog) {
		return nil
	}
	s.cancel()
	err := msg.err
	if err == nil {
		err = msg.snapshot.Validate()
	}
	if err == nil {
		err = s.selection.ValidateProviderOwner(s.workspace.Config())
	}
	if err == nil && s.selection.Model.Model != "" && s.generation != m.modelSelectionGen {
		err = errors.New("model selection changed; reopen authentication")
	}
	var found *providerauth.Status
	if err == nil {
		for _, status := range msg.snapshot.Providers {
			if status.Owner == providerauth.PublicOwner(s.selection.ProviderOwner) {
				copy := status
				found = &copy
				break
			}
		}
		if found == nil {
			err = providerauth.ErrOwner
		}
	}
	if err != nil {
		s.dialog.SetPresentation(dialog.APIKeyPresentation{Message: "Could not load authentication: " + safeAPIKeyError(err), Reload: true})
		m.showAPIKeyOperation(s.dialog)
		return util.ReportError(errors.New("Could not load selected workspace authentication: " + safeAPIKeyError(err)))
	}
	s.target = providerauth.Target{WorkspaceID: msg.snapshot.WorkspaceID, Owner: found.Owner, Generation: msg.snapshot.Generation}
	s.slots = slices.Clone(found.CredentialSlots)
	s.ready = true
	if m.apiKeyOperations[s.workspace] != nil {
		m.showAPIKeyOperation(s.dialog)
		return nil
	}
	// Only the owner's declared choices are offered. OAuth capability does not
	// hide declared configuration credentials or imply an API-key fallback.
	var choices []dialog.APIKeyCredentialChoice
	for _, slot := range s.slots {
		choices = append(choices, dialog.APIKeyCredentialChoice{ID: slot.ID, Label: apiKeyCredentialLabel(slot), Configured: slot.Configured})
	}
	if found.Owner.HasOAuth {
		choices = append(choices, dialog.APIKeyCredentialChoice{OAuth: true, Label: "OAuth sign-in"})
	}
	if len(choices) == 1 && (choices[0].ID == "provider.api_key" || choices[0].OAuth) {
		return m.selectAPIKeyCredential(dialog.ActionAPIKeySelectCredential{Dialog: s.dialog, CredentialID: choices[0].ID, OAuth: choices[0].OAuth})
	}
	if len(choices) == 0 {
		s.ready = false
		s.dialog.SetPresentation(dialog.APIKeyPresentation{Message: "This owner reports no editable credential slots or OAuth sign-in. Reload status after changing the provider definition.", Reload: true})
		return nil
	}
	s.dialog.SetChoices(choices)
	s.dialog.SetPresentation(dialog.APIKeyPresentation{Message: "Choose the credential to configure. Each check and save keeps this exact choice. OAuth sign-in is a separate action.", Reload: true})
	return nil
}

func apiKeyCredentialLabel(slot providerauth.CredentialSlot) string {
	if slot.ID == "provider.api_key" {
		return "Provider API key (provider.api_key)"
	}
	return slot.ID + " · " + slot.Kind + " · configuration property " + strconv.Quote(slot.Property)
}

func (m *UI) selectAPIKeyCredential(action dialog.ActionAPIKeySelectCredential) tea.Cmd {
	s := m.apiKeySessions[action.Dialog]
	if s == nil || !s.ready || s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(action.Dialog) || s.selection.Model.Model != "" && s.generation != m.modelSelectionGen {
		return util.ReportError(errors.New("Reload authentication for the current provider before choosing a credential"))
	}
	if err := s.selection.ValidateProviderOwner(s.workspace.Config()); err != nil {
		return util.ReportError(err)
	}
	if m.apiKeyOperations[s.workspace] != nil {
		return util.ReportError(errors.New("The original credential request is retained; use its retry or explicitly reload before choosing another credential"))
	}
	if s.credentialID != "" {
		return util.ReportError(errors.New("Credential input is already selected; explicitly reload before choosing another credential"))
	}
	if action.OAuth {
		if action.CredentialID != "" || !s.target.Owner.HasOAuth {
			return util.ReportError(providerauth.ErrOwner)
		}
		m.dialog.CloseDialog(dialog.APIKeyInputID)
		m.pruneAPIKeySessions()
		var selection *dialog.ActionSelectModel
		if s.selection.Model.Model != "" {
			selection = &s.selection
		}
		return m.openOAuthAuthentication(selection, &s.target.Owner)
	}
	for _, slot := range s.slots {
		if slot.ID != action.CredentialID {
			continue
		}
		s.credentialID, s.credentialLabel = slot.ID, apiKeyCredentialLabel(slot)
		s.dialog.SetChoices(nil)
		s.dialog.SetPresentation(dialog.APIKeyPresentation{Credential: s.credentialLabel, Message: "Enter a credential or expression to check on the selected workspace. Enter checks; saving is a separate action. Ctrl+N reloads credential choices.", Editable: true, Reload: true})
		return nil
	}
	return util.ReportError(errors.New("The selected credential is not declared by this owner; reload status"))
}
func (m *UI) showAPIKeyOperation(d *dialog.APIKeyInput) {
	s := m.apiKeySessions[d]
	if s == nil || !m.apiKeyDialogOpen(d) {
		return
	}
	op := m.apiKeyOperations[s.workspace]
	if op == nil {
		return
	}
	d.SetChoices(nil)
	p := dialog.APIKeyPresentation{SaveDispatched: op.attemptedSave, Credential: op.credentialLabel, Message: "Original credential request for " + op.selection.Model.Provider + ". " + op.message, Evidence: dialog.APIKeyProbeDescription(op.checked.Probe)}
	if !op.busy && !op.preparing {
		p.Retry = op.kind != "" && !op.resolved
		p.Save = op.checked.CheckedTarget != nil && !op.attemptedSave
		p.Reload = !op.attemptedSave || op.resolved
		p.Recover = op.attemptedSave && !op.resolved && op.recoverer != nil
		p.RetryRecovery = p.Recover && op.recovery != nil
	}
	d.SetPresentation(p)
}
func (m *UI) updateAPIKeyDialogs() {
	for d := range m.apiKeySessions {
		m.showAPIKeyOperation(d)
	}
}
func (m *UI) beginAPIKeyCheck(d *dialog.APIKeyInput) tea.Cmd {
	s := m.apiKeySessions[d]
	if s == nil || !s.ready || !m.apiKeyDialogOpen(d) || s.workspace != m.com.Workspace || s.selection.Model.Model != "" && s.generation != m.modelSelectionGen {
		return util.ReportError(errors.New("Reload authentication for the current model selection before checking input"))
	}
	if err := s.selection.ValidateProviderOwner(s.workspace.Config()); err != nil {
		return util.ReportError(err)
	}
	if previous := m.apiKeyOperations[s.workspace]; previous != nil {
		return util.ReportError(errors.New("Retry the retained request or explicitly start new input before checking another credential"))
	}
	if s.credentialID == "" || !slices.ContainsFunc(s.slots, func(slot providerauth.CredentialSlot) bool { return slot.ID == s.credentialID }) {
		return util.ReportError(errors.New("Choose a credential reported by the selected owner before checking input"))
	}
	source := d.TakeSource()
	if strings.TrimSpace(source) == "" {
		d.SetPresentation(dialog.APIKeyPresentation{Credential: s.credentialLabel, Message: "Enter a credential or expression.", Editable: true, Reload: true})
		return nil
	}
	if m.apiKeyOperations == nil {
		m.apiKeyOperations = make(map[workspace.Workspace]*apiKeyOperation)
	}
	op := &apiKeyOperation{initialCheck: true, workspace: s.workspace, dialog: d, selection: s.selection, generation: s.generation, credentialLabel: s.credentialLabel, check: providerauth.APIKeyCheckRequest{Target: s.target, CredentialID: s.credentialID, Source: source}, preparing: true, kind: "check", attempt: 1, message: "Preparing the original credential check…"}
	if r, ok := op.workspace.(workspace.ProviderAuthenticationRecoverer); ok && r.CanRecoverProviderAuthentication() {
		op.recoverer = r
	}
	m.apiKeyOperations[s.workspace] = op
	m.updateAPIKeyDialogs()
	return prepareAPIKeyIDs(op, op.attempt, false)
}
func prepareAPIKeyIDs(op *apiKeyOperation, attempt uint64, recovery bool) tea.Cmd {
	return func() tea.Msg {
		var first, second [16]byte
		_, err := rand.Read(first[:])
		if err == nil && !recovery {
			_, err = rand.Read(second[:])
		}
		return apiKeyIDsMsg{operation: op, attempt: attempt, checkID: hex.EncodeToString(first[:]), saveID: hex.EncodeToString(second[:]), recovery: recovery, err: err}
	}
}
func (m *UI) completeAPIKeyIDs(msg apiKeyIDsMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.apiKeyOperations[op.workspace] != op || !op.preparing || op.attempt != msg.attempt {
		return nil
	}
	op.preparing = false
	if msg.err != nil {
		op.message = "Could not prepare request identifiers; no request was dispatched."
		m.updateAPIKeyDialogs()
		return util.ReportError(errors.New(op.message))
	}
	if msg.recovery {
		op.recoverySequence++
		op.recovery = &workspace.ProviderAuthenticationRecoveryRequest{RecoveryID: msg.checkID, OperationID: op.save.OperationID, Target: op.save.Target, RecoverySequence: op.recoverySequence}
	} else {
		op.check.CheckID = msg.checkID
		op.save.OperationID = msg.saveID
		op.save.CheckID = msg.checkID
	}
	initialCheck := op.initialCheck
	op.initialCheck = false
	if op.workspace != m.com.Workspace || !m.apiKeyDialogOpen(op.dialog) || initialCheck && op.selection.Model.Model != "" && op.generation != m.modelSelectionGen {
		op.message = "Preparation cancelled before dispatch. The original request remains available for explicit retry."
		m.updateAPIKeyDialogs()
		return nil
	}
	if msg.recovery {
		return m.dispatchAPIKeySave(op, true)
	}
	return m.dispatchAPIKeyCheck(op)
}
func (m *UI) dispatchAPIKeyCheck(op *apiKeyOperation) tea.Cmd {
	op.busy = true
	op.kind = "check"
	op.attempt++
	op.message = "Checking the retained input for " + op.selection.Model.Provider + " on its workspace owner…"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	op.cancel = cancel
	ws, request, attempt := op.workspace, op.check, op.attempt
	m.updateAPIKeyDialogs()
	return func() tea.Msg {
		defer cancel()
		outcome, err := ws.CheckProviderAPIKey(ctx, request)
		return apiKeyCheckMsg{op, attempt, outcome, err}
	}
}
func (m *UI) completeAPIKeyCheck(msg apiKeyCheckMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.apiKeyOperations[op.workspace] != op || !op.busy || op.kind != "check" || op.attempt != msg.attempt {
		return nil
	}
	op.busy = false
	op.cancel = nil
	err := msg.err
	if validation := msg.outcome.Validate(); validation != nil || msg.outcome.CheckID != op.check.CheckID || msg.outcome.Previous != op.check.Target || msg.outcome.CredentialID != op.check.CredentialID {
		err = providerauth.ErrReceiptUnverified
	} else {
		op.checked = msg.outcome
	}
	if err == nil && msg.outcome.CheckedTarget == nil {
		err = providerauth.ErrReceiptUnverified
	}
	if err != nil {
		op.checked.CheckedTarget = nil
		op.message = "Credential check did not complete: " + safeAPIKeyError(err) + " Ctrl+T retries the original check; Ctrl+N starts new input."
		m.updateAPIKeyDialogs()
		return util.ReportError(errors.New(op.message))
	}
	op.save.Target = *op.checked.CheckedTarget
	op.message = "Original input for " + op.selection.Model.Provider + " is retained. Enter saves that retained input and continues the originally selected model only if that selection is still current. Escape cancels without saving."
	if op.selection.Model.Model == "" {
		op.message = "Original input for " + op.selection.Model.Provider + " is retained. Enter saves that retained credential. Escape cancels without saving."
	}
	if op.checked.PendingConfiguration {
		op.message = "The entered credential is retained. Other declared credential fields are still required, so no connection probe was performed. Enter saves this field for continued setup. Escape cancels without saving."
	} else if op.checked.SchemaOnly {
		op.message = "The entered credential passed source and configuration-schema validation only. No connection probe applies to this declared field, and provider access has not been verified. Enter saves the retained field. Escape cancels without saving."
	}
	m.updateAPIKeyDialogs()
	return nil
}
func (m *UI) beginAPIKeySave(d *dialog.APIKeyInput) tea.Cmd {
	s := m.apiKeySessions[d]
	if s == nil || !m.apiKeyDialogOpen(d) || s.workspace != m.com.Workspace {
		return nil
	}
	op := m.apiKeyOperations[s.workspace]
	if op == nil || op.busy || op.preparing || op.attemptedSave || op.checked.CheckedTarget == nil {
		return nil
	}
	if err := op.selection.ValidateProviderOwner(op.workspace.Config()); err != nil {
		return util.ReportError(err)
	}
	op.dialog = d
	return m.dispatchAPIKeySave(op, false)
}
func (m *UI) retryAPIKeyOperation(d *dialog.APIKeyInput) tea.Cmd {
	s := m.apiKeySessions[d]
	if s == nil || s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(d) {
		return nil
	}
	op := m.apiKeyOperations[s.workspace]
	if op == nil || op.busy || op.preparing || op.resolved {
		return nil
	}
	op.dialog = d
	if op.attemptedSave {
		return m.dispatchAPIKeySave(op, false)
	}
	op.initialCheck = false
	if op.check.CheckID == "" {
		op.preparing = true
		op.attempt++
		m.updateAPIKeyDialogs()
		return prepareAPIKeyIDs(op, op.attempt, false)
	}
	return m.dispatchAPIKeyCheck(op)
}
func (m *UI) recoverAPIKeyOperation(action dialog.ActionAPIKeyRecover) tea.Cmd {
	s := m.apiKeySessions[action.Dialog]
	if s == nil || s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(action.Dialog) {
		return nil
	}
	op := m.apiKeyOperations[s.workspace]
	if op == nil || op.busy || op.preparing || op.resolved || !op.attemptedSave || op.recoverer == nil {
		return nil
	}
	op.dialog = action.Dialog
	if action.Retry {
		if op.recovery == nil {
			return nil
		}
		return m.dispatchAPIKeySave(op, true)
	}
	op.preparing = true
	op.attempt++
	op.message = "Preparing an explicit recovery attempt for the original saved credential…"
	m.updateAPIKeyDialogs()
	return prepareAPIKeyIDs(op, op.attempt, true)
}
func (m *UI) dispatchAPIKeySave(op *apiKeyOperation, recovery bool) tea.Cmd {
	op.busy = true
	op.attemptedSave = true
	op.attempt++
	op.kind = "save"
	op.message = "Saving the original checked input; waiting for workspace acknowledgement…"
	var recoveryRequest *workspace.ProviderAuthenticationRecoveryRequest
	if recovery {
		value := *op.recovery
		recoveryRequest = &value
		op.kind = "recovery"
		op.message = "Attempting explicit publication recovery of the original saved credential…"
	}
	ws, request, attempt, recoverer := op.workspace, op.save, op.attempt, op.recoverer
	m.updateAPIKeyDialogs()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var outcome providerauth.MutationOutcome
		var err error
		if recoveryRequest != nil {
			outcome, err = recoverer.RecoverProviderAuthentication(ctx, *recoveryRequest)
		} else {
			outcome, err = ws.SaveCheckedProviderAPIKey(ctx, request)
		}
		return apiKeySaveMsg{op, attempt, outcome, recoveryRequest, err}
	}
}
func (m *UI) completeAPIKeySave(msg apiKeySaveMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.apiKeyOperations[op.workspace] != op || !op.busy || op.attempt != msg.attempt || op.kind == "check" {
		return nil
	}
	if (msg.recovery == nil) != (op.kind != "recovery") || msg.recovery != nil && (op.recovery == nil || *msg.recovery != *op.recovery) {
		return nil
	}
	op.busy = false
	err := msg.err
	if validation := msg.outcome.ValidateAPIKeySave(op.save); validation != nil ||
		msg.outcome.CredentialID != "" && msg.outcome.CredentialID != op.check.CredentialID ||
		msg.outcome.Change != nil && msg.outcome.CredentialID != op.check.CredentialID {
		err = providerauth.ErrReceiptUnverified
	} else {
		op.outcome = msg.outcome
		op.progress.AccountRefreshed = op.progress.AccountRefreshed || msg.outcome.Progress.AccountRefreshed
		op.progress.AccountsSaved = op.progress.AccountsSaved || msg.outcome.Progress.AccountsSaved
		op.progress.ConfigSaved = op.progress.ConfigSaved || msg.outcome.Progress.ConfigSaved
		op.progress.RuntimePublished = op.progress.RuntimePublished || msg.outcome.Progress.RuntimePublished
	}
	if err == nil && !msg.outcome.Superseded && msg.outcome.Change == nil {
		err = providerauth.ErrReceiptUnverified
	}
	if err != nil {
		op.message = "Credential save has no current acknowledgement: " + safeAPIKeyError(err) + " " + apiKeyProgress(op.progress) + " Ctrl+T retries the original save receipt."
		if op.recoverer != nil {
			op.message += " Alt+R attempts publication recovery; checked-credential review is not supported here."
		}
		m.updateAPIKeyDialogs()
		return util.ReportError(errors.New(op.message))
	}
	op.resolved = true
	if msg.outcome.Superseded {
		op.message = "The original credential save is historical; it does not authorize a model change. Reload status for new input."
		m.updateAPIKeyDialogs()
		return util.ReportWarn(op.message)
	}
	var missing []string
	for _, slot := range msg.outcome.Change.Current.Status.CredentialSlots {
		if slot.Property != "" && !slot.Configured {
			missing = append(missing, apiKeyCredentialLabel(slot))
		}
	}
	if len(missing) != 0 {
		op.message = "Credential field saved and acknowledged. Setup is still pending; configure " + strings.Join(missing, "; ") + ". Ctrl+N reloads the credential choices for the next field."
		m.updateAPIKeyDialogs()
		return util.ReportInfo(op.message)
	}
	op.message = "Checked credential saved and acknowledged by the selected workspace."
	m.updateAPIKeyDialogs()
	if op.selection.Model.Model == "" {
		return util.ReportInfo(op.message)
	}
	if op.continued || op.workspace != m.com.Workspace || op.generation != m.modelSelectionGen {
		return util.ReportInfo(op.message + " The newer model selection was preserved.")
	}
	if err := op.selection.ValidateProviderOwner(op.workspace.Config()); err != nil {
		return util.ReportError(errors.New(op.message + " The provider owner changed; model selection was not continued."))
	}
	op.continued = true
	selection := op.selection
	selection.ReAuthenticate = false
	return m.handleSelectModelAfterImport(selection, false)
}
func (m *UI) reloadAPIKeyInput(d *dialog.APIKeyInput) tea.Cmd {
	s := m.apiKeySessions[d]
	if s == nil || s.workspace != m.com.Workspace || !m.apiKeyDialogOpen(d) {
		return nil
	}
	if op := m.apiKeyOperations[s.workspace]; op != nil {
		if op.busy || op.preparing || op.attemptedSave && !op.resolved {
			return util.ReportError(errors.New("The original save receipt remains unresolved; retry it or attempt explicit recovery before entering another credential"))
		}
		delete(m.apiKeyOperations, s.workspace)
	}
	return m.openAPIKeyAuthentication(s.selection)
}
func apiKeyProgress(p providerauth.MutationProgress) string {
	var parts []string
	if p.AccountRefreshed {
		parts = append(parts, "Account refresh reported.")
	}
	if p.AccountsSaved {
		parts = append(parts, "Account persistence reported.")
	}
	if p.ConfigSaved {
		parts = append(parts, "The owning configuration was saved.")
	}
	if p.RuntimePublished {
		parts = append(parts, "The owning runtime publication was reported; receiver acknowledgement remains separate.")
	}
	return strings.Join(parts, " ")
}
func safeAPIKeyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "The request was cancelled."
	case errors.Is(err, context.DeadlineExceeded):
		return "The request timed out; its original receipt may still exist."
	case errors.Is(err, providerauth.ErrStale):
		return "Authentication state changed; reload status for a new check."
	case errors.Is(err, providerauth.ErrOwner):
		return "The selected provider owner is unavailable or changed."
	case errors.Is(err, providerauth.ErrAPIKeyCheckUnavailable):
		return "The checked receipt is unavailable; a new explicit check is required."
	case errors.Is(err, providerauth.ErrAPIKeyCheck):
		return "The workspace could not complete the provider's check policy."
	case errors.Is(err, providerauth.ErrReceiptUnverified):
		return "The response did not verify the original request."
	case errors.Is(err, config.ErrClientRuntimeManaged):
		return "This receiver does not own the provider credentials."
	default:
		return "The workspace could not acknowledge this request."
	}
}

func (m *UI) cancelAPIKeyReads() {
	for _, s := range m.apiKeySessions {
		s.cancel()
	}
	for _, op := range m.apiKeyOperations {
		if op.kind == "check" && op.cancel != nil {
			op.cancel()
		}
	}
}
