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
	"strings"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
)

// Review and apply are separate explicit actions. They publish already coherent
// saved state, never repeat or repair the original authentication transaction.
type clientAuthenticationReviewChoice struct {
	Kind         string // empty: original intent; saved-account, saved-logout, saved-oauth-token: explicit choice
	AccountID    string
	CredentialID string
}

type clientAuthenticationReviewRequest struct {
	OperationID    string
	OriginalTarget providerauth.Target
	FreshSaved     bool
	SavedTarget    providerauth.Target
	ReviewID       string
	ReviewSequence uint64
	Choice         clientAuthenticationReviewChoice
}

type clientAuthenticationApplyRequest struct {
	OperationID    string
	OriginalTarget providerauth.Target
	FreshSaved     bool
	SavedTarget    providerauth.Target
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
	OriginalLoginID                  string
	FreshSaved                       bool
	OriginalOAuthToken               bool
	SavedOAuthToken                  bool
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
	abandon                    *ProviderAuthenticationReviewAbandonRequest
	supersededByReview         string
	originalAbandonedBy        string
	journalRevision            uint64
	journalCompleted, restored bool
	observation                string
	request                    clientAuthenticationReviewRequest
	summary                    clientAuthenticationReviewSummary
	owner                      providerregistry.RegistrationOwner
	generation                 providerauth.Generation
	capture                    config.AuthenticationCapture
	base                       config.RemoteAuthority
	cache                      config.RemoteAuthority
	accepted                   config.RemoteAuthority
	priorRemoved, removed      map[providerregistry.RegistrationOwner]bool
	proposal                   *config.RemoteRuntimeProposal
	apply                      *clientAuthenticationApplyReceipt
	err                        error
	savedStateSupersededBy     string
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

func (r clientAuthenticationReviewRequest) target() providerauth.Target {
	if r.FreshSaved {
		return r.SavedTarget
	}
	return r.OriginalTarget
}

func (r clientAuthenticationApplyRequest) target() providerauth.Target {
	if r.FreshSaved {
		return r.SavedTarget
	}
	return r.OriginalTarget
}

func validateAuthenticationReviewMode(fresh bool, operation string, original, saved providerauth.Target, ids ...string) error {
	if fresh {
		if operation != "" || original != (providerauth.Target{}) {
			return errors.New("fresh saved intent must not name an original operation")
		}
		return validateAuthenticationReviewIDs(saved, ids...)
	}
	if saved != (providerauth.Target{}) {
		return errors.New("original review has an unexpected saved target")
	}
	return validateAuthenticationReviewIDs(original, append([]string{operation}, ids...)...)
}

func (r clientAuthenticationApplyRequest) validate() error {
	return validateAuthenticationReviewMode(r.FreshSaved, r.OperationID, r.OriginalTarget, r.SavedTarget, r.ReviewID, r.PreviewID, r.ApplyID)
}

func (r clientAuthenticationReviewRequest) validate() error {
	if err := validateAuthenticationReviewMode(r.FreshSaved, r.OperationID, r.OriginalTarget, r.SavedTarget, r.ReviewID); err != nil {
		return err
	}
	if r.ReviewSequence == 0 {
		return errors.New("authentication review sequence is required")
	}
	if r.FreshSaved && r.Choice.Kind == "" {
		return errors.New("fresh saved intent requires an explicit choice")
	}
	if r.Choice.Kind != "saved-credential" && r.Choice.CredentialID != "" {
		return errors.New("unexpected authentication credential slot")
	}
	switch r.Choice.Kind {
	case "", "saved-logout", "saved-oauth-token":
		if r.Choice.AccountID != "" {
			return errors.New("authentication review choice has an unexpected account")
		}
		if r.Choice.Kind == "saved-oauth-token" && !r.target().Owner.HasOAuth {
			return errors.New("saved OAuth credential review requires an OAuth owner")
		}
	case "saved-account":
		return (providerauth.SwitchRequest{OperationID: r.ReviewID, Target: r.target(), AccountID: r.Choice.AccountID}).Validate()
	case "saved-credential":
		if r.Choice.AccountID != "" || strings.TrimSpace(r.Choice.CredentialID) == "" || len(r.Choice.CredentialID) > 128 || !utf8.ValidString(r.Choice.CredentialID) || strings.ContainsAny(r.Choice.CredentialID, "\x00\r\n") {
			return errors.New("invalid saved credential slot choice")
		}
	default:
		return errors.New("authentication review choice is invalid")
	}
	return nil
}

func (a *clientAuthority) freshAuthenticationReviewCurrent(review *clientAuthenticationReviewReceipt) bool {
	for _, other := range a.authenticationReviews {
		if other.request.FreshSaved && other.request.SavedTarget == review.request.SavedTarget && other.request.ReviewSequence > review.request.ReviewSequence {
			return false
		}
	}
	return true
}

func authenticationReviewOriginalCurrent(a *clientAuthority, original *clientAuthenticationReceipt, review *clientAuthenticationReviewReceipt) bool {
	if review.request.FreshSaved {
		return original == nil && review.abandon == nil && review.supersededByReview == "" && review.originalAbandonedBy == "" && review.savedStateSupersededBy == "" && a.freshAuthenticationReviewCurrent(review)
	}
	return original != nil && original.abandon == nil && review.abandon == nil && review.supersededByReview == "" && review.originalAbandonedBy == "" && original.savedStateSupersededBy == "" && original.reconciledBy == "" && original.reviewSequence == review.request.ReviewSequence
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
	if valid {
		if err := a.loadAuthenticationJournal(ctx, target.WorkspaceID); err != nil {
			a.mu.Unlock()
			return nil, nil, err
		}
	}
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

func (w *ClientWorkspace) reviewClientAuthentication(ctx context.Context, request clientAuthenticationReviewRequest) (summary clientAuthenticationReviewSummary, failure error) {
	initial := clientAuthenticationReviewSummary{FreshSaved: request.FreshSaved, OperationID: request.OperationID, ReviewID: request.ReviewID, ReviewSequence: request.ReviewSequence, Choice: request.Choice}
	if err := request.validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	var a *clientAuthority
	var original *clientAuthenticationReceipt
	var err error
	if request.FreshSaved {
		a, err = w.lockSavedAuthentication(ctx, request.SavedTarget.WorkspaceID)
	} else {
		a, original, err = w.lockAuthenticationReview(ctx, request.OperationID, request.OriginalTarget)
	}
	if err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	release, leaseErr := a.acquireAuthenticationPublication(ctx, request.target().WorkspaceID)
	if leaseErr != nil {
		return initial, leaseErr
	}
	defer release()
	if err := a.loadAuthenticationJournal(ctx, request.target().WorkspaceID); err != nil {
		return initial, err
	}
	if !request.FreshSaved {
		original = a.authenticationReceipts[request.OperationID]
	}
	defer func() {
		if err := a.finishAuthenticationJournal(ctx); err != nil {
			failure = errors.Join(failure, err)
		}
	}()
	if review := a.authenticationReviews[request.ReviewID]; review != nil {
		if review.request != request {
			return initial, providerauth.ErrOperationConflict
		}
		return cloneAuthenticationReviewSummary(review.summary), review.err
	}
	var owner providerregistry.RegistrationOwner
	var freshCapture config.AuthenticationCapture
	if request.FreshSaved {
		freshCapture, owner, err = a.providerAuth.CaptureSavedAuthentication(ctx, request.SavedTarget)
		if err != nil {
			return initial, err
		}
		for _, other := range a.authenticationReviews {
			if other.request.FreshSaved && other.request.SavedTarget == request.SavedTarget && other.request.ReviewSequence >= request.ReviewSequence {
				return initial, providerauth.ErrStale
			}
		}
	} else {
		if original.abandon != nil || original.savedStateSupersededBy != "" || original.reconciledBy != "" || original.acknowledged && original.adopted || request.ReviewSequence <= original.reviewSequence {
			return initial, providerauth.ErrStale
		}
		original.reviewSequence = request.ReviewSequence
		owner = original.owner
	}
	// Preserve unresolved attempted publications when the bounded ledger fills.
	if len(a.authenticationReviewIDs) >= clientAuthenticationReceiptLimit {
		oldest := a.authenticationReviews[a.authenticationReviewIDs[0]]
		if oldest != nil && !oldest.journalCompleted && oldest.originalAbandonedBy == "" && oldest.apply != nil && oldest.apply.put && !oldest.apply.outcome.Adopted && oldest.savedStateSupersededBy == "" {
			return initial, errors.New("an unresolved reviewed publication must be reconciled before another review")
		}
	}
	review := &clientAuthenticationReviewReceipt{request: request, owner: owner, summary: initial}
	a.retainAuthenticationReview(review)
	for _, previous := range a.authenticationReviews {
		if previous == review || previous.owner != owner || previous.request.target() != request.target() || previous.request.ReviewSequence >= request.ReviewSequence || previous.request.FreshSaved != request.FreshSaved || previous.request.OperationID != request.OperationID || previous.journalCompleted {
			continue
		}
		if previous.apply == nil || !previous.apply.put {
			previous.supersededByReview = request.ReviewID
		}
	}
	review.summary.Owner = providerauth.PublicOwner(owner)
	if original != nil {
		review.summary.OriginalProgress = original.outcome.Progress
		review.summary.OriginalLogout, review.summary.OriginalAccountID = original.request.logout, original.request.accountID
		review.summary.OriginalLoginID = original.request.loginID
		review.summary.OriginalOAuthToken = original.oauthTokenID != ""
		if original.removalAdmitted {
			review.summary.OriginalLogout = original.removalSuccessor == ""
			review.summary.OriginalAccountID = original.removalSuccessor
		}
		if original.request.loginID != "" && original.outcome.Change != nil {
			review.summary.OriginalAccountID = original.outcome.Change.Current.Status.ActiveAccountID
		}
	}
	fail := func(err error) (clientAuthenticationReviewSummary, error) {
		review.err = err
		return cloneAuthenticationReviewSummary(review.summary), err
	}
	if original != nil && original.request.checkID != "" && request.Choice.Kind == "" && original.credentialEffectID == "" {
		return fail(errors.New("original checked credential proof is unavailable; select a separate saved credential explicitly"))
	}
	if original != nil && original.request.loginID != "" && review.summary.OriginalAccountID == "" && original.oauthTokenID == "" && request.Choice.Kind == "" {
		return fail(errors.New("saved OAuth login has no confirmed credential selection for review; use exact completed-capture recovery or a separate explicit saved-state choice"))
	}
	if err := w.lockAuthenticationReviewWorkspace(ctx); err != nil {
		return fail(err)
	}
	if w.authority != a || w.ws.ID != request.target().WorkspaceID || w.ws.Authority == nil {
		w.mu.Unlock()
		return fail(providerauth.ErrStale)
	}
	review.cache = cloneAuthenticationReviewAuthority(*w.ws.Authority)
	w.mu.Unlock()
	review.accepted = config.RemoteAuthority{Mode: "client", Principal: a.principal, Revision: a.accepted.Revision, Digest: a.accepted.Digest}
	review.priorRemoved = maps.Clone(a.removed)
	remote, err := w.client.PeerWorkspaceAuthority(ctx, request.target().WorkspaceID, a.principal)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication review cannot verify receiver authority through peer channel", err))
	}
	if remote.Mode != "client" || remote.Principal != a.principal || remote.Revision == 0 || remote.Revision == ^uint64(0) {
		return fail(providerauth.ErrStale)
	}
	digest, err := hex.DecodeString(remote.Digest)
	if err != nil || len(digest) != 32 {
		return fail(providerauth.ErrStale)
	}
	review.base = cloneAuthenticationReviewAuthority(*remote)
	review.summary.Receiver = cloneAuthenticationReviewAuthority(review.base)
	current := freshCapture
	if !request.FreshSaved {
		if original != nil && original.outcome.Change == nil {
			status, statusErr := a.providerAuth.Status(ctx)
			if statusErr != nil {
				err = statusErr
			} else {
				currentTarget := providerauth.Target{WorkspaceID: status.WorkspaceID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation}
				ownerPresent := false
				for _, provider := range status.Providers {
					if provider.Owner == currentTarget.Owner {
						ownerPresent = true
						break
					}
				}
				if !ownerPresent {
					err = providerauth.ErrOwner
				} else {
					var capturedOwner providerregistry.RegistrationOwner
					current, capturedOwner, err = a.providerAuth.CaptureSavedAuthentication(ctx, currentTarget)
					if err == nil && capturedOwner != owner {
						err = providerauth.ErrOwner
					}
					review.generation = currentTarget.Generation
				}
			}
		} else {
			current, err = a.store.CaptureAuthentication(ctx)
		}
	}
	if err != nil {
		return fail(clientAuthenticationFailure("authentication review cannot capture saved state", err))
	}
	if original != nil && original.removalAdmitted && request.Choice.Kind == "" {
		saved, err := current.Accounts(original.owner)
		if err != nil {
			return fail(err)
		}
		for _, account := range saved {
			if account.ID == original.request.removedAccountID {
				return fail(errors.New("the removed account is present again; review a separate explicit saved-state choice"))
			}
		}
	}
	effect := config.AuthenticationReconciliationEffect{Logout: review.summary.OriginalLogout, AccountID: review.summary.OriginalAccountID}
	if original != nil {
		effect.OAuthTokenID = original.oauthTokenID
		if original.request.checkID != "" {
			effect = config.AuthenticationReconciliationEffect{CredentialID: original.outcome.CredentialID, CredentialEffectID: original.credentialEffectID}
		}
	}
	switch request.Choice.Kind {
	case "saved-account":
		effect = config.AuthenticationReconciliationEffect{AccountID: request.Choice.AccountID}
	case "saved-logout":
		effect = config.AuthenticationReconciliationEffect{Logout: true}
	case "saved-credential":
		identity, err := current.ConfiguredCredentialEffectID(owner, request.Choice.CredentialID)
		if err != nil {
			return fail(err)
		}
		effect = config.AuthenticationReconciliationEffect{CredentialID: request.Choice.CredentialID, CredentialEffectID: identity}
	case "saved-oauth-token":
		identity, err := current.ConfiguredOAuthTokenCredentialID(owner)
		if err != nil {
			return fail(err)
		}
		effect = config.AuthenticationReconciliationEffect{OAuthTokenID: identity}
	}
	prepared, err := a.store.PrepareAuthenticationReconciliation(ctx, current, owner, effect)
	if err != nil {
		return fail(err)
	}
	review.summary.SavedOAuthToken = effect.OAuthTokenID != ""
	var valid bool
	review.capture, valid = prepared.AuthenticationCapture()
	if !valid {
		return fail(providerauth.ErrReceiptUnverified)
	}
	if request.FreshSaved {
		review.generation = request.SavedTarget.Generation
	} else if original != nil && original.outcome.Change != nil {
		if original.outcome.Change.Current.Target.Owner != providerauth.PublicOwner(owner) {
			return fail(providerauth.ErrReceiptUnverified)
		}
		review.generation = original.outcome.Change.Current.Target.Generation
	} else if review.generation.Validate() != nil {
		return fail(providerauth.ErrReceiptUnverified)
	}
	review.removed = maps.Clone(a.removed)
	if review.removed == nil {
		review.removed = map[providerregistry.RegistrationOwner]bool{}
	}
	if effect.Logout {
		review.removed[owner] = true
	} else {
		delete(review.removed, owner)
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
		if provider.Owner == owner {
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
	if a.authenticationReviews[review.request.ReviewID] != nil {
		a.authenticationReviews[review.request.ReviewID] = review
		return
	}
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
	for _, review := range a.authenticationReviews {
		if review.abandon == nil && review.request.FreshSaved && review.request.target().WorkspaceID == id && review.apply != nil && review.apply.put && !review.apply.outcome.Adopted && review.savedStateSupersededBy == "" {
			return true
		}
	}
	for _, original := range a.authenticationReceipts {
		if original.abandon == nil && original.request.target.WorkspaceID == id && original.pendingReview != "" && original.reconciledBy == "" && original.savedStateSupersededBy == "" {
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
	if review.restored {
		if w.authority != a || w.ws.ID != review.request.target().WorkspaceID || w.ws.Authority == nil || w.ws.Authority.Principal != a.principal || !matchesAuthority(w.ws.Authority, a.principal, a.accepted) {
			return errors.New("restored authentication review authority changed")
		}
		return nil
	}
	if w.authority != a || w.ws.ID != review.request.target().WorkspaceID || w.ws.Authority == nil || !authenticationReviewAuthorityEqual(*w.ws.Authority, review.cache) || !matchesAuthority(&review.accepted, a.principal, a.accepted) || !maps.Equal(a.removed, review.priorRemoved) {
		return errors.New("authentication review cached authority changed; review saved state again")
	}
	return nil
}

func (w *ClientWorkspace) verifyAuthenticationReviewCapture(ctx context.Context, a *clientAuthority, review *clientAuthenticationReviewReceipt) error {
	if err := w.restoreAuthenticationReview(ctx, a, review); err != nil {
		return err
	}
	current, err := a.store.CaptureAuthentication(ctx)
	if err != nil {
		return clientAuthenticationFailure("authentication review cannot verify saved state", err)
	}
	if !review.capture.SameObservation(current) {
		return errors.New("authentication review saved state changed; review saved state again")
	}
	return ctx.Err()
}

func (w *ClientWorkspace) applyClientAuthenticationReview(ctx context.Context, request clientAuthenticationApplyRequest) (outcome clientAuthenticationReconciliationOutcome, failure error) {
	initial := clientAuthenticationReconciliationOutcome{OperationID: request.OperationID, ReviewID: request.ReviewID, PreviewID: request.PreviewID, ApplyID: request.ApplyID, OriginalDisposition: "unresolved"}
	if request.FreshSaved {
		initial.OriginalDisposition = "not-applicable"
	}
	if err := request.validate(); err != nil {
		return initial, err
	}
	ctx, done := providerAuthContext(ctx, w.subCtx)
	defer done()
	var a *clientAuthority
	var original *clientAuthenticationReceipt
	var err error
	if request.FreshSaved {
		a, err = w.lockSavedAuthentication(ctx, request.SavedTarget.WorkspaceID)
	} else {
		a, original, err = w.lockAuthenticationReview(ctx, request.OperationID, request.OriginalTarget)
	}
	if err != nil {
		return initial, err
	}
	defer a.mu.Unlock()
	release, leaseErr := a.acquireAuthenticationPublication(ctx, request.target().WorkspaceID)
	if leaseErr != nil {
		return initial, leaseErr
	}
	defer release()
	if err := a.loadAuthenticationJournal(ctx, request.target().WorkspaceID); err != nil {
		return initial, err
	}
	if !request.FreshSaved {
		original = a.authenticationReceipts[request.OperationID]
	}
	defer func() {
		if err := a.finishAuthenticationJournal(ctx); err != nil {
			failure = errors.Join(failure, err)
		}
	}()
	review := a.authenticationReviews[request.ReviewID]
	if review == nil || review.request.OperationID != request.OperationID || review.request.OriginalTarget != request.OriginalTarget || review.request.FreshSaved != request.FreshSaved || review.request.SavedTarget != request.SavedTarget || review.summary.PreviewID != request.PreviewID || review.err != nil || review.proposal == nil || review.abandon != nil || review.supersededByReview != "" || review.originalAbandonedBy != "" {
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
	if !authenticationReviewOriginalCurrent(a, original, review) {
		return fail(providerauth.ErrStale)
	}
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return fail(err)
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return fail(err)
	}
	remote, err := w.client.PeerWorkspaceAuthority(ctx, request.target().WorkspaceID, a.principal)
	if err != nil {
		return fail(clientAuthenticationFailure("authentication apply cannot verify receiver authority through peer channel", err))
	}
	if !authenticationReviewAuthorityEqual(*remote, review.base) {
		return fail(errors.New("authentication review receiver changed; review saved state again"))
	}
	if err := w.verifyAuthenticationReviewCapture(ctx, a, review); err != nil {
		return fail(err)
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return fail(err)
	}
	authentication, err := clientAuthenticationPeerStatesFromCapture(*review.proposal, review.capture, review.generation)
	if err != nil {
		return fail(err)
	}
	apply.put = true
	if err := a.persistAuthenticationReview(ctx, review); err != nil {
		apply.put = false
		return fail(err)
	}
	if original != nil {
		original.pendingReview = review.summary.PreviewID
		if err := a.persistAuthenticationReceipt(ctx, original); err != nil {
			return fail(err)
		}
	}
	base := a.accepted
	if !matchesAuthority(&review.base, a.principal, base) {
		if a.pending == nil || !matchesAuthority(&review.base, a.principal, *a.pending) {
			return fail(errors.New("authentication review has no retained receiver runtime base"))
		}
		base = *a.pending
	}
	a.pending, a.pendingView = review.proposal, review.proposal.CollectionConfig()
	ack, err := w.client.PatchRemoteRuntime(ctx, request.target().WorkspaceID, base, *review.proposal, authentication...)
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
	if !authenticationReviewOriginalCurrent(a, original, review) {
		return apply.outcome, providerauth.ErrStale
	}
	if err := w.verifyAuthenticationReviewCache(ctx, a, review); err != nil {
		return apply.outcome, err
	}
	remote, err := w.client.PeerWorkspaceAuthority(ctx, review.request.target().WorkspaceID, a.principal)
	if err != nil {
		return apply.outcome, clientAuthenticationFailure("authentication apply acknowledgement cannot be verified through peer channel", err)
	}
	if !matchesAuthority(remote, a.principal, *review.proposal) {
		return apply.outcome, errors.New("receiver has not acknowledged the exact reviewed authentication proposal")
	}
	return w.completeAuthenticationReviewApply(ctx, a, original, review, remote)
}

func (w *ClientWorkspace) completeAuthenticationReviewApply(ctx context.Context, a *clientAuthority, original *clientAuthenticationReceipt, review *clientAuthenticationReviewReceipt, ack *config.RemoteAuthority) (clientAuthenticationReconciliationOutcome, error) {
	apply := review.apply
	if !matchesAuthority(ack, a.principal, *review.proposal) {
		return apply.outcome, providerauth.ErrReceiptUnverified
	}
	apply.outcome.RemoteAcknowledged = true
	if review.restored {
		return w.adoptRestoredAuthenticationReview(ctx, a, original, review, ack)
	}
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
	if w.authority != a || w.ws.ID != review.request.target().WorkspaceID || w.ws.Authority == nil || !authenticationReviewAuthorityEqual(*w.ws.Authority, review.cache) || !matchesAuthority(&review.accepted, a.principal, a.accepted) || !maps.Equal(a.removed, review.priorRemoved) || !authenticationReviewOriginalCurrent(a, original, review) {
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
	if review.request.FreshSaved {
		a.supersedeAuthenticationWithSavedReview(review)
	}
	if original != nil {
		apply.outcome.OriginalDisposition = "runtime-reconciled"
		original.reconciledBy = review.summary.PreviewID
	}
	return apply.outcome, nil
}

// Called only after exact fresh proposal acknowledgement and local adoption.
// This retires same-owner publication barriers, never rewrites an old outcome,
// acknowledgement or original-reconciliation receipt.
func (a *clientAuthority) supersedeAuthenticationWithSavedReview(review *clientAuthenticationReviewReceipt) {
	if !review.request.FreshSaved || review.apply == nil || !review.apply.outcome.Adopted {
		return
	}
	for _, receipt := range a.authenticationReceipts {
		if !receipt.journalCompleted && receipt.owner == review.owner && receipt.principal == a.principal && receipt.request.target.WorkspaceID == review.request.SavedTarget.WorkspaceID && receipt.savedStateSupersededBy == "" {
			receipt.savedStateSupersededBy = review.summary.PreviewID
		}
	}
	for _, previous := range a.authenticationReviews {
		if previous != review && !previous.journalCompleted && previous.originalAbandonedBy == "" && previous.owner == review.owner && previous.request.target().WorkspaceID == review.request.SavedTarget.WorkspaceID && previous.apply != nil && previous.apply.put && !previous.apply.outcome.Adopted {
			previous.savedStateSupersededBy = review.summary.PreviewID
		}
	}
}
