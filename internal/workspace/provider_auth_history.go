package workspace

import (
	"context"
	"errors"
	"sort"

	"github.com/example-git/crux/internal/providerauth"
)

// ProviderAuthenticationHistory is a redacted, owning-client history. It is not
// a current service generation, credential capture, or receiver acknowledgement.
type ProviderAuthenticationHistory struct {
	Operations []ProviderAuthenticationHistoryOperation
	Reviews    []ProviderAuthenticationHistoryReview
}
type ProviderAuthenticationHistoryOperation struct {
	OperationID                                string
	Target                                     providerauth.Target
	Outcome                                    providerauth.MutationOutcome
	LocalFinished, RemoteAcknowledged, Adopted bool
	RecoverySequence, ReviewSequence           uint64
	ReconciledBy, SavedStateSupersededBy       string
}
type ProviderAuthenticationHistoryReview struct {
	Request                ProviderAuthenticationReviewRequest
	Summary                ProviderAuthenticationReviewSummary
	ApplyRequest           *ProviderAuthenticationApplyRequest
	ApplyOutcome           *ProviderAuthenticationReconciliationOutcome
	SavedStateSupersededBy string
}
type ProviderAuthenticationHistorian interface {
	ProviderAuthenticationHistory(context.Context) (ProviderAuthenticationHistory, error)
}

func (w *ClientWorkspace) ProviderAuthenticationHistory(ctx context.Context) (ProviderAuthenticationHistory, error) {
	if !w.CanRecoverProviderAuthentication() {
		return ProviderAuthenticationHistory{}, errors.New("authentication publication history requires the owning client workspace")
	}
	ctx, cancel := providerAuthContext(ctx, w.subCtx)
	defer cancel()
	a := w.authority
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return ProviderAuthenticationHistory{}, err
	}
	defer a.mu.Unlock()
	id := w.workspaceID()
	if w.authority != a || !w.clientOwned() {
		return ProviderAuthenticationHistory{}, providerauth.ErrStale
	}
	if err := a.loadAuthenticationJournal(ctx, id); err != nil {
		return ProviderAuthenticationHistory{}, err
	}
	result := ProviderAuthenticationHistory{Operations: []ProviderAuthenticationHistoryOperation{}, Reviews: []ProviderAuthenticationHistoryReview{}}
	for _, receipt := range a.authenticationReceipts {
		if receipt.principal != a.principal || receipt.request.target.WorkspaceID != id {
			continue
		}
		outcome, err := clientAuthenticationOutcome(receipt, nil)
		if err != nil {
			return ProviderAuthenticationHistory{}, err
		}
		result.Operations = append(result.Operations, ProviderAuthenticationHistoryOperation{OperationID: receipt.request.operationID, Target: receipt.request.target, Outcome: outcome, LocalFinished: receipt.localFinished, RemoteAcknowledged: receipt.acknowledged, Adopted: receipt.adopted, RecoverySequence: receipt.recoverySequence, ReviewSequence: receipt.reviewSequence, ReconciledBy: receipt.reconciledBy, SavedStateSupersededBy: receipt.savedStateSupersededBy})
	}
	for _, review := range a.authenticationReviews {
		if review.request.target().WorkspaceID != id {
			continue
		}
		entry := ProviderAuthenticationHistoryReview{Request: ProviderAuthenticationReviewRequest(review.request), Summary: cloneAuthenticationReviewSummary(review.summary), SavedStateSupersededBy: review.savedStateSupersededBy}
		if review.apply != nil {
			request := ProviderAuthenticationApplyRequest(review.apply.request)
			outcome := review.apply.outcome
			entry.ApplyRequest, entry.ApplyOutcome = &request, &outcome
		}
		result.Reviews = append(result.Reviews, entry)
	}
	sort.Slice(result.Operations, func(i, j int) bool { return result.Operations[i].OperationID < result.Operations[j].OperationID })
	sort.Slice(result.Reviews, func(i, j int) bool { return result.Reviews[i].Request.ReviewID < result.Reviews[j].Request.ReviewID })
	return result, ctx.Err()
}
