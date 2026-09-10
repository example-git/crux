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
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type savedAuthenticationUI struct {
	workspace  workspace.Workspace
	capability workspace.ProviderAuthenticationSavedState
	dialog     *dialog.SavedAuthentication
	snapshot   providerauth.Snapshot
	pending    bool
	attempt    uint64
	cancel     context.CancelFunc
	reload     *providerauth.ReloadRequest
	outcome    *providerauth.ReloadOutcome
	message    string
}
type savedAuthenticationReadMsg struct {
	state    *savedAuthenticationUI
	attempt  uint64
	snapshot providerauth.Snapshot
	err      error
}
type savedAuthenticationReloadMsg struct {
	state   *savedAuthenticationUI
	attempt uint64
	request providerauth.ReloadRequest
	outcome providerauth.ReloadOutcome
	err     error
}

func (m *UI) savedAuthenticationOpen(s *savedAuthenticationUI) bool {
	return s != nil && s.workspace == m.com.Workspace && m.dialog.Dialog(dialog.SavedAuthenticationID) == s.dialog
}

func (m *UI) openSavedAuthentication() tea.Cmd {
	capability, ok := m.com.Workspace.(workspace.ProviderAuthenticationSavedState)
	if !ok || !capability.CanReconcileProviderAuthentication() {
		return util.ReportError(errors.New("saved authentication reload and review requires the owning client workspace"))
	}
	if m.savedAuthentications == nil {
		m.savedAuthentications = map[workspace.Workspace]*savedAuthenticationUI{}
	}
	s := m.savedAuthentications[m.com.Workspace]
	if s != nil {
		m.savedAuthentication = s
		m.dialog.CloseDialog(dialog.SavedAuthenticationID)
		m.dialog.OpenDialog(s.dialog)
		m.showSavedAuthentication(s)
		return nil
	}
	s = &savedAuthenticationUI{workspace: m.com.Workspace, capability: capability, dialog: dialog.NewSavedAuthentication(m.com)}
	m.savedAuthentication = s
	m.savedAuthentications[m.com.Workspace] = s
	m.dialog.OpenDialog(s.dialog)
	return m.readSavedAuthentication(s)
}

func (m *UI) showSavedAuthentication(s *savedAuthenticationUI) {
	if s != nil && s.dialog != nil {
		s.dialog.SetState(s.message, s.pending, s.reload != nil)
	}
}

func savedAuthenticationRows(snapshot providerauth.Snapshot) []dialog.SavedAuthenticationChoice {
	var rows []dialog.SavedAuthenticationChoice
	for _, provider := range snapshot.Providers {
		hasOAuth := false
		for _, credential := range provider.Credentials {
			if credential.Kind == "oauth" && credential.State != "absent" {
				hasOAuth = true
			}
		}
		var slots []providerauth.CredentialSlot
		for _, slot := range provider.CredentialSlots {
			if slot.Configured && (slot.Property != "" || !hasOAuth) {
				slots = append(slots, slot)
			}
		}
		target := providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Generation: snapshot.Generation, Owner: provider.Owner}
		add := func(choice workspace.ProviderAuthenticationReviewChoice, label string) {
			rows = append(rows, dialog.SavedAuthenticationChoice{Target: target, Choice: choice, Label: provider.Owner.ProviderID + " · " + label, Slots: slots})
		}
		for _, slot := range slots {
			if slot.Configured {
				add(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-credential", CredentialID: slot.ID}, "saved "+slot.ID)
			}
		}
		if provider.ActiveAccountID != "" {
			add(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-account", AccountID: provider.ActiveAccountID}, "saved active account "+provider.ActiveAccountID)
		}
		if provider.Owner.HasOAuth && provider.ActiveAccountID == "" {
			for _, credential := range provider.Credentials {
				if credential.Kind == "oauth" && credential.State != "absent" {
					add(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-oauth-token"}, "saved OAuth token")
					break
				}
			}
		}
		add(workspace.ProviderAuthenticationReviewChoice{Kind: "saved-logout"}, "saved logout (must be empty)")
	}
	return rows
}

func (m *UI) readSavedAuthentication(s *savedAuthenticationUI) tea.Cmd {
	if s.pending {
		return nil
	}
	s.pending = true
	s.attempt++
	attempt := s.attempt
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	s.cancel = cancel
	s.message = "Reading current local saved status; no receiver publication or credential operation is performed."
	m.showSavedAuthentication(s)
	return func() tea.Msg {
		defer cancel()
		snapshot, err := s.capability.SavedProviderAuthentication(ctx)
		return savedAuthenticationReadMsg{s, attempt, snapshot, err}
	}
}

func (m *UI) completeSavedAuthenticationRead(msg savedAuthenticationReadMsg) tea.Cmd {
	s := msg.state
	if s == nil || m.savedAuthentications[s.workspace] != s || !s.pending || s.attempt != msg.attempt {
		return nil
	}
	s.pending = false
	s.cancel = nil
	err := msg.err
	if err == nil {
		err = msg.snapshot.Validate()
	}
	if err != nil {
		s.message = "Saved status unavailable: " + err.Error()
		m.showSavedAuthentication(s)
		return util.ReportError(errors.New(s.message))
	}
	s.snapshot = msg.snapshot
	s.dialog.SetRows(savedAuthenticationRows(msg.snapshot))
	s.message = "Choose exact saved intent. Ctrl+L runs the normal owner-local file load, evaluating sources and configured model discovery; Enter reviews. Apply separately publishes the reviewed runtime."
	m.showSavedAuthentication(s)
	return nil
}

func (m *UI) handleSavedAuthentication(action dialog.ActionSavedAuthentication) tea.Cmd {
	s := m.savedAuthentication
	if !m.savedAuthenticationOpen(s) || action.Dialog != s.dialog {
		return nil
	}
	if action.Kind == "cancel" {
		if s.cancel != nil {
			s.cancel()
		}
		return nil
	}
	if s.pending {
		return nil
	}
	switch action.Kind {
	case "status":
		return m.readSavedAuthentication(s)
	case "reload":
		valid := false
		for _, row := range savedAuthenticationRows(s.snapshot) {
			if row.Target == action.Selection.Target && row.Choice == action.Selection.Choice {
				valid = true
				break
			}
		}
		if !valid {
			return nil
		}
		// ID preparation and the actual reload both occur inside the command. The
		// returned request is retained before any retry can be dispatched.
		return m.dispatchSavedAuthenticationReload(s, nil, action.Selection.Target)
	case "retry-reload":
		if s.reload != nil {
			return m.dispatchSavedAuthenticationReload(s, s.reload, s.reload.Target)
		}
	case "review":
		var selected *dialog.SavedAuthenticationChoice
		for _, row := range savedAuthenticationRows(s.snapshot) {
			if row.Target == action.Selection.Target && row.Choice == action.Selection.Choice {
				copy := row
				selected = &copy
				break
			}
		}
		if selected == nil {
			return nil
		}
		operation := &authenticationOperation{workspace: s.workspace, row: dialog.AuthenticationRow{Target: selected.Target}, message: "New explicit saved-state intent; no original operation is asserted."}
		command := m.openAuthenticationReconciliationOperation(operation)
		if state := m.authenticationReconciliations[operation]; state != nil {
			state.fresh = true
			for _, previous := range m.authenticationReconciliations {
				if previous != state && previous.fresh && previous.operation.workspace == s.workspace && previous.operation.row.Target == selected.Target && previous.sequence > state.sequence {
					state.sequence = previous.sequence
				}
			}
			state.dialog.SetFreshSaved(selected.Slots)
			state.dialog.SetChoice(selected.Choice)
			state.message = "Review this exact saved choice. No historical operation result is changed. Return to Saved Authentication for an explicit disk reload."
			m.showAuthenticationReconciliation(state)
		}
		return command
	}
	return nil
}

func (m *UI) dispatchSavedAuthenticationReload(s *savedAuthenticationUI, retained *providerauth.ReloadRequest, target providerauth.Target) tea.Cmd {
	s.pending = true
	s.attempt++
	attempt := s.attempt
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	s.cancel = cancel
	s.message = "Reloading owner-local saved files. Earlier operation receipts remain historical; this does not publish to the receiver."
	m.showSavedAuthentication(s)
	var request providerauth.ReloadRequest
	if retained != nil {
		request = *retained
	}
	return func() tea.Msg {
		defer cancel()
		if retained == nil {
			var id [16]byte
			if _, err := rand.Read(id[:]); err != nil {
				return savedAuthenticationReloadMsg{state: s, attempt: attempt, err: err}
			}
			request = providerauth.ReloadRequest{ReloadID: hex.EncodeToString(id[:]), Target: target}
		}
		outcome, err := s.capability.ReloadProviderAuthentication(ctx, request)
		return savedAuthenticationReloadMsg{s, attempt, request, outcome, err}
	}
}

func (m *UI) completeSavedAuthenticationReload(msg savedAuthenticationReloadMsg) tea.Cmd {
	s := msg.state
	if s == nil || m.savedAuthentications[s.workspace] != s || !s.pending || s.attempt != msg.attempt {
		return nil
	}
	s.pending = false
	s.cancel = nil
	err := msg.err
	if msg.request.ReloadID != "" {
		request := msg.request
		s.reload = &request
		s.outcome = nil
		validation := msg.outcome.Validate(request)
		if validation != nil {
			err = validation
		} else {
			outcome := msg.outcome
			s.outcome = &outcome
		}
	}
	if err != nil {
		s.message = "Reload result unresolved: " + err.Error() + ". Ctrl+T reads this exact receipt; Ctrl+R captures current status before a new explicit action."
		if s.outcome != nil && s.outcome.Reloaded {
			s.message += " Local reload publication was recorded."
		}
		m.showSavedAuthentication(s)
		return util.ReportError(errors.New(s.message))
	}
	if msg.outcome.Snapshot == nil {
		s.message = "Reload returned no verified saved-state snapshot. Ctrl+R reads current status."
		m.showSavedAuthentication(s)
		return util.ReportError(providerauth.ErrReceiptUnverified)
	}
	if s.snapshot.Generation.Epoch == msg.outcome.Snapshot.Generation.Epoch && s.snapshot.Generation.Sequence > msg.outcome.Snapshot.Generation.Sequence {
		s.message = "Historical reload receipt retained. A newer saved status is already shown; Ctrl+R reads current status."
		m.showSavedAuthentication(s)
		return nil
	}
	s.snapshot = *msg.outcome.Snapshot
	s.dialog.SetRows(savedAuthenticationRows(s.snapshot))
	s.message = fmt.Sprintf("Local saved files reloaded (generation %d). Choose a saved effect for a new review and separate Apply; old results remain unchanged.", s.snapshot.Generation.Sequence)
	m.showSavedAuthentication(s)
	return nil
}
