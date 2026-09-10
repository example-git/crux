package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/config"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type authenticationRead struct {
	workspace  workspace.Workspace
	dialog     *dialog.AccountAuthentication
	generation uint64
	cancel     context.CancelFunc
	completed  bool
	rows       []dialog.AuthenticationRow
}
type authenticationLoadedMsg struct {
	read *authenticationRead
	rows []dialog.AuthenticationRow
	err  error
}
type authenticationOperation struct {
	workspace                            workspace.Workspace
	dialog                               *dialog.AccountAuthentication
	generation                           uint64
	row                                  dialog.AuthenticationRow
	logout                               bool
	id                                   string
	attempt                              uint64
	preparing, pending, delivered, retry bool
	message                              string
	recoverer                            workspace.ProviderAuthenticationRecoverer
	blockNew                             bool
	recovery                             *workspace.ProviderAuthenticationRecoveryRequest
	recoveryPreparing                    *authenticationRecoveryPreparation
	accountChoices                       []dialog.AuthenticationRow
}
type authenticationPreparedMsg struct {
	operation *authenticationOperation
	id        string
	err       error
}
type authenticationCompletedMsg struct {
	operation *authenticationOperation
	attempt   uint64
	outcome   providerauth.MutationOutcome
	err       error
	recovery  *workspace.ProviderAuthenticationRecoveryRequest
}
type authenticationUsageMsg struct {
	workspace  workspace.Workspace
	generation uint64
	usage      *oauthusage.Usage
}

func (m *UI) authenticationDialogOpen(d *dialog.AccountAuthentication) bool {
	if d == nil || m.dialog == nil {
		return false
	}
	current, ok := m.dialog.Dialog(d.ID()).(interface {
		AuthenticationState() *dialog.AccountAuthentication
	})
	return ok && current.AuthenticationState() == d
}

func (m *UI) pruneAuthenticationReads() {
	m.pruneAuthenticationHistory()
	m.pruneAuthenticationReconciliations()
	for d, read := range m.authenticationReads {
		if read.workspace != m.com.Workspace || !m.authenticationDialogOpen(d) {
			read.cancel()
			delete(m.authenticationReads, d)
			if m.authenticationDialogOpen(d) {
				d.CompleteRead(d.BeginRead(), nil, errors.New("workspace changed; reload authentication status"))
				d.SetOperation("", false, false)
				m.showAuthenticationOperation(d)
			}
		}
	}
	for ws, operation := range m.authenticationOperations {
		if preparation := operation.recoveryPreparing; preparation != nil && (ws != m.com.Workspace || !m.authenticationDialogOpen(preparation.dialog)) {
			operation.recoveryPreparing = nil
			operation.message = "Recovery preparation cancelled; the original authentication receipt remains available."
			m.updateAuthenticationDialogs()
		}
		if operation.preparing && (ws != m.com.Workspace || !m.authenticationDialogOpen(operation.dialog)) {
			// No mutation has launched, so closing during random-ID preparation is
			// cancellation. A command already dispatched has a separate lifetime.
			delete(m.authenticationOperations, ws)
			operation.dialog.SetOperation("Authentication preparation cancelled; no operation was dispatched.", false, false)
		}
	}
}

func (m *UI) openAuthenticationAccounts(logout bool) tea.Cmd {
	var d *dialog.AccountAuthentication
	if logout {
		picker := dialog.NewLogout(m.com)
		d = picker.AuthenticationState()
		m.dialog.CloseDialog(dialog.LogoutID)
		m.dialog.OpenDialog(picker)
	} else {
		picker := dialog.NewAccountSwitcher(m.com)
		d = picker.AuthenticationState()
		m.dialog.CloseDialog(dialog.AccountSwitcherID)
		m.dialog.OpenDialog(picker)
	}
	m.pruneAuthenticationReads()
	m.showAuthenticationOperation(d)
	return m.loadAuthenticationAccounts(d)
}

func (m *UI) showAuthenticationOperation(d *dialog.AccountAuthentication) {
	if operation := m.authenticationOperations[m.com.Workspace]; operation != nil {
		message := operation.message
		if state := m.authenticationReconciliations[operation]; state != nil && state.message != "" {
			message += "\nSeparate review: " + state.message
		}
		d.SetOperation(message, operation.pending || operation.preparing || operation.recoveryPreparing != nil || m.authenticationReconciliationBusy(operation), operation.retry)
		d.SetRecovery(operation.retry && operation.blockNew && operation.recoverer != nil, operation.retry && operation.recovery != nil)
		d.SetReview(operation.retry || m.authenticationReconciliations[operation] != nil)
	} else {
		d.SetOperation("", false, false)
		d.SetRecovery(false, false)
		d.SetReview(false)
	}
}

func (m *UI) loadAuthenticationAccounts(d *dialog.AccountAuthentication) tea.Cmd {
	if !m.authenticationDialogOpen(d) {
		return nil
	}
	if old := m.authenticationReads[d]; old != nil {
		old.cancel()
	}
	if m.authenticationReads == nil {
		m.authenticationReads = make(map[*dialog.AccountAuthentication]*authenticationRead)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	read := &authenticationRead{workspace: m.com.Workspace, dialog: d, generation: d.BeginRead(), cancel: cancel}
	m.authenticationReads[d] = read
	logout := d.ID() == dialog.LogoutID
	return func() tea.Msg {
		defer cancel()
		rows, err := dialog.LoadAuthenticationRows(ctx, read.workspace, logout)
		return authenticationLoadedMsg{read: read, rows: rows, err: err}
	}
}

func (m *UI) completeAuthenticationRead(msg authenticationLoadedMsg) {
	read := msg.read
	if read == nil || m.authenticationReads[read.dialog] != read || read.completed {
		return
	}
	// Retain the source identity while its rows remain selectable. A different
	// Workspace must never execute a row loaded by this one.
	read.completed = true
	read.rows = append([]dialog.AuthenticationRow(nil), msg.rows...)
	read.cancel()
	if m.com.Workspace == read.workspace && m.authenticationDialogOpen(read.dialog) {
		read.dialog.CompleteRead(read.generation, msg.rows, msg.err)
	}
}

func (m *UI) beginAuthenticationOperation(action dialog.ActionAuthenticationSelect) tea.Cmd {
	if !m.authenticationDialogOpen(action.Dialog) || action.Dialog.Generation() != action.Generation {
		return nil
	}
	read := m.authenticationReads[action.Dialog]
	if read == nil || !read.completed || read.workspace != m.com.Workspace || read.generation != action.Generation {
		return util.ReportError(errors.New("authentication workspace changed; reload status before selecting an account"))
	}
	if current := m.authenticationOperations[m.com.Workspace]; current != nil {
		if current.pending || current.preparing || current.recoveryPreparing != nil || m.authenticationReconciliationBusy(current) {
			return nil
		}
		if current.blockNew {
			return util.ReportError(errors.New("Resolve the original " + current.description() + " before selecting another account. Ctrl+T retries its original receipt; Alt+R attempts recovery using that receipt."))
		}
		delete(m.authenticationReconciliations, current)
	}
	if err := action.Row.Target.Validate(); err != nil {
		return util.ReportError(err)
	}
	if m.authenticationOperations == nil {
		m.authenticationOperations = make(map[workspace.Workspace]*authenticationOperation)
	}
	operation := &authenticationOperation{workspace: m.com.Workspace, dialog: action.Dialog, generation: action.Generation, row: action.Row, logout: action.Dialog.ID() == dialog.LogoutID, preparing: true, message: "Preparing authentication operation…"}
	for _, row := range read.rows {
		if row.Target.Owner == action.Row.Target.Owner && row.AccountID != "" {
			operation.accountChoices = append(operation.accountChoices, row)
		}
	}
	if recoverer, ok := operation.workspace.(workspace.ProviderAuthenticationRecoverer); ok && recoverer.CanRecoverProviderAuthentication() {
		operation.recoverer = recoverer
	}
	m.authenticationOperations[operation.workspace] = operation
	m.updateAuthenticationDialogs()
	return prepareAuthenticationOperation(operation)
}

func prepareAuthenticationOperation(operation *authenticationOperation) tea.Cmd {
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		return authenticationPreparedMsg{operation: operation, id: hex.EncodeToString(id[:]), err: err}
	}
}

func (m *UI) completeAuthenticationPreparation(msg authenticationPreparedMsg) tea.Cmd {
	operation := msg.operation
	if operation == nil || m.authenticationOperations[operation.workspace] != operation || !operation.preparing {
		return nil
	}
	if operation.workspace != m.com.Workspace || !m.authenticationDialogOpen(operation.dialog) || operation.dialog.Generation() != operation.generation {
		delete(m.authenticationOperations, operation.workspace)
		operation.dialog.SetOperation("Authentication preparation cancelled; no operation was dispatched.", false, false)
		return nil
	}
	operation.preparing = false
	if msg.err != nil {
		operation.message = "Could not prepare authentication operation: " + msg.err.Error()
		m.updateAuthenticationDialogs()
		return util.ReportError(errors.New(operation.message))
	}
	operation.id = msg.id
	return m.dispatchAuthenticationOperation(operation)
}

func (m *UI) retryAuthenticationOperation(action dialog.ActionAuthenticationRetry) tea.Cmd {
	if !m.authenticationDialogOpen(action.Dialog) {
		return nil
	}
	operation := m.authenticationOperations[m.com.Workspace]
	if operation == nil || !operation.retry || operation.pending || operation.preparing || operation.recoveryPreparing != nil || m.authenticationReconciliationBusy(operation) {
		return nil
	}
	return m.dispatchAuthenticationOperation(operation)
}

func (m *UI) dispatchAuthenticationOperation(operation *authenticationOperation) tea.Cmd {
	operation.pending, operation.delivered = true, false
	operation.attempt++
	operation.message = operation.description() + ": waiting for authentication acknowledgement…"
	if operation.retry {
		operation.message = "Retrying original receipt for " + operation.description() + "…"
	}
	m.updateAuthenticationDialogs()
	// Only immutable values are used off-thread. The operation pointer is a
	// completion identity, not a mutable state object for the command to inspect.
	ws, row, logout, id, attempt := operation.workspace, operation.row, operation.logout, operation.id, operation.attempt
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		var outcome providerauth.MutationOutcome
		var err error
		if logout {
			request := providerauth.LogoutRequest{OperationID: id, Target: row.Target}
			if err = request.Validate(); err == nil {
				outcome, err = ws.LogoutProvider(ctx, request)
			}
		} else {
			request := providerauth.SwitchRequest{OperationID: id, Target: row.Target, AccountID: row.AccountID}
			if err = request.Validate(); err == nil {
				outcome, err = ws.SwitchProviderAccount(ctx, request)
			}
		}
		return authenticationCompletedMsg{operation: operation, attempt: attempt, outcome: outcome, err: err}
	}
}

func (m *UI) updateAuthenticationDialogs() {
	for _, id := range []string{dialog.AccountSwitcherID, dialog.LogoutID} {
		if current, ok := m.dialog.Dialog(id).(interface {
			AuthenticationState() *dialog.AccountAuthentication
		}); ok {
			m.showAuthenticationOperation(current.AuthenticationState())
		}
	}
}

func (m *UI) completeAuthenticationOperation(msg authenticationCompletedMsg) tea.Cmd {
	operation := msg.operation
	if operation == nil || m.authenticationOperations[operation.workspace] != operation || operation.attempt != msg.attempt || operation.delivered {
		return nil
	}
	if msg.recovery != nil && (operation.recovery == nil || *operation.recovery != *msg.recovery) {
		return nil
	}
	operation.pending, operation.delivered = false, true
	validationErr := msg.outcome.ValidateSwitch(providerauth.SwitchRequest{OperationID: operation.id, Target: operation.row.Target, AccountID: operation.row.AccountID})
	if operation.logout {
		validationErr = msg.outcome.ValidateLogout(providerauth.LogoutRequest{OperationID: operation.id, Target: operation.row.Target})
	}
	accepted := validationErr == nil && msg.err == nil && msg.outcome.Change != nil && !msg.outcome.Superseded
	historical := validationErr == nil && msg.err == nil && msg.outcome.Superseded
	operation.retry = !accepted && !historical
	if accepted || historical {
		operation.blockNew = false
	} else if operation.recoverer != nil {
		progress := msg.outcome.Progress
		operation.blockNew = operation.blockNew || validationErr != nil || progress.AccountRefreshed || progress.AccountsSaved || progress.ConfigSaved || progress.RuntimePublished
	}
	switch {
	case accepted:
		operation.message = "Switched account to " + operation.row.Label + "."
		if operation.logout {
			operation.message = "Logged out of " + operation.row.Label + "."
		}
	case historical:
		operation.message = "The original operation (" + operation.description() + ") completed; its receipt no longer describes the current authentication state. Reload status to inspect it."
	default:
		operation.message = "Authentication result could not be confirmed for " + operation.description() + "."
		if msg.err != nil {
			operation.message += " " + msg.err.Error()
		}
		if validationErr != nil && msg.err == nil {
			operation.message += " The acknowledgement did not match the original request."
		}
		if validationErr == nil {
			operation.message += authenticationProgressText(msg.outcome.Progress)
		}
		operation.message += " Ctrl+T retries the original operation; Ctrl+R reloads status only. Receipts are limited to this workspace's recent operations."
		if operation.blockNew && operation.recoverer != nil {
			operation.message += " Alt+R attempts recovery from the original receipt; partial local changes may require saved-state reconciliation."
			if operation.recovery != nil {
				operation.message += " Alt+T retries the last recovery receipt without another publication attempt."
			}
		}
	}
	sameWorkspace := operation.workspace == m.com.Workspace
	message := operation.message
	if !sameWorkspace {
		message = "Previous workspace: " + message
	}
	info := util.NewInfoMsg(message)
	if operation.retry {
		info = util.NewErrorMsg(errors.New(message))
	}
	if historical {
		info = util.NewWarnMsg(message)
	}
	cmds := []tea.Cmd{util.CmdHandler(info)}
	if sameWorkspace {
		m.updateAuthenticationDialogs()
		if accepted {
			// Workspace owns publication. Refresh presentation only; never rebuild or
			// restore the model state carried by a historical acknowledgement.
			m.invalidateBusyCaches()
			cmds = append(cmds, m.dispatchBusyRefresh())
			m.providerUsage = nil
			m.usageFetchGen++
			cmds = append(cmds, authenticationUsageCommand(operation.workspace, m.usageFetchGen))
			for _, id := range []string{dialog.AccountSwitcherID, dialog.LogoutID} {
				if current, ok := m.dialog.Dialog(id).(interface {
					AuthenticationState() *dialog.AccountAuthentication
				}); ok {
					// A replacement dialog is not closed or populated by the old result.
					// Its own explicit read/reload remains authoritative for its rows.
					if current.AuthenticationState() == operation.dialog {
						cmds = append(cmds, m.loadAuthenticationAccounts(operation.dialog))
					}
				}
			}
		}
	}
	return tea.Batch(cmds...)
}

func (operation *authenticationOperation) description() string {
	if operation.logout {
		return "logout from " + operation.row.Label
	}
	return "switch to " + operation.row.Label + " on " + operation.row.Target.Owner.ProviderID
}

func authenticationProgressText(progress providerauth.MutationProgress) string {
	var effects []string
	if progress.AccountRefreshed {
		effects = append(effects, "account refreshed")
	}
	if progress.AccountsSaved {
		effects = append(effects, "accounts saved")
	}
	if progress.ConfigSaved {
		effects = append(effects, "configuration saved")
	}
	if progress.RuntimePublished {
		effects = append(effects, "local runtime published")
	}
	if len(effects) == 0 {
		return " No durable effects were confirmed; this does not establish rollback."
	}
	return " Confirmed local effects: " + strings.Join(effects, ", ") + "."
}

func authenticationUsageCommand(ws workspace.Workspace, generation uint64) tea.Cmd {
	return func() tea.Msg {
		result := authenticationUsageMsg{workspace: ws, generation: generation}
		cfg := ws.Config()
		if cfg == nil {
			return result
		}
		selected := cfg.Models[config.SelectedModelTypeLarge]
		surface, ok := providerregistry.LookupSurface(ws.ProviderSurfaces(), selected.Provider)
		if !ok || !surface.Available || !surface.UsageAvailable || surface.Owner == nil {
			return result
		}
		fetch := ws.PrepareProviderUsage(*surface.Owner)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result.usage, _ = fetch(ctx)
		return result
	}
}
