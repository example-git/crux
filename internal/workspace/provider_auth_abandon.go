package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// Abandon stops recovery of an exact original publication. It does not undo a
// local write or an earlier PUT, assert rejection, or manufacture an outcome.
// WorkspaceID is the current UI authority; Target remains the original target.
type ProviderAuthenticationAbandonRequest struct {
	WorkspaceID            string
	Target                 providerauth.Target
	OperationID, AbandonID string
	Revision               uint64
}

func (r ProviderAuthenticationAbandonRequest) Validate() error {
	if r.WorkspaceID == "" || r.Revision == 0 || r.AbandonID == r.OperationID {
		return errors.New("exact current workspace, original revision and distinct abandonment ID are required")
	}
	current := r.Target
	current.WorkspaceID = r.WorkspaceID
	if err := current.Validate(); err != nil {
		return err
	}
	if err := validateAuthenticationReviewIDs(r.Target, r.OperationID, r.AbandonID); err != nil {
		return err
	}
	return nil
}

type ProviderAuthenticationAbandonOutcome struct {
	Request                     ProviderAuthenticationAbandonRequest
	Abandoned                   bool
	Original                    providerauth.MutationOutcome
	RemoteAcknowledged, Adopted bool
}

func (o ProviderAuthenticationAbandonOutcome) Validate(r ProviderAuthenticationAbandonRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if err := o.Original.Validate(); err != nil {
		return providerauth.ErrReceiptUnverified
	}
	if o.Request != r || !o.Abandoned || o.Original.OperationID != r.OperationID || o.Original.Previous != r.Target || o.Adopted && !o.RemoteAcknowledged {
		return providerauth.ErrReceiptUnverified
	}
	return nil
}

type ProviderAuthenticationAbandoner interface {
	AbandonProviderAuthentication(context.Context, ProviderAuthenticationAbandonRequest) (ProviderAuthenticationAbandonOutcome, error)
}

func (w *ClientWorkspace) AbandonProviderAuthentication(ctx context.Context, request ProviderAuthenticationAbandonRequest) (ProviderAuthenticationAbandonOutcome, error) {
	result := ProviderAuthenticationAbandonOutcome{Request: request}
	if err := request.Validate(); err != nil {
		return result, err
	}
	if !w.CanRecoverProviderAuthentication() {
		return result, errors.New("publication abandonment requires the owning client")
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
	selected := a
	if request.Target.WorkspaceID != request.WorkspaceID {
		// Historical IDs remain isolated from the replacement workspace's maps.
		selected = &clientAuthority{store: a.store, principal: a.principal, authenticationConnection: a.authenticationConnection, authenticationScope: a.authenticationScope, authenticationJournal: a.authenticationJournal}
	}
	release, err := selected.acquireAuthenticationPublication(ctx, request.Target.WorkspaceID)
	if err != nil {
		return result, err
	}
	defer release()
	if err := selected.loadAuthenticationJournal(ctx, request.Target.WorkspaceID); err != nil {
		return result, err
	}
	receipt := selected.authenticationReceipts[request.OperationID]
	if receipt == nil || receipt.request.target != request.Target || receipt.principal != a.principal || providerauth.PublicOwner(receipt.owner) != request.Target.Owner {
		return result, providerauth.ErrOperationConflict
	}
	original, err := clientAuthenticationOutcome(receipt, nil)
	if err != nil {
		return result, err
	}
	result.Original, result.RemoteAcknowledged, result.Adopted = original, receipt.acknowledged, receipt.adopted
	if receipt.abandon != nil {
		// WorkspaceID is the current authority envelope, already checked
		// above. The original target, operation, revision and action ID remain
		// the durable action identity across workspace reincarnations.
		retained := *receipt.abandon
		retained.WorkspaceID = request.WorkspaceID
		if retained != request {
			return result, providerauth.ErrOperationConflict
		}
		result.Abandoned = true
		if err := selected.retireAbandonedOriginalReviews(ctx, receipt); err != nil {
			return result, err
		}
		return result, result.Validate(request)
	}
	if receipt.journalRevision != request.Revision {
		return result, errors.New("original publication history changed; review it again before abandonment")
	}
	if receipt.journalCompleted {
		return result, errors.New("original publication is already terminal and has no pending recovery to abandon")
	}
	copy := request
	receipt.abandon = &copy
	if err := selected.persistAuthenticationReceipt(ctx, receipt); err != nil {
		// Until exact durable readback succeeds, keep all active barriers.
		receipt.abandon = nil
		return result, err
	}
	result.Abandoned = true
	if err := selected.retireAbandonedOriginalReviews(ctx, receipt); err != nil {
		return result, err
	}
	return result, result.Validate(request)
}

// The caller holds the publication lane lease. Each associated record keeps
// its own immutable progress/ack, with only a separate terminal marker added.
func (a *clientAuthority) retireAbandonedOriginalReviews(ctx context.Context, original *clientAuthenticationReceipt) error {
	a.discardAbandonedOriginalPending(original)
	for _, review := range a.authenticationReviews {
		if review.request.FreshSaved || review.request.OperationID != original.request.operationID || review.request.OriginalTarget != original.request.target || review.owner != original.owner || review.journalCompleted {
			continue
		}
		review.originalAbandonedBy = original.abandon.AbandonID
		if err := a.persistAuthenticationReview(ctx, review); err != nil {
			return err
		}
	}
	return nil
}

func (a *clientAuthority) discardAbandonedOriginalPending(original *clientAuthenticationReceipt) {
	if a.pending != nil && original.proposal != nil && a.pending.Revision == original.proposal.Revision && a.pending.Digest == original.proposal.Digest {
		a.pending, a.pendingView = nil, nil
	}
	if a.pending != nil {
		for _, review := range a.authenticationReviews {
			if !review.request.FreshSaved && review.request.OperationID == original.request.operationID && review.request.OriginalTarget == original.request.target && review.proposal != nil && a.pending.Revision == review.proposal.Revision && a.pending.Digest == review.proposal.Digest {
				a.pending, a.pendingView = nil, nil
				break
			}
		}
	}
}
