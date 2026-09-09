package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// Review abandonment retires an exact attempted publication. It does not claim
// the receiver rejected it or change the original apply outcome. WorkspaceID
// names the current authority; Review retains its original workspace target.
type ProviderAuthenticationReviewAbandonRequest struct {
	WorkspaceID          string
	Review               ProviderAuthenticationReviewRequest
	PreviewID, AbandonID string
	Revision             uint64
}

func (r ProviderAuthenticationReviewAbandonRequest) Validate() error {
	if err := r.Review.Validate(); err != nil {
		return err
	}
	if r.Revision == 0 || r.AbandonID == r.Review.ReviewID || r.AbandonID == r.PreviewID || r.AbandonID == r.Review.OperationID {
		return errors.New("exact review revision and distinct abandonment ID are required")
	}
	target := clientAuthenticationReviewRequest(r.Review).target()
	current := target
	current.WorkspaceID = r.WorkspaceID
	if err := current.Validate(); err != nil {
		return err
	}
	return validateAuthenticationReviewIDs(target, r.PreviewID, r.AbandonID)
}

type ProviderAuthenticationReviewAbandonOutcome struct {
	Request   ProviderAuthenticationReviewAbandonRequest
	Abandoned bool
	Original  ProviderAuthenticationReconciliationOutcome
}

func (o ProviderAuthenticationReviewAbandonOutcome) Validate(r ProviderAuthenticationReviewAbandonRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if o.Request != r || !o.Abandoned || o.Original.Adopted {
		return providerauth.ErrReceiptUnverified
	}
	return o.Original.Validate(ProviderAuthenticationApplyRequest{
		OperationID: r.Review.OperationID, OriginalTarget: r.Review.OriginalTarget,
		FreshSaved: r.Review.FreshSaved, SavedTarget: r.Review.SavedTarget,
		ReviewID: r.Review.ReviewID, PreviewID: r.PreviewID, ApplyID: o.Original.ApplyID,
	})
}

type ProviderAuthenticationReviewAbandoner interface {
	AbandonProviderAuthenticationReview(context.Context, ProviderAuthenticationReviewAbandonRequest) (ProviderAuthenticationReviewAbandonOutcome, error)
}

func (w *ClientWorkspace) AbandonProviderAuthenticationReview(ctx context.Context, request ProviderAuthenticationReviewAbandonRequest) (ProviderAuthenticationReviewAbandonOutcome, error) {
	result := ProviderAuthenticationReviewAbandonOutcome{Request: request}
	if err := request.Validate(); err != nil {
		return result, err
	}
	if !w.CanRecoverProviderAuthentication() {
		return result, errors.New("review abandonment requires the owning client")
	}
	ctx, cancel := providerAuthContext(ctx, w.subCtx)
	defer cancel()
	a := w.authority
	if err := lockProviderAuthAuthority(ctx, a); err != nil {
		return result, err
	}
	defer a.mu.Unlock()
	if w.authority != a || w.workspaceID() != request.WorkspaceID || !w.clientOwned() {
		return result, providerauth.ErrStale
	}
	if err := a.loadAuthenticationJournal(ctx, request.WorkspaceID); err != nil {
		return result, err
	}
	if a.authenticationJournal == nil {
		return result, errors.New("this connection has no durable publication authority")
	}
	target := clientAuthenticationReviewRequest(request.Review).target()
	selected := a
	if target.WorkspaceID != request.WorkspaceID {
		selected = &clientAuthority{store: a.store, principal: a.principal, authenticationConnection: a.authenticationConnection, authenticationScope: a.authenticationScope, authenticationJournal: a.authenticationJournal}
	}
	release, err := selected.acquireAuthenticationPublication(ctx, target.WorkspaceID)
	if err != nil {
		return result, err
	}
	defer release()
	if err := selected.loadAuthenticationJournal(ctx, target.WorkspaceID); err != nil {
		return result, err
	}
	review := selected.authenticationReviews[request.Review.ReviewID]
	if review == nil || review.request != clientAuthenticationReviewRequest(request.Review) || review.summary.PreviewID != request.PreviewID || providerauth.PublicOwner(review.owner) != target.Owner || review.apply == nil || !review.apply.put {
		return result, providerauth.ErrOperationConflict
	}
	result.Original = review.apply.outcome
	if request.AbandonID == review.apply.request.ApplyID {
		return result, errors.New("abandonment ID must differ from the original apply ID")
	}
	if err := result.Original.Validate(ProviderAuthenticationApplyRequest(review.apply.request)); err != nil {
		return result, err
	}
	if review.abandon != nil {
		if *review.abandon != request {
			return result, providerauth.ErrOperationConflict
		}
		selected.discardAbandonedReviewPending(review)
		result.Abandoned = true
		return result, result.Validate(request)
	}
	if review.journalRevision != request.Revision {
		return result, errors.New("review publication history changed; review it again before abandonment")
	}
	if review.journalCompleted || review.apply.outcome.Adopted {
		return result, errors.New("review publication is already terminal and has no pending recovery to abandon")
	}
	copy := request
	review.abandon = &copy
	if err := selected.persistAuthenticationReview(ctx, review); err != nil {
		review.abandon = nil
		return result, err
	}
	selected.discardAbandonedReviewPending(review)
	result.Abandoned = true
	return result, result.Validate(request)
}

// The durable review marker is sufficient to derive this cache cleanup after a
// crash. It does not update an original operation's progress or acknowledgement.
func (a *clientAuthority) discardAbandonedReviewPending(review *clientAuthenticationReviewReceipt) {
	if a.pending != nil && review.proposal != nil && a.pending.Revision == review.proposal.Revision && a.pending.Digest == review.proposal.Digest {
		a.pending, a.pendingView = nil, nil
	}
	if !review.request.FreshSaved {
		if original := a.authenticationReceipts[review.request.OperationID]; original != nil && original.owner == review.owner && original.request.target == review.request.OriginalTarget && original.pendingReview == review.summary.PreviewID {
			original.pendingReview = ""
		}
	}
}
