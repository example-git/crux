package workspace

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
)

// Review and apply are separate explicit actions. They publish already coherent
// saved state, never repeat or repair the original authentication transaction.
type clientAuthenticationReviewChoice struct {
	Kind      string // empty: original intent; saved-account or saved-logout: new explicit choice
	AccountID string
}

type clientAuthenticationReviewRequest struct {
	OperationID    string
	OriginalTarget providerauth.Target
	ReviewID       string
	ReviewSequence uint64
	Choice         clientAuthenticationReviewChoice
}

type clientAuthenticationApplyRequest struct {
	OperationID    string
	OriginalTarget providerauth.Target
	ReviewID       string
	PreviewID      string
	ApplyID        string
}

type clientAuthenticationReviewModel struct {
	Kind, Provider, Model string
}

// The summary deliberately contains no raw configuration, options, endpoint,
// credentials, metadata, namespace, capture or private proposal.
type clientAuthenticationReviewSummary struct {
	OperationID, ReviewID, PreviewID string
	ReviewSequence                   uint64
	Owner                            providerauth.Owner
	OriginalProgress                 providerauth.MutationProgress
	OriginalLogout                   bool
	OriginalAccountID                string
	Choice                           clientAuthenticationReviewChoice
	ActiveAccountID                  string
	Configured, Disabled             bool
	Models                           []clientAuthenticationReviewModel
	ChangedCategories                []string
	Receiver                         config.RemoteAuthority
}

type clientAuthenticationReconciliationOutcome struct {
	OperationID, ReviewID, PreviewID, ApplyID string
	RemoteAcknowledged, Adopted               bool
	OriginalDisposition                       string
}

type clientAuthenticationApplyReceipt struct {
	request clientAuthenticationApplyRequest
	outcome clientAuthenticationReconciliationOutcome
	err     error
	put     bool
}

type clientAuthenticationReviewReceipt struct {
	request               clientAuthenticationReviewRequest
	summary               clientAuthenticationReviewSummary
	owner                 providerregistry.RegistrationOwner
	capture               config.AuthenticationCapture
	base                  config.RemoteAuthority
	cache                 config.RemoteAuthority
	accepted              config.RemoteAuthority
	priorRemoved, removed map[providerregistry.RegistrationOwner]bool
	proposal              *config.RemoteRuntimeProposal
	apply                 *clientAuthenticationApplyReceipt
	err                   error
}

func (clientAuthenticationReviewReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication review receipts are private")
}
func (clientAuthenticationReviewReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication review receipt]"))
}
func (clientAuthenticationApplyReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication apply receipts are private")
}
func (clientAuthenticationApplyReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication apply receipt]"))
}

func validateAuthenticationReviewIDs(target providerauth.Target, ids ...string) error {
	for _, id := range ids {
		if err := (providerauth.LogoutRequest{OperationID: id, Target: target}).Validate(); err != nil {
			return err
		}
	}
	return nil
}

func (r clientAuthenticationReviewRequest) validate() error {
	if err := validateAuthenticationReviewIDs(r.OriginalTarget, r.OperationID, r.ReviewID); err != nil {
		return err
	}
	if r.ReviewSequence == 0 {
		return errors.New("authentication review sequence is required")
	}
	switch r.Choice.Kind {
	case "", "saved-logout":
		if r.Choice.AccountID != "" {
			return errors.New("authentication review choice has an unexpected account")
		}
	case "saved-account":
		return (providerauth.SwitchRequest{OperationID: r.ReviewID, Target: r.OriginalTarget, AccountID: r.Choice.AccountID}).Validate()
	default:
		return errors.New("authentication review choice is invalid")
	}
	return nil
}

func (w *ClientWorkspace) lockAuthenticationReview(ctx context.Context, operation string, target providerauth.Target) (*clientAuthority, *clientAuthenticationReceipt, error) {
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return nil, nil, err
	}
	a := w.authority
	w.mu.Unlock()
	if a == nil || a.store == nil || w.client == nil {
		return nil, nil, providerauth.ErrStale
	}
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return nil, nil, err
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		a.mu.Unlock()
		return nil, nil, err
	}
	valid := w.authority == a && w.ws.ID == target.WorkspaceID && w.ws.Authority != nil && w.ws.Authority.Mode == "client" && w.ws.Authority.Principal == a.principal
	w.mu.Unlock()
	original := a.authenticationReceipts[operation]
	if !valid || original == nil {
		a.mu.Unlock()
		return nil, nil, providerauth.ErrStale
	}
	if original.request.target != target || original.principal != a.principal || original.owner.ProviderID == "" {
		a.mu.Unlock()
		return nil, nil, providerauth.ErrOperationConflict
	}
	return a, original, nil
}

func (w *ClientWorkspace) reviewClientAuthentication(ctx context.Context, request clientAuthenticationReviewRequest) (clientAuthenticationReviewSummary, error) {
	initial := clientAuthenticationReviewSummary{OperationID: request.OperationID, ReviewID: request.ReviewID, ReviewSequence: request.ReviewSequence, Choice: request.Choice}
	if err := request.validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	a, original, err := w.lockAuthenticationReview(ctx, request.OperationID, request.OriginalTarget)
	if err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	if review := a.authenticationReviews[request.ReviewID]; review != nil {
		if review.request != request {
			return initial, providerauth.ErrOperationConflict
		}
		return cloneAuthenticationReviewSummary(review.summary), review.err
	}
	if original.reconciledBy != "" || original.acknowledged && original.adopted || request.ReviewSequence <= original.reviewSequence {
		return initial, providerauth.ErrStale
	}
	original.reviewSequence = request.ReviewSequence
	review := &clientAuthenticationReviewReceipt{request: request, owner: original.owner, summary: initial}
	a.retainAuthenticationReview(review)
	review.summary.Owner = providerauth.PublicOwner(original.owner)
	review.summary.OriginalProgress = original.outcome.Progress
	review.summary.OriginalLogout, review.summary.OriginalAccountID = original.request.logout, original.request.accountID
	fail := func(err error) (clientAuthenticationReviewSummary, error) {
		review.err = err
		return cloneAuthenticationReviewSummary(review.summary), err
	}
	// Checked-key receipts have a distinct effect and no account selection.
	// Until that effect has a validator, never reinterpret one as a switch or
	// permit an alternate choice to replace its original admitted authority.
	if !original.request.logout && original.request.accountID == "" {
		return fail(errors.New("saved API-key authentication reconciliation is not supported"))
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return fail(err)
	}
	if w.authority != a || w.ws.ID != request.OriginalTarget.WorkspaceID || w.ws.Authority == nil {
		w.mu.Unlock()
		return fail(providerauth.ErrStale)
	}
	review.cache = cloneAuthenticationReviewAuthority(*w.ws.Authority)
	w.mu.Unlock()
	review.accepted = config.RemoteAuthority{Mode: "client", Principal: a.principal, Revision: a.accepted.Revision, Digest: a.accepted.Digest}
	review.priorRemoved = maps.Clone(a.removed)
	remote, err := w.client.GetWorkspace(ctx, request.OriginalTarget.WorkspaceID)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication review cannot verify receiver authority", err))
	}
	if remote.ID != request.OriginalTarget.WorkspaceID || remote.Authority == nil || remote.Authority.Mode != "client" || remote.Authority.Principal != a.principal || remote.Authority.Revision == 0 || remote.Authority.Revision == ^uint64(0) {
		return fail(providerauth.ErrStale)
	}
	digest, err := hex.DecodeString(remote.Authority.Digest)
	if err != nil || len(digest) != 32 {
		return fail(providerauth.ErrStale)
	}
	review.base = cloneAuthenticationReviewAuthority(*remote.Authority)
	review.summary.Receiver = cloneAuthenticationReviewAuthority(review.base)
	current, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication review cannot capture saved state", err))
	}
	effect := config.AuthenticationReconciliationEffect{Logout: original.request.logout, AccountID: original.request.accountID}
	switch request.Choice.Kind {
	case "saved-account":
		effect = config.AuthenticationReconciliationEffect{AccountID: request.Choice.AccountID}
	case "saved-logout":
		effect = config.AuthenticationReconciliationEffect{Logout: true}
	}
	prepared, err := a.store.PrepareAuthenticationReconciliation(ctx, current, original.owner, effect)
	if err != nil {
		return fail(err)
	}
	var valid bool
	review.capture, valid = prepared.AuthenticationCapture()
	if !valid {
		return fail(providerauth.ErrReceiptUnverified)
	}
	review.removed = maps.Clone(a.removed)
	if review.removed == nil {
		review.removed = map[providerregistry.RegistrationOwner]bool{}
	}
	if effect.Logout {
		review.removed[original.owner] = true
	} else {
		delete(review.removed, original.owner)
	}
	proposal, err := a.store.CollectRemoteRuntimeForAuthentication(ctx, review.capture, review.base.Revision+1, review.removed)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication review exact collection failed", err))
	}
	review.proposal = &proposal
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return fail(err)
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return fail(err)
	}
	for _, provider := range current.Providers() {
		if provider.Owner == original.owner {
			review.summary.ActiveAccountID = provider.ActiveAccountID
			review.summary.Configured, review.summary.Disabled = provider.Configured, provider.Disabled
		}
	}
	for _, kind := range []config.SelectedModelType{config.SelectedModelTypeLarge, config.SelectedModelTypeSmall} {
		if model, ok := proposal.Models[kind]; ok {
			review.summary.Models = append(review.summary.Models, clientAuthenticationReviewModel{string(kind), model.Provider, model.Model})
		}
	}
	review.summary.ChangedCategories, err = authenticationReviewChangedCategories(a.accepted, proposal)
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	var id [16]byte
	_, _ = rand.Read(id[:])
	review.summary.PreviewID = hex.EncodeToString(id[:])
	return cloneAuthenticationReviewSummary(review.summary), nil
}

func authenticationReviewChangedCategories(before, after config.RemoteRuntimeProposal) ([]string, error) {
	var changed []string
	for _, comparison := range []struct {
		label       string
		left, right any
	}{
		{"runtime format", before.Version, after.Version},
		{"selected models", before.Models, after.Models},
		{"execution controls", before.Controls, after.Controls},
		{"provider settings", before.Providers, after.Providers},
		{"image settings", before.Images, after.Images},
		{"authentication", before.Credentials, after.Credentials},
		{"provider instructions", before.ProviderContextInstructions, after.ProviderContextInstructions},
		{"credential environment", before.CredentialEnvironment, after.CredentialEnvironment},
		{"provider bundles", before.Bundles, after.Bundles},
	} {
		left, le := json.Marshal(comparison.left)
		right, re := json.Marshal(comparison.right)
		if le != nil || re != nil {
			return nil, providerauth.ErrReceiptUnverified
		}
		// Conservative comparison preserves execution-significant RawMessage
		// numeric spelling. Display only fixed categories, never payload values.
		if !bytes.Equal(left, right) {
			changed = append(changed, comparison.label)
		}
	}
	return changed, nil
}

func (a *clientAuthority) retainAuthenticationReview(review *clientAuthenticationReviewReceipt) {
	if a.authenticationReviews == nil {
		a.authenticationReviews = map[string]*clientAuthenticationReviewReceipt{}
	}
	if len(a.authenticationReviewIDs) == clientAuthenticationReceiptLimit {
		delete(a.authenticationReviews, a.authenticationReviewIDs[0])
		a.authenticationReviewIDs = a.authenticationReviewIDs[1:]
	}
	a.authenticationReviewIDs = append(a.authenticationReviewIDs, review.request.ReviewID)
	a.authenticationReviews[review.request.ReviewID] = review
}

// Attempted reviewed publications are acknowledged only by their apply ledger.
// Generic pending adoption and phase-one recovery must not turn this distinct
// action into an original transaction's successful receipt.
func (a *clientAuthority) pendingAuthenticationReview(id string) bool {
	for _, original := range a.authenticationReceipts {
		if original.request.target.WorkspaceID == id && original.pendingReview != "" && original.reconciledBy == "" {
			return true
		}
	}
	return false
}

func (w *ClientWorkspace) lockAuthenticationReviewWorkspace(ctx context.Context) error {
	for !w.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	if err := ctx.Err(); err != nil {
		w.mu.Unlock()
		return err
	}
	return nil
}

func cloneAuthenticationReviewAuthority(value config.RemoteAuthority) config.RemoteAuthority {
	value.Accounts = slices.Clone(value.Accounts)
	return value
}
func cloneAuthenticationReviewSummary(value clientAuthenticationReviewSummary) clientAuthenticationReviewSummary {
	value.Models = slices.Clone(value.Models)
	value.ChangedCategories = slices.Clone(value.ChangedCategories)
	value.Receiver = cloneAuthenticationReviewAuthority(value.Receiver)
	return value
}
func authenticationReviewAuthorityEqual(left, right config.RemoteAuthority) bool {
	return left.Mode == right.Mode && left.Principal == right.Principal && left.Revision == right.Revision && left.Digest == right.Digest && slices.Equal(left.Accounts, right.Accounts)
}

func (w *ClientWorkspace) verifyAuthenticationReviewCache(ctx context.Context, a *clientAuthority, review *clientAuthenticationReviewReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return err
	}
	defer w.mu.Unlock()
	if w.authority != a || w.ws.ID != review.request.OriginalTarget.WorkspaceID || w.ws.Authority == nil || !authenticationReviewAuthorityEqual(*w.ws.Authority, review.cache) || !matchesAuthority(&review.accepted, a.principal, a.accepted) || !maps.Equal(a.removed, review.priorRemoved) {
		return errors.New("authentication review cached authority changed; review saved state again")
	}
	return nil
}

func (w *ClientWorkspace) verifyAuthenticationReviewCapture(ctx context.Context, a *clientAuthority, review *clientAuthenticationReviewReceipt) error {
	current, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return clientAuthenticationFailure("authentication review cannot verify saved state", err)
	}
	if !review.capture.SameObservation(current) {
		return errors.New("authentication review saved state changed; review saved state again")
	}
	return ctx.Err()
}

func (w *ClientWorkspace) applyClientAuthenticationReview(ctx context.Context, request clientAuthenticationApplyRequest) (clientAuthenticationReconciliationOutcome, error) {
	initial := clientAuthenticationReconciliationOutcome{OperationID: request.OperationID, ReviewID: request.ReviewID, PreviewID: request.PreviewID, ApplyID: request.ApplyID, OriginalDisposition: "unresolved"}
	if err := validateAuthenticationReviewIDs(request.OriginalTarget, request.OperationID, request.ReviewID, request.PreviewID, request.ApplyID); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	a, original, err := w.lockAuthenticationReview(ctx, request.OperationID, request.OriginalTarget)
	if err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	review := a.authenticationReviews[request.ReviewID]
	if review == nil || review.request.OperationID != request.OperationID || review.request.OriginalTarget != request.OriginalTarget || review.summary.PreviewID != request.PreviewID || review.err != nil || review.proposal == nil {
		return initial, providerauth.ErrStale
	}
	for _, other := range a.authenticationReviews {
		if other.apply != nil && other.apply.request.ApplyID == request.ApplyID && other.apply.request != request {
			return initial, providerauth.ErrOperationConflict
		}
	}
	if review.apply != nil {
		if review.apply.request != request {
			return initial, providerauth.ErrOperationConflict
		}
		return w.replayAuthenticationReviewApply(ctx, a, original, review)
	}
	apply := &clientAuthenticationApplyReceipt{request: request, outcome: initial}
	review.apply = apply
	fail := func(err error) (clientAuthenticationReconciliationOutcome, error) {
		apply.err = err
		return apply.outcome, err
	}
	if original.reconciledBy != "" || original.reviewSequence != review.request.ReviewSequence {
		return fail(providerauth.ErrStale)
	}
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return fail(err)
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return fail(err)
	}
	remote, err := w.client.GetWorkspace(ctx, request.OriginalTarget.WorkspaceID)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication apply cannot verify receiver authority", err))
	}
	if remote.ID != request.OriginalTarget.WorkspaceID || remote.Authority == nil || !authenticationReviewAuthorityEqual(*remote.Authority, review.base) {
		return fail(errors.New("authentication review receiver changed; review saved state again"))
	}
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return fail(err)
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return fail(err)
	}
	// This preview permits one attempt. Transport retries only inspect the
	// receiver for the retained exact proposal; they never re-collect or PUT.
	apply.put = true
	original.pendingReview = review.summary.PreviewID
	a.pending, a.pendingView = review.proposal, review.proposal.CollectionConfig()
	ack, err := w.client.ReplaceRemoteRuntime(ctx, request.OriginalTarget.WorkspaceID, review.base.Revision, *review.proposal)
	if err == nil && matchesAuthority(ack, a.principal, *review.proposal) {
		return w.completeAuthenticationReviewApply(ctx, a, original, review, ack)
	}
	return w.replayAuthenticationReviewApply(ctx, a, original, review)
}

func (w *ClientWorkspace) replayAuthenticationReviewApply(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, review *clientAuthenticationReviewReceipt) (clientAuthenticationReconciliationOutcome, error) {
	apply := review.apply
	if apply.outcome.Adopted {
		return apply.outcome, ctx.Err()
	}
	if !apply.put {
		return apply.outcome, apply.err
	}
	if original.reconciledBy != "" || original.reviewSequence != review.request.ReviewSequence {
		return apply.outcome, providerauth.ErrStale
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return apply.outcome, err
	}
	remote, err := w.client.GetWorkspace(ctx, review.request.OriginalTarget.WorkspaceID)
	if err != nil {
		return apply.outcome, clientAuthenticationFailure("authentication apply acknowledgement cannot be verified", err)
	}
	if remote.ID != review.request.OriginalTarget.WorkspaceID || !matchesAuthority(remote.Authority, a.principal, *review.proposal) {
		return apply.outcome, errors.New("receiver has not acknowledged the exact reviewed authentication proposal")
	}
	return w.completeAuthenticationReviewApply(ctx, a, original, review, remote.Authority)
}

func (w *ClientWorkspace) completeAuthenticationReviewApply(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, review *clientAuthenticationReviewReceipt, ack *config.RemoteAuthority) (clientAuthenticationReconciliationOutcome, error) {
	apply := review.apply
	if !matchesAuthority(ack, a.principal, *review.proposal) {
		return apply.outcome, providerauth.ErrReceiptUnverified
	}
	apply.outcome.RemoteAcknowledged = true
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return apply.outcome, err
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return apply.outcome, err
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return apply.outcome, err
	}
	defer w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return apply.outcome, err
	}
	if w.authority != a || w.ws.ID != review.request.OriginalTarget.WorkspaceID || w.ws.Authority == nil || !authenticationReviewAuthorityEqual(*w.ws.Authority, review.cache) || !matchesAuthority(&review.accepted, a.principal, a.accepted) || !maps.Equal(a.removed, review.priorRemoved) || original.reviewSequence != review.request.ReviewSequence || original.reconciledBy != "" {
		return apply.outcome, providerauth.ErrStale
	}
	a.accepted = *review.proposal
	a.view.Store(review.proposal.CollectionConfig())
	a.removed = maps.Clone(review.removed)
	if a.pending != nil && a.pending.Revision == review.proposal.Revision && a.pending.Digest == review.proposal.Digest {
		a.pending, a.pendingView = nil, nil
	}
	w.ws.Authority = new(config.RemoteAuthority)
	*w.ws.Authority = cloneAuthenticationReviewAuthority(*ack)
	w.appliedRefresh = w.refreshSequence.Add(1)
	w.providerAuthAppliedRead = w.providerAuthReadSequence.Add(1)
	apply.outcome.Adopted = true
	apply.outcome.OriginalDisposition = "runtime-reconciled"
	original.reconciledBy = review.summary.PreviewID
	return apply.outcome, nil
}
