package providerauth

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
)

// Abandonment retires a tokenless preparation or unknown exchange's reservation.
// It is neither login authorization nor evidence that the original exchange
// failed or succeeded.
type OAuthLoginAbandonRequest struct {
	Target              Target `json:"target"`
	OriginalWorkspaceID string `json:"original_workspace_id"`
	OriginalOperationID string `json:"original_operation_id"`
}

func (r OAuthLoginAbandonRequest) Validate() error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.Target.Owner.HasOAuth || !validText(r.OriginalWorkspaceID, 512, true) || !validOperationID(r.OriginalOperationID) {
		return errors.New("invalid OAuth abandonment request")
	}
	return nil
}

type OAuthLoginAbandonOutcome struct {
	Request         OAuthLoginAbandonRequest `json:"request"`
	Abandoned       bool                     `json:"abandoned"`
	ExchangeOutcome string                   `json:"exchange_outcome,omitempty"`
}

func (o OAuthLoginAbandonOutcome) Validate(request OAuthLoginAbandonRequest) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if o.Request != request || o.Abandoned && o.ExchangeOutcome != "unknown" && o.ExchangeOutcome != "not-started" || !o.Abandoned && o.ExchangeOutcome != "" {
		return errors.New("OAuth abandonment receipt does not match its original operation")
	}
	return nil
}

func (s *Service) AbandonOAuthLoginResult(ctx context.Context, request OAuthLoginAbandonRequest) (OAuthLoginAbandonOutcome, error) {
	return s.abandonOAuthLoginResult(ctx, request, nil, nil)
}

func (s *Service) AbandonOAuthLoginResultForAccepted(ctx context.Context, request OAuthLoginAbandonRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginAbandonOutcome, error) {
	return s.abandonOAuthLoginResult(ctx, request, &accepted, view)
}

func (s *Service) abandonOAuthLoginResult(ctx context.Context, request OAuthLoginAbandonRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginAbandonOutcome, error) {
	outcome := OAuthLoginAbandonOutcome{Request: request}
	if err := request.Validate(); err != nil {
		return outcome, err
	}
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return outcome, err
	}
	defer func() { <-s.gate }()
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return outcome, err
	}
	if request.Target.WorkspaceID != s.workspaceID || request.Target.Generation != snapshot.Generation {
		return outcome, ErrStale
	}
	for _, provider := range before.Providers() {
		if PublicOwner(provider.Owner) != request.Target.Owner {
			continue
		}
		disposition, err := s.store.AbandonOAuthLoginResult(ctx, before, provider.Owner, request.OriginalWorkspaceID, request.OriginalOperationID)
		outcome.Abandoned = disposition != ""
		outcome.ExchangeOutcome = disposition
		return outcome, err
	}
	return outcome, ErrOwner
}
