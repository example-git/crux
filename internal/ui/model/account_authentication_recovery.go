package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"math"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
)

type authenticationRecoveryPreparation struct {
	dialog     *dialog.AccountAuthentication
	generation uint64
	sequence   uint64
}

type authenticationRecoveryPreparedMsg struct {
	operation   *authenticationOperation
	preparation *authenticationRecoveryPreparation
	id          string
	err         error
}

func (m *UI) beginAuthenticationRecovery(action dialog.ActionAuthenticationRecover) tea.Cmd {
	if !m.authenticationDialogOpen(action.Dialog) {
		return nil
	}
	operation := m.authenticationOperations[m.com.Workspace]
	if operation == nil || operation.recoverer == nil || !operation.retry || !operation.blockNew || operation.pending || operation.preparing || operation.recoveryPreparing != nil || m.authenticationReconciliationBusy(operation) {
		return nil
	}
	if action.Retry {
		if operation.recovery == nil {
			return nil
		}
		return m.dispatchAuthenticationRecovery(operation, *operation.recovery, true)
	}
	sequence := uint64(1)
	if operation.recovery != nil {
		if operation.recovery.RecoverySequence == math.MaxUint64 {
			return util.ReportError(errors.New("authentication recovery sequence exhausted; the original receipt is retained"))
		}
		sequence = operation.recovery.RecoverySequence + 1
	}
	preparation := &authenticationRecoveryPreparation{dialog: action.Dialog, generation: action.Dialog.Generation(), sequence: sequence}
	operation.recoveryPreparing = preparation
	operation.message = "Preparing explicit recovery of " + operation.description() + "…"
	m.updateAuthenticationDialogs()
	return func() tea.Msg {
		var id [16]byte
		_, err := rand.Read(id[:])
		return authenticationRecoveryPreparedMsg{operation: operation, preparation: preparation, id: hex.EncodeToString(id[:]), err: err}
	}
}

func (m *UI) completeAuthenticationRecoveryPreparation(msg authenticationRecoveryPreparedMsg) tea.Cmd {
	operation, preparation := msg.operation, msg.preparation
	if operation == nil || preparation == nil || m.authenticationOperations[operation.workspace] != operation || operation.recoveryPreparing != preparation {
		return nil
	}
	operation.recoveryPreparing = nil
	if operation.workspace != m.com.Workspace || !m.authenticationDialogOpen(preparation.dialog) || preparation.dialog.Generation() != preparation.generation {
		operation.message = "Recovery preparation cancelled; the original authentication receipt remains available."
		m.updateAuthenticationDialogs()
		return nil
	}
	if msg.err != nil {
		operation.message = "Could not prepare authentication recovery; the original receipt is retained: " + msg.err.Error()
		m.updateAuthenticationDialogs()
		return util.ReportError(errors.New(operation.message))
	}
	request := workspace.ProviderAuthenticationRecoveryRequest{RecoveryID: msg.id, OperationID: operation.id, Target: operation.row.Target, RecoverySequence: preparation.sequence}
	if err := request.Validate(); err != nil {
		operation.message = "Could not prepare authentication recovery: " + err.Error()
		m.updateAuthenticationDialogs()
		return util.ReportError(errors.New(operation.message))
	}
	operation.recovery = &request
	return m.dispatchAuthenticationRecovery(operation, request, false)
}

func (m *UI) dispatchAuthenticationRecovery(operation *authenticationOperation, request workspace.ProviderAuthenticationRecoveryRequest, retry bool) tea.Cmd {
	operation.pending, operation.delivered = true, false
	operation.attempt++
	operation.message = "Attempting recovery for " + operation.description() + "…"
	if retry {
		operation.message = "Checking the last recovery receipt for " + operation.description() + "…"
	}
	m.updateAuthenticationDialogs()
	recoverer, attempt := operation.recoverer, operation.attempt
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		outcome, err := recoverer.RecoverProviderAuthentication(ctx, request)
		return authenticationCompletedMsg{operation: operation, attempt: attempt, outcome: outcome, err: err, recovery: &request}
	}
}
