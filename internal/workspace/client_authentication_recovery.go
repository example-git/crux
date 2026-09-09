package workspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
)

// The UI creates one ID/sequence for each explicit recovery action, starting
// at sequence 1. Transport retries retain every field. A later explicit action
// increments the sequence; it never creates another Switch/Logout operation.
// The result is the original operation's outcome. An asynchronous UI bridge
// must retain RecoveryID/RecoverySequence when routing this local result.
type clientAuthenticationRecoveryRequest struct {
	RecoveryID       string
	OperationID      string
	Target           providerauth.Target
	RecoverySequence uint64
}

func (request clientAuthenticationRecoveryRequest) validate() error {
	if request.RecoverySequence == 0 {
		return errors.New("invalid authentication recovery sequence")
	}
	for _, id := range []string{request.OperationID, request.RecoveryID} {
		if err := (providerauth.LogoutRequest{OperationID: id, Target: request.Target}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

type clientAuthenticationRecoveryReceipt struct {
	request  clientAuthenticationRecoveryRequest
	proposal *config.RemoteRuntimeProposal
	err      error
}

func (clientAuthenticationRecoveryReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("client authentication recovery receipts are private")
}
func (clientAuthenticationRecoveryReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private client authentication recovery receipt]"))
}

// Recovery is an explicit publication action. It never switches, refreshes, or
// removes an account and never writes local configuration. A failed collection
// may be repeated only by a new recovery action and only from the same original
// completed capture. Drift requires a separate explicit reconciliation flow.
func (w *ClientWorkspace) recoverClientAuthentication(ctx context.Context, request clientAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	initial := providerauth.MutationOutcome{OperationID: request.OperationID, Previous: request.Target}
	if err := request.validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	a := w.authority
	if a == nil || a.store == nil || w.client == nil || !w.clientOwned() {
		return initial, errors.New("owning client authentication authority is unavailable")
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	if w.authority != a || w.workspaceID() != request.Target.WorkspaceID || !w.clientOwned() {
		return initial, providerauth.ErrStale
	}
	original := a.authenticationReceipts[request.OperationID]
	if original == nil {
		return initial, providerauth.ErrStale
	}
	if original.request.target != request.Target || original.principal != a.principal {
		return initial, providerauth.ErrOperationConflict
	}
	if original.savedStateSupersededBy != "" {
		return clientAuthenticationOutcome(original, errors.New("a separate fresh saved-state action superseded this publication; the original result is unchanged"))
	}
	if original.reconciledBy != "" {
		return clientAuthenticationOutcome(original, errors.New("saved authentication was published by a separate reviewed action; the original mutation result is unchanged"))
	}
	if a.pendingAuthenticationReview(request.Target.WorkspaceID) {
		return clientAuthenticationOutcome(original, errors.New("acknowledge the reviewed authentication publication through its apply action"))
	}
	if recovery := a.authenticationRecoveries[request.RecoveryID]; recovery != nil {
		if recovery.request != request {
			return clientAuthenticationOutcome(original, providerauth.ErrOperationConflict)
		}
		return w.replayClientAuthenticationRecoveryLocked(ctx, a, original, recovery)
	}
	if request.RecoverySequence <= original.recoverySequence {
		return clientAuthenticationOutcome(original, providerauth.ErrStale)
	}
	original.recoverySequence = request.RecoverySequence
	recovery := &clientAuthenticationRecoveryReceipt{request: request, proposal: original.proposal}
	a.retainClientAuthenticationRecovery(recovery)
	if original.acknowledged {
		return w.replayClientAuthenticationRecoveryLocked(ctx, a, original, recovery)
	}
	if original.outcome.Change == nil || !(original.outcome.Progress.RuntimePublished || original.removalAdmitted && !original.removalActive && original.outcome.Progress.AccountsSaved) || !original.after.SameObservation(original.after) {
		recovery.err = errors.New("authentication recovery has no complete local capture; explicit saved-state reconciliation is required")
		return clientAuthenticationOutcome(original, recovery.err)
	}
	if err := w.verifyClientAuthenticationRecoveryCache(a, original); err != nil {
		recovery.err = err
		return clientAuthenticationOutcome(original, err)
	}
	remote, err := w.client.GetWorkspace(ctx, request.Target.WorkspaceID)
	if err != nil {
		recovery.err = clientAuthenticationFailure("authentication recovery cannot verify receiver authority", err)
		return clientAuthenticationOutcome(original, recovery.err)
	}
	if remote.ID != request.Target.WorkspaceID {
		recovery.err = providerauth.ErrStale
		return clientAuthenticationOutcome(original, recovery.err)
	}
	if original.proposal != nil && matchesAuthority(remote.Authority, original.principal, *original.proposal) {
		return w.acknowledgeClientAuthenticationRecoveryLocked(ctx, a, original, remote)
	}
	if !matchesAuthority(remote.Authority, original.principal, original.base) || a.accepted.Revision != original.base.Revision || a.accepted.Digest != original.base.Digest {
		recovery.err = errors.New("authentication recovery receiver no longer has the original accepted authority")
		return clientAuthenticationOutcome(original, recovery.err)
	}
	if err := w.verifyClientAuthenticationRecoveryCapture(ctx, a, original); err != nil {
		recovery.err = err
		return clientAuthenticationOutcome(original, err)
	}
	if original.removalAdmitted && !original.removalActive {
		recovery.err = errors.New("inactive removal has no matching unchanged receiver authority; reload and reconcile explicitly")
		return clientAuthenticationOutcome(original, recovery.err)
	}
	if original.proposal == nil {
		if original.base.Revision == ^uint64(0) {
			recovery.err = errors.New("client runtime revision exhausted")
			return clientAuthenticationOutcome(original, recovery.err)
		}
		proposal, err := a.store.CollectRemoteRuntimeForAuthentication(ctx, original.after, original.base.Revision+1, original.removed)
		if err != nil {
			recovery.err = clientAuthenticationFailure("authentication recovery exact collection failed", err)
			return clientAuthenticationOutcome(original, recovery.err)
		}
		original.proposal = &proposal
	}
	recovery.proposal = original.proposal
	if err := w.verifyClientAuthenticationRecoveryCapture(ctx, a, original); err != nil {
		recovery.err = err
		return clientAuthenticationOutcome(original, err)
	}
	if err := w.verifyClientAuthenticationRecoveryCache(a, original); err != nil {
		recovery.err = err
		return clientAuthenticationOutcome(original, err)
	}
	if err := ctx.Err(); err != nil {
		recovery.err = err
		return clientAuthenticationOutcome(original, err)
	}
	// These fields now describe the authorized recovery attempt; the original
	// mutation's ordinary retry remains GET-only and cannot resend this PUT.
	original.err = nil
	a.pending, a.pendingView = original.proposal, original.proposal.CollectionConfig()
	ack, err := w.client.ReplaceRemoteRuntime(ctx, request.Target.WorkspaceID, original.base.Revision, *original.proposal)
	if err == nil && matchesAuthority(ack, original.principal, *original.proposal) {
		return w.completeClientAuthenticationPutLocked(ctx, a, original, ack)
	}
	return w.reconcileClientAuthenticationLocked(ctx, a, original)
}

func (w *ClientWorkspace) verifyClientAuthenticationRecoveryCache(a *clientAuthority, original *clientAuthenticationReceipt) error {
	current := w.cached()
	if current.ID != original.request.target.WorkspaceID || current.Authority == nil || current.Authority.Mode != "client" || current.Authority.Principal != original.principal {
		return providerauth.ErrStale
	}
	if matchesAuthority(current.Authority, a.principal, a.accepted) {
		return nil
	}
	if original.proposal != nil && matchesAuthority(current.Authority, a.principal, *original.proposal) {
		return nil
	}
	return errors.New("authentication recovery cached authority changed")
}

func (w *ClientWorkspace) verifyClientAuthenticationRecoveryCapture(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt) error {
	current, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return clientAuthenticationFailure("authentication recovery cannot verify the original local capture", err)
	}
	if !original.after.SameObservation(current) {
		return errors.New("authentication recovery local capture changed; explicit saved-state reconciliation is required")
	}
	return nil
}

func (w *ClientWorkspace) replayClientAuthenticationRecoveryLocked(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, recovery *clientAuthenticationRecoveryReceipt) (providerauth.MutationOutcome, error) {
	if original.acknowledged && matchesAuthority(w.cached().Authority, a.principal, a.accepted) && original.proposal != nil && a.accepted.Revision >= original.proposal.Revision {
		return w.clientAuthenticationAcknowledgedOutcome(ctx, a, original)
	}
	if recovery.proposal == nil {
		if recovery.err != nil {
			return clientAuthenticationOutcome(original, recovery.err)
		}
		return clientAuthenticationOutcome(original, providerauth.ErrReceiptUnverified)
	}
	return w.reconcileClientAuthenticationLocked(ctx, a, original)
}

func (w *ClientWorkspace) acknowledgeClientAuthenticationRecoveryLocked(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, remote *proto.Workspace) (providerauth.MutationOutcome, error) {
	original.acknowledged = true
	original.err = nil
	if a.accepted.Revision <= original.proposal.Revision {
		if err := w.adoptClientAuthenticationLocked(ctx, a, original, remote.Authority); err != nil {
			return clientAuthenticationOutcome(original, err)
		}
		w.adoptRuntimeResponse(*remote)
	}
	return w.clientAuthenticationAcknowledgedOutcome(ctx, a, original)
}

func (a *clientAuthority) retainClientAuthenticationRecovery(receipt *clientAuthenticationRecoveryReceipt) {
	if a.authenticationRecoveries == nil {
		a.authenticationRecoveries = map[string]*clientAuthenticationRecoveryReceipt{}
	}
	if len(a.authenticationRecoveryIDs) == clientAuthenticationReceiptLimit {
		delete(a.authenticationRecoveries, a.authenticationRecoveryIDs[0])
		a.authenticationRecoveryIDs = a.authenticationRecoveryIDs[1:]
	}
	a.authenticationRecoveryIDs = append(a.authenticationRecoveryIDs, receipt.request.RecoveryID)
	a.authenticationRecoveries[receipt.request.RecoveryID] = receipt
}

func (a *clientAuthority) unacknowledgedClientAuthentication(id string) bool {
	// A separately reviewed publication can follow an admitted operation that
	// made no local changes. Its attempted proposal still needs exact adoption
	// before generic paths may publish or change the retained capture.
	if a.pendingAuthenticationReview(id) {
		return true
	}
	for _, receipt := range a.authenticationReceipts {
		if receipt.request.target.WorkspaceID == id && receipt.reconciledBy == "" && receipt.savedStateSupersededBy == "" && (!receipt.acknowledged || !receipt.adopted) && clientAuthenticationChanged(receipt.outcome.Progress) {
			return true
		}
	}
	return false
}

// Callers hold a.mu. Generic runtime mutations cannot authorize recovery of
// an earlier saved authentication operation or discard its intended removals.
func (a *clientAuthority) requireAuthenticationPublication(id string) error {
	if a.recoveryProposal != nil {
		return errors.New("workspace recreation is awaiting acknowledgement; retry the same recovery before publishing client runtime changes")
	}
	return a.requireAuthenticationReceipt(id)
}

func (a *clientAuthority) requireAuthenticationReceipt(id string) error {
	if a.unacknowledgedClientAuthentication(id) {
		return errors.New("recover the saved authentication operation before publishing client runtime changes")
	}
	return nil
}
