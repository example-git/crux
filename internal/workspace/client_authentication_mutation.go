package workspace

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
)

const clientAuthenticationReceiptLimit = 128

type clientAuthenticationRequest struct {
	operationID      string
	target           providerauth.Target
	accountID        string
	removedAccountID string
	logout           bool
	checkID          string
	loginID          string
}

// A local receipt is not an acknowledgement. Keep the exact collected proposal
// even after generic pending reconciliation concludes the PUT was rejected.
// Saved TLS connections also retain this receipt in the captured private journal.
// A restored receipt is historical until exact receiver/capture evidence agrees.
type clientAuthenticationReceipt struct {
	abandon                                   *ProviderAuthenticationAbandonRequest
	journalRevision                           uint64
	journalCompleted, restored, localFinished bool
	observation, credentialEffectID           string
	request                                   clientAuthenticationRequest
	principal                                 string
	base                                      config.RemoteRuntimeProposal
	outcome                                   providerauth.MutationOutcome
	after                                     config.AuthenticationCapture
	owner                                     providerregistry.RegistrationOwner
	removed                                   map[providerregistry.RegistrationOwner]bool
	proposal                                  *config.RemoteRuntimeProposal
	acknowledged                              bool
	adopted                                   bool
	recoverySequence                          uint64
	reviewSequence                            uint64
	savedStateSupersededBy                    string
	reconciledBy                              string
	pendingReview                             string
	err                                       error
	removalSuccessor                          string
	removalActive                             bool
	removalAdmitted                           bool
	oauthTokenID                              string
}

func (clientAuthenticationReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("client authentication receipts are private")
}

func (clientAuthenticationReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private client authentication receipt]"))
}

func (w *ClientWorkspace) switchClientAuthentication(ctx context.Context, request providerauth.SwitchRequest) (providerauth.MutationOutcome, error) {
	if err := request.Validate(); err != nil {
		return providerauth.MutationOutcome{}, err
	}
	return w.mutateClientAuthentication(ctx, clientAuthenticationRequest{operationID: request.OperationID, target: request.Target, accountID: request.AccountID})
}

func (w *ClientWorkspace) logoutClientAuthentication(ctx context.Context, request providerauth.LogoutRequest) (providerauth.MutationOutcome, error) {
	if err := request.Validate(); err != nil {
		return providerauth.MutationOutcome{}, err
	}
	return w.mutateClientAuthentication(ctx, clientAuthenticationRequest{operationID: request.OperationID, target: request.Target, logout: true})
}

func (w *ClientWorkspace) mutateClientAuthentication(ctx context.Context, request clientAuthenticationRequest) (outcome providerauth.MutationOutcome, failure error) {
	initial := providerauth.MutationOutcome{OperationID: request.operationID, CheckID: request.checkID, LoginID: request.loginID, RemovedAccountID: request.removedAccountID, Previous: request.target}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	a := w.authority
	if a == nil || a.store == nil || w.client == nil || !w.clientOwned() {
		return initial, errors.New("owning client authentication authority is unavailable")
	}
	if request.target.WorkspaceID != w.workspaceID() {
		return initial, providerauth.ErrStale
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	if request.target.WorkspaceID != w.workspaceID() || w.authority != a || !w.clientOwned() {
		return initial, providerauth.ErrStale
	}
	if err := a.loadAuthenticationJournal(ctx, request.target.WorkspaceID); err != nil {
		return initial, err
	}
	var releaseOperation func()
	defer func() {
		if releaseOperation == nil {
			return
		}
		if err := a.finishAuthenticationJournal(ctx); err != nil {
			failure = errors.Join(failure, err)
		}
		if releaseOperation != nil {
			releaseOperation()
		}
	}()
	if receipt := a.authenticationReceipts[request.operationID]; receipt != nil {
		var err error
		releaseOperation, err = a.acquireAuthenticationPublication(ctx, request.target.WorkspaceID)
		if err != nil {
			return initial, err
		}
		if err = a.loadAuthenticationJournal(ctx, request.target.WorkspaceID); err != nil {
			return initial, err
		}
		receipt = a.authenticationReceipts[request.operationID]
		if receipt.request != request {
			return initial, providerauth.ErrOperationConflict
		}
		return w.replayClientAuthenticationLocked(ctx, a, receipt)
	}
	if request.target.Generation.Epoch == a.authenticationRetired.Epoch && request.target.Generation.Sequence <= a.authenticationRetired.Sequence {
		return initial, providerauth.ErrStale
	}
	if err := w.prepareClientProviderAuth(ctx, request.target.WorkspaceID); err != nil {
		return initial, err
	}
	// An unfinished local change requires explicit recovery. A new ID/target
	// cannot silently adopt it or perform a second local transaction.
	if a.unacknowledgedClientAuthentication(request.target.WorkspaceID) {
		return initial, errors.New("a saved client authentication change is not acknowledged; explicit recovery of its original operation is required")
	}
	if a.accepted.Revision == ^uint64(0) && request.removedAccountID == "" {
		return initial, errors.New("client runtime revision exhausted")
	}
	if request.loginID != "" {
		state, err := a.providerAuth.WaitOAuthLogin(ctx, providerauth.OAuthLoginRef{LoginID: request.loginID, OperationID: request.operationID, Target: request.target}, 0)
		if err != nil {
			return initial, err
		}
		if state.Phase != providerauth.OAuthLoginAuthorized {
			return initial, errors.New("OAuth login is not authorized for completion")
		}
	}
	before, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return initial, err
	}
	var intentOwner providerregistry.RegistrationOwner
	for _, provider := range before.Providers() {
		if providerauth.PublicOwner(provider.Owner) == request.target.Owner {
			intentOwner = provider.Owner
			break
		}
	}
	if intentOwner.ProviderID == "" {
		return initial, providerauth.ErrOwner
	}
	receipt := &clientAuthenticationReceipt{request: request, principal: a.principal, base: a.accepted, outcome: initial, owner: intentOwner}
	if request.loginID == "" {
		releaseOperation, err = a.acquireAuthenticationPublication(ctx, request.target.WorkspaceID)
		if err != nil {
			return initial, err
		}
		if err := a.persistAuthenticationReceipt(ctx, receipt); err != nil {
			return initial, err
		}
		a.retainClientAuthentication(receipt)
	}
	var local providerauth.MutationResult
	admittedIntent := request.loginID == ""
	if request.loginID != "" {
		// Service preflight remains retryable until its admission hook records
		// the intent. Then keep the fixed commit's lease until its result is known. The
		// caller still controls collection/publication below; cancellation there
		// retains the completed capture for explicit recovery instead of losing
		// an in-progress local result and allowing an unrelated publication.
		admissionCtx := providerauth.WithOAuthCompletionAdmission(w.subCtx, func(admission context.Context) (func(), error) {
			lease, err := a.acquireAuthenticationPublication(admission, request.target.WorkspaceID)
			if err != nil {
				return nil, err
			}
			if err := a.persistAuthenticationReceipt(admission, receipt); err != nil {
				lease()
				return nil, err
			}
			a.retainClientAuthentication(receipt)
			admittedIntent = true
			outer, worker := shareAuthenticationLease(lease)
			releaseOperation = outer
			return worker, nil
		})
		local, err = a.providerAuth.CompleteOAuthLoginForAccepted(admissionCtx, providerauth.OAuthLoginRef{LoginID: request.loginID, OperationID: request.operationID, Target: request.target}, a.accepted, a.configView())
	} else if request.checkID != "" {
		local, err = a.providerAuth.SaveAPIKeyForAccepted(ctx, providerauth.APIKeySaveRequest{OperationID: request.operationID, Target: request.target, CheckID: request.checkID}, a.accepted, a.configView())
	} else if request.removedAccountID != "" {
		local, err = a.providerAuth.RemoveForAccepted(ctx, providerauth.RemoveRequest{OperationID: request.operationID, Target: request.target, AccountID: request.removedAccountID}, a.accepted, a.configView())
	} else if request.logout {
		local, err = a.providerAuth.LogoutForAccepted(ctx, providerauth.LogoutRequest{OperationID: request.operationID, Target: request.target}, a.accepted, a.configView())
	} else {
		local, err = a.providerAuth.SwitchForAccepted(ctx, providerauth.SwitchRequest{OperationID: request.operationID, Target: request.target, AccountID: request.accountID}, a.accepted, a.configView())
	}
	if !admittedIntent {
		return local.Outcome, err
	}
	receipt.outcome, receipt.err = local.Outcome, err
	receipt.localFinished = !local.LocalCommitPending()
	if local.LocalCommitPending() {
		return clientAuthenticationOutcome(receipt, err)
	}
	receipt.removalSuccessor, receipt.removalActive, receipt.removalAdmitted = local.OriginalRemovalSelection()
	receipt.oauthTokenID, _ = local.OriginalOAuthTokenCredentialID()
	receipt.credentialEffectID, _ = local.OriginalConfiguredCredentialEffectID()
	admittedOwner, admitted := local.OriginalOwner()
	if admitted && admittedOwner != receipt.owner {
		receipt.err = providerauth.ErrOwner
		return clientAuthenticationOutcome(receipt, receipt.err)
	}
	if err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	after, current := local.AuthenticationCapture()
	if !current {
		receipt.err = errors.New("local authentication receipt is historical and has no receiver acknowledgement; explicit recovery is required")
		return clientAuthenticationOutcome(receipt, receipt.err)
	}
	receipt.after = after
	if err := ctx.Err(); err != nil {
		// The admitted local writer returned its real result. Retain that
		// capture for the deferred durable finish, while the caller's canceled
		// publication request remains canceled and performs no receiver PUT.
		receipt.err = err
		return clientAuthenticationOutcome(receipt, err)
	}
	if err := a.persistAuthenticationReceipt(ctx, receipt); err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	ownerRetained := false
	for _, provider := range after.Providers() {
		if provider.Owner == receipt.owner && providerauth.PublicOwner(provider.Owner) == request.target.Owner {
			ownerRetained = true
			break
		}
	}
	if !ownerRetained {
		receipt.err = providerauth.ErrOwner
		return clientAuthenticationOutcome(receipt, receipt.err)
	}
	if request.removedAccountID != "" && !receipt.removalActive {
		// The config transaction proved credentials and publication unchanged.
		// Retain the original accepted proposal before any cancelable GET, so
		// retries and explicit recovery only verify it and never issue a PUT.
		receipt.removed = maps.Clone(a.removed)
		if err := after.ValidateAcceptedAuthentication(a.accepted, a.configView()); err != nil {
			receipt.err = err
			return clientAuthenticationOutcome(receipt, err)
		}
		proposal := receipt.base
		receipt.proposal = &proposal
		if err := a.persistAuthenticationReceipt(ctx, receipt); err != nil {
			return clientAuthenticationOutcome(receipt, err)
		}
		if err := w.verifyClientProviderAuthAuthority(request.target.WorkspaceID, a); err != nil {
			return clientAuthenticationOutcome(receipt, err)
		}
		return w.reconcileClientAuthenticationLocked(ctx, a, receipt)
	}
	removed := maps.Clone(a.removed)
	if removed == nil {
		removed = map[providerregistry.RegistrationOwner]bool{}
	}
	if request.logout || receipt.removalAdmitted && receipt.removalActive && receipt.removalSuccessor == "" {
		removed[receipt.owner] = true
	} else {
		delete(removed, receipt.owner)
	}
	receipt.removed = maps.Clone(removed)
	proposal, err := a.store.CollectRemoteRuntimeForAuthentication(ctx, after, a.accepted.Revision+1, removed)
	if err != nil {
		receipt.err = clientAuthenticationFailure("client authentication saved; exact runtime collection failed; explicit recovery is required", err)
		return clientAuthenticationOutcome(receipt, receipt.err)
	}
	receipt.proposal = &proposal
	if err := a.persistAuthenticationReceipt(ctx, receipt); err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	if err := w.verifyClientProviderAuthAuthority(request.target.WorkspaceID, a); err != nil {
		receipt.err = err
		return clientAuthenticationOutcome(receipt, err)
	}
	if err := ctx.Err(); err != nil {
		receipt.err = err
		return clientAuthenticationOutcome(receipt, err)
	}
	a.pending, a.pendingView = receipt.proposal, proposal.CollectionConfig()
	ack, err := w.client.ReplaceRemoteRuntime(ctx, request.target.WorkspaceID, receipt.base.Revision, proposal)
	if err == nil && matchesAuthority(ack, receipt.principal, proposal) {
		return w.completeClientAuthenticationPutLocked(ctx, a, receipt, ack)
	}
	// Exactly one PUT is attempted for this operation. Even a visibly rejected
	// PUT is retained; retries can prove a missed acknowledgement only by GET.
	return w.reconcileClientAuthenticationLocked(ctx, a, receipt)
}

func (w *ClientWorkspace) completeClientAuthenticationPutLocked(ctx context.Context, a *clientAuthority, receipt *clientAuthenticationReceipt, ack *config.RemoteAuthority) (providerauth.MutationOutcome, error) {
	if receipt.proposal == nil || !matchesAuthority(ack, receipt.principal, *receipt.proposal) {
		return clientAuthenticationOutcome(receipt, providerauth.ErrReceiptUnverified)
	}
	// Preserve genuine remote proof even when cancellation or a changed cache
	// prevents local adoption. The separate adopted flag keeps generic paths
	// from publishing until the accepted view and removal intent are installed.
	receipt.acknowledged = true
	if err := w.adoptClientAuthenticationLocked(ctx, a, receipt, ack); err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	return w.clientAuthenticationAcknowledgedOutcome(ctx, a, receipt)
}

func clientAuthenticationChanged(progress providerauth.MutationProgress) bool {
	return progress.AccountRefreshed || progress.AccountsSaved || progress.ConfigSaved || progress.RuntimePublished
}

func (a *clientAuthority) retainClientAuthentication(receipt *clientAuthenticationReceipt) {
	if a.authenticationReceipts[receipt.request.operationID] != nil {
		a.authenticationReceipts[receipt.request.operationID] = receipt
		return
	}
	if a.authenticationReceipts == nil {
		a.authenticationReceipts = map[string]*clientAuthenticationReceipt{}
	}
	if len(a.authenticationReceiptIDs) == clientAuthenticationReceiptLimit {
		retired := a.authenticationReceipts[a.authenticationReceiptIDs[0]]
		// The service can retain a successful local result longer than this
		// adapter retains admission failures. An evicted admitted target may
		// never recover that result and collect it as a new remote operation.
		if retired.request.target.WorkspaceID == a.providerAuthWorkspaceID && (retired.outcome.Change != nil || clientAuthenticationChanged(retired.outcome.Progress) || errors.Is(retired.err, providerauth.ErrMutation)) {
			generation := retired.request.target.Generation
			if a.authenticationRetired.Epoch != generation.Epoch || a.authenticationRetired.Sequence < generation.Sequence {
				a.authenticationRetired = generation
			}
		}
		delete(a.authenticationReceipts, a.authenticationReceiptIDs[0])
		a.authenticationReceiptIDs = a.authenticationReceiptIDs[1:]
	}
	a.authenticationReceiptIDs = append(a.authenticationReceiptIDs, receipt.request.operationID)
	a.authenticationReceipts[receipt.request.operationID] = receipt
}

func (w *ClientWorkspace) replayClientAuthenticationLocked(ctx context.Context, a *clientAuthority, receipt *clientAuthenticationReceipt) (providerauth.MutationOutcome, error) {
	if receipt.principal != a.principal || receipt.request.target.WorkspaceID != w.workspaceID() {
		return clientAuthenticationOutcome(receipt, providerauth.ErrStale)
	}
	if receipt.abandon != nil {
		return clientAuthenticationOutcome(receipt, errors.New("original authentication publication was explicitly abandoned; its outcome is unchanged"))
	}
	if receipt.savedStateSupersededBy != "" {
		return clientAuthenticationOutcome(receipt, errors.New("a separate fresh saved-state action superseded this publication; the original result is unchanged"))
	}
	if receipt.reconciledBy != "" {
		return clientAuthenticationOutcome(receipt, errors.New("saved authentication was published by a separate reviewed action; the original mutation result is unchanged"))
	}
	if a.pendingAuthenticationReview(receipt.request.target.WorkspaceID) {
		return clientAuthenticationOutcome(receipt, errors.New("acknowledge the reviewed authentication publication through its apply action"))
	}
	if receipt.err != nil && (!receipt.restored || receipt.proposal == nil) {
		return clientAuthenticationOutcome(receipt, receipt.err)
	}
	if receipt.proposal == nil {
		return clientAuthenticationOutcome(receipt, providerauth.ErrReceiptUnverified)
	}
	if receipt.acknowledged && matchesAuthority(w.cached().Authority, a.principal, a.accepted) && a.accepted.Revision >= receipt.proposal.Revision {
		return w.clientAuthenticationAcknowledgedOutcome(ctx, a, receipt)
	}
	return w.reconcileClientAuthenticationLocked(ctx, a, receipt)
}

func (w *ClientWorkspace) reconcileClientAuthenticationLocked(ctx context.Context, a *clientAuthority, receipt *clientAuthenticationReceipt) (providerauth.MutationOutcome, error) {
	if err := ctx.Err(); err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	current := w.cached()
	if current.ID != receipt.request.target.WorkspaceID || current.Authority == nil || current.Authority.Mode != "client" || current.Authority.Principal != receipt.principal {
		return clientAuthenticationOutcome(receipt, providerauth.ErrStale)
	}
	if !matchesAuthority(current.Authority, a.principal, a.accepted) && !matchesAuthority(current.Authority, a.principal, *receipt.proposal) {
		return clientAuthenticationOutcome(receipt, errors.New("cached client authentication authority changed; reconcile or reconnect explicitly"))
	}
	remote, err := w.client.GetWorkspace(ctx, receipt.request.target.WorkspaceID)
	if err != nil {
		return clientAuthenticationOutcome(receipt, clientAuthenticationFailure("client authentication saved; receiver acknowledgement is pending", err))
	}
	if remote.ID != receipt.request.target.WorkspaceID || !matchesAuthority(remote.Authority, receipt.principal, *receipt.proposal) {
		return clientAuthenticationOutcome(receipt, errors.New("client authentication saved; receiver has not acknowledged this exact operation"))
	}
	receipt.acknowledged = true
	if err := ctx.Err(); err != nil {
		return clientAuthenticationOutcome(receipt, err)
	}
	// A historical proof must not roll back a later accepted runtime.
	if a.accepted.Revision <= receipt.proposal.Revision {
		if err := w.adoptClientAuthenticationLocked(ctx, a, receipt, remote.Authority); err != nil {
			return clientAuthenticationOutcome(receipt, err)
		}
		w.adoptRuntimeResponse(*remote)
	}
	return w.clientAuthenticationAcknowledgedOutcome(ctx, a, receipt)
}

func (w *ClientWorkspace) adoptClientAuthenticationLocked(ctx context.Context, a *clientAuthority, receipt *clientAuthenticationReceipt, ack *config.RemoteAuthority) error {
	if err := w.restoreAuthenticationReceipt(ctx, a, receipt); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if w.ws.ID != receipt.request.target.WorkspaceID || (!matchesAuthority(w.ws.Authority, a.principal, a.accepted) && !matchesAuthority(w.ws.Authority, a.principal, *receipt.proposal)) {
		return errors.New("client authentication workspace or authority changed before acknowledgement adoption")
	}
	a.accepted = *receipt.proposal
	a.view.Store(receipt.proposal.CollectionConfig())
	w.noteClientAuthenticationAcknowledgementLocked(a, receipt.request.target.WorkspaceID, a.accepted)
	if a.pending != nil && matchesAuthority(ack, a.principal, *a.pending) {
		a.pending, a.pendingView = nil, nil
	}
	copy := *ack
	w.ws.Authority = &copy
	w.appliedRefresh = w.refreshSequence.Add(1)
	return nil
}

// Generic pending reconciliation can establish the same exact proof. Record it
// at that point, before later publications discard the pending proposal.
func (w *ClientWorkspace) noteClientAuthenticationAcknowledgementLocked(a *clientAuthority, id string, proposal config.RemoteRuntimeProposal) {
	for _, receipt := range a.authenticationReceipts {
		if receipt.abandon != nil {
			continue
		}
		if receipt.proposal == nil || receipt.principal != a.principal || receipt.request.target.WorkspaceID != id || receipt.proposal.Revision != proposal.Revision || receipt.proposal.Digest != proposal.Digest {
			continue
		}
		receipt.acknowledged = true
		receipt.adopted = true
		if a.removed == nil {
			a.removed = map[providerregistry.RegistrationOwner]bool{}
		}
		if receipt.removalAdmitted && !receipt.removalActive {
			continue // An inactive removal preserves prior credential publication intent.
		}
		if receipt.request.logout || receipt.removalAdmitted && receipt.removalSuccessor == "" {
			a.removed[receipt.owner] = true
		} else {
			delete(a.removed, receipt.owner)
		}
	}
}

func (w *ClientWorkspace) clientAuthenticationAcknowledgedOutcome(ctx context.Context, a *clientAuthority, receipt *clientAuthenticationReceipt) (providerauth.MutationOutcome, error) {
	outcome, err := clientAuthenticationOutcome(receipt, nil)
	if err != nil {
		return outcome, err
	}
	if a.accepted.Revision != receipt.proposal.Revision || a.accepted.Digest != receipt.proposal.Digest {
		outcome.Superseded = true
		return outcome, ctx.Err()
	}
	current, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return outcome, clientAuthenticationFailure("acknowledged authentication receipt cannot be verified against current local state", err)
	}
	if receipt.restored {
		id, err := current.DurableObservationID()
		if err != nil {
			return outcome, err
		}
		outcome.Superseded = id != receipt.observation
	} else {
		outcome.Superseded = !receipt.after.SameObservation(current)
	}
	return outcome, ctx.Err()
}

func clientAuthenticationOutcome(receipt *clientAuthenticationReceipt, cause error) (providerauth.MutationOutcome, error) {
	data, err := json.Marshal(receipt.outcome)
	if err != nil {
		return providerauth.MutationOutcome{OperationID: receipt.request.operationID, CheckID: receipt.request.checkID, CredentialID: receipt.outcome.CredentialID, LoginID: receipt.request.loginID, RemovedAccountID: receipt.request.removedAccountID, Previous: receipt.request.target, Progress: receipt.outcome.Progress}, providerauth.ErrReceiptUnverified
	}
	var outcome providerauth.MutationOutcome
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&outcome); err != nil {
		return providerauth.MutationOutcome{}, providerauth.ErrReceiptUnverified
	}
	return outcome, cause
}

type clientAuthenticationMutationError struct {
	public string
	cause  error
}

func clientAuthenticationFailure(public string, cause error) error {
	return &clientAuthenticationMutationError{public, cause}
}
func (err *clientAuthenticationMutationError) Error() string { return err.public }
func (err *clientAuthenticationMutationError) Unwrap() error { return err.cause }
func (err *clientAuthenticationMutationError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(err.public))
}
