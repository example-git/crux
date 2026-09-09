package workspace

import (
	"context"
	"errors"
	"sort"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
)

// ProviderAuthenticationHistory is a redacted, owning-client history. It is not
// a current service generation, credential capture, or receiver acknowledgement.
type ProviderAuthenticationHistory struct {
	Operations []ProviderAuthenticationHistoryOperation
	Reviews    []ProviderAuthenticationHistoryReview
}
type ProviderAuthenticationHistoryOperation struct {
	JournalRevision                            uint64
	Abandoned                                  bool
	AbandonRequest                             *ProviderAuthenticationAbandonRequest
	HistoricalWorkspace                        bool
	RecoveryRequest                            *ProviderAuthenticationRecoveryRequest
	OperationID                                string
	Target                                     providerauth.Target
	Outcome                                    providerauth.MutationOutcome
	LocalFinished, RemoteAcknowledged, Adopted bool
	RecoverySequence, ReviewSequence           uint64
	ReconciledBy, SavedStateSupersededBy       string
}
type ProviderAuthenticationHistoryReview struct {
	ApplyAttempted         bool
	JournalRevision        uint64
	Abandoned              bool
	AbandonRequest         *ProviderAuthenticationReviewAbandonRequest
	SupersededByReview     string
	OriginalAbandonedBy    string
	HistoricalWorkspace    bool
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
	result, err := authenticationHistoryProjection(a, id, false)
	if err != nil {
		return ProviderAuthenticationHistory{}, err
	}
	if a.authenticationJournal != nil {
		keys, err := a.authenticationJournal.AllKeys(ctx, config.AuthenticationJournalClient)
		if err != nil {
			return ProviderAuthenticationHistory{}, err
		}
		seen := map[string]bool{id: true}
		for _, key := range keys {
			if seen[key.WorkspaceID] {
				continue
			}
			seen[key.WorkspaceID] = true
			// Old records never enter the active authority's receipt maps and
			// therefore cannot install barriers or publish to a replacement ID.
			history := &clientAuthority{store: a.store, principal: a.principal, authenticationConnection: a.authenticationConnection, authenticationScope: a.authenticationScope, authenticationJournal: a.authenticationJournal}
			if err := history.loadAuthenticationJournal(ctx, key.WorkspaceID); err != nil {
				return ProviderAuthenticationHistory{}, err
			}
			older, err := authenticationHistoryProjection(history, key.WorkspaceID, true)
			if err != nil {
				return ProviderAuthenticationHistory{}, err
			}
			result.Operations = append(result.Operations, older.Operations...)
			result.Reviews = append(result.Reviews, older.Reviews...)
		}
	}
	sort.Slice(result.Operations, func(i, j int) bool {
		left, right := result.Operations[i], result.Operations[j]
		if left.Target.WorkspaceID != right.Target.WorkspaceID {
			return left.Target.WorkspaceID < right.Target.WorkspaceID
		}
		return left.OperationID < right.OperationID
	})
	sort.Slice(result.Reviews, func(i, j int) bool {
		left, right := result.Reviews[i], result.Reviews[j]
		lt, rt := clientAuthenticationReviewRequest(left.Request).target(), clientAuthenticationReviewRequest(right.Request).target()
		if lt.WorkspaceID != rt.WorkspaceID {
			return lt.WorkspaceID < rt.WorkspaceID
		}
		return left.Request.ReviewID < right.Request.ReviewID
	})
	return result, ctx.Err()
}

func authenticationHistoryProjection(a *clientAuthority, id string, historical bool) (ProviderAuthenticationHistory, error) {
	result := ProviderAuthenticationHistory{Operations: []ProviderAuthenticationHistoryOperation{}, Reviews: []ProviderAuthenticationHistoryReview{}}
	for _, receipt := range a.authenticationReceipts {
		if receipt.principal != a.principal || receipt.request.target.WorkspaceID != id {
			continue
		}
		outcome, err := clientAuthenticationOutcome(receipt, nil)
		if err != nil {
			return ProviderAuthenticationHistory{}, err
		}
		entry := ProviderAuthenticationHistoryOperation{JournalRevision: receipt.journalRevision, Abandoned: receipt.abandon != nil, HistoricalWorkspace: historical, OperationID: receipt.request.operationID, Target: receipt.request.target, Outcome: outcome, LocalFinished: receipt.localFinished, RemoteAcknowledged: receipt.acknowledged, Adopted: receipt.adopted, RecoverySequence: receipt.recoverySequence, ReviewSequence: receipt.reviewSequence, ReconciledBy: receipt.reconciledBy, SavedStateSupersededBy: receipt.savedStateSupersededBy}
		if receipt.abandon != nil {
			request := *receipt.abandon
			entry.AbandonRequest = &request
		}
		for _, recovery := range a.authenticationRecoveries {
			if recovery.request.OperationID == receipt.request.operationID && recovery.request.RecoverySequence == receipt.recoverySequence {
				request := ProviderAuthenticationRecoveryRequest(recovery.request)
				entry.RecoveryRequest = &request
				break
			}
		}
		result.Operations = append(result.Operations, entry)
	}
	for _, review := range a.authenticationReviews {
		if review.request.target().WorkspaceID != id {
			continue
		}
		entry := ProviderAuthenticationHistoryReview{JournalRevision: review.journalRevision, Abandoned: review.abandon != nil, OriginalAbandonedBy: review.originalAbandonedBy, SupersededByReview: review.supersededByReview, HistoricalWorkspace: historical, Request: ProviderAuthenticationReviewRequest(review.request), Summary: cloneAuthenticationReviewSummary(review.summary), SavedStateSupersededBy: review.savedStateSupersededBy}
		if review.abandon != nil {
			request := *review.abandon
			entry.AbandonRequest = &request
		}
		if review.apply != nil {
			entry.ApplyAttempted = review.apply.put
			request := ProviderAuthenticationApplyRequest(review.apply.request)
			outcome := review.apply.outcome
			entry.ApplyRequest, entry.ApplyOutcome = &request, &outcome
		}
		result.Reviews = append(result.Reviews, entry)
	}
	sort.Slice(result.Operations, func(i, j int) bool { return result.Operations[i].OperationID < result.Operations[j].OperationID })
	sort.Slice(result.Reviews, func(i, j int) bool { return result.Reviews[i].Request.ReviewID < result.Reviews[j].Request.ReviewID })
	return result, nil
}
