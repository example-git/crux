package workspace

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/providerauth"
)

// Reconciliation publishes separately reviewed, already coherent saved state.
// It never repairs, reloads, or repeats the original authentication mutation.
type ProviderAuthenticationReviewChoice = clientAuthenticationReviewChoice
type ProviderAuthenticationReviewModel = clientAuthenticationReviewModel
type ProviderAuthenticationReviewSummary = clientAuthenticationReviewSummary
type ProviderAuthenticationReconciliationOutcome = clientAuthenticationReconciliationOutcome
type ProviderAuthenticationReviewRequest clientAuthenticationReviewRequest
type ProviderAuthenticationApplyRequest clientAuthenticationApplyRequest

func (r ProviderAuthenticationReviewRequest) Validate() error {
	return clientAuthenticationReviewRequest(r).validate()
}
func (r ProviderAuthenticationApplyRequest) Validate() error {
	return clientAuthenticationApplyRequest(r).validate()
}

func (s clientAuthenticationReviewSummary) Validate(r ProviderAuthenticationReviewRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if s.OperationID != r.OperationID || s.ReviewID != r.ReviewID || s.ReviewSequence != r.ReviewSequence || s.Choice != r.Choice || s.FreshSaved != r.FreshSaved || s.Owner != clientAuthenticationReviewRequest(r).target().Owner {
		return providerauth.ErrReceiptUnverified
	}
	if err := validateAuthenticationReviewIDs(clientAuthenticationReviewRequest(r).target(), s.PreviewID); err != nil {
		return providerauth.ErrReceiptUnverified
	}
	if r.FreshSaved && (s.OriginalProgress != (providerauth.MutationProgress{}) || s.OriginalLogout || s.OriginalAccountID != "" || s.OriginalLoginID != "" || s.OriginalOAuthToken) {
		return providerauth.ErrReceiptUnverified
	}
	if s.Receiver.Mode != "client" || s.Receiver.Principal == "" || s.Receiver.Revision == 0 || s.Receiver.Digest == "" {
		return providerauth.ErrReceiptUnverified
	}
	return nil
}
func (o clientAuthenticationReconciliationOutcome) Validate(r ProviderAuthenticationApplyRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if o.OperationID != r.OperationID || o.ReviewID != r.ReviewID || o.PreviewID != r.PreviewID || o.ApplyID != r.ApplyID || o.Adopted && !o.RemoteAcknowledged {
		return providerauth.ErrReceiptUnverified
	}
	if r.FreshSaved {
		if o.OriginalDisposition != "not-applicable" {
			return providerauth.ErrReceiptUnverified
		}
	} else if o.OriginalDisposition != "unresolved" && o.OriginalDisposition != "runtime-reconciled" || (o.OriginalDisposition == "runtime-reconciled") != o.Adopted {
		return providerauth.ErrReceiptUnverified
	}

	return nil
}

// ProviderAuthenticationReconciler is optional. It is supported only by the
// owning client. Original mode uses retained switch/logout or confirmed OAuth
// receipts; fresh mode uses a separately captured current saved target and an
// explicit account, logout, complete OAuth token or credential-slot choice.
// Original checked-key probe provenance remains a distinct journal concern.
type ProviderAuthenticationReconciler interface {
	CanReconcileProviderAuthentication() bool
	ReviewProviderAuthentication(context.Context, ProviderAuthenticationReviewRequest) (ProviderAuthenticationReviewSummary, error)
	ApplyProviderAuthenticationReview(context.Context, ProviderAuthenticationApplyRequest) (ProviderAuthenticationReconciliationOutcome, error)
}

func (w *ClientWorkspace) CanReconcileProviderAuthentication() bool {
	return w != nil && w.CanRecoverProviderAuthentication()
}

var errAuthenticationReconciliationUnsupported = errors.New("saved authentication review/apply requires the owning client workspace; server-owned and local workspaces are not supported")

func (w *ClientWorkspace) ReviewProviderAuthentication(ctx context.Context, request ProviderAuthenticationReviewRequest) (ProviderAuthenticationReviewSummary, error) {
	if err := request.Validate(); err != nil {
		return ProviderAuthenticationReviewSummary{}, err
	}
	if !w.CanReconcileProviderAuthentication() {
		return ProviderAuthenticationReviewSummary{}, errAuthenticationReconciliationUnsupported
	}
	summary, err := w.reviewClientAuthentication(ctx, clientAuthenticationReviewRequest(request))
	if err == nil {
		err = summary.Validate(request)
	}
	return summary, err
}
func (w *ClientWorkspace) ApplyProviderAuthenticationReview(ctx context.Context, request ProviderAuthenticationApplyRequest) (ProviderAuthenticationReconciliationOutcome, error) {
	if err := request.Validate(); err != nil {
		return ProviderAuthenticationReconciliationOutcome{}, err
	}
	if !w.CanReconcileProviderAuthentication() {
		return ProviderAuthenticationReconciliationOutcome{}, errAuthenticationReconciliationUnsupported
	}
	outcome, err := w.applyClientAuthenticationReview(ctx, clientAuthenticationApplyRequest(request))
	if err == nil {
		err = outcome.Validate(request)
		if err == nil && !outcome.Adopted {
			err = providerauth.ErrReceiptUnverified
		}
	}
	return outcome, err
}
