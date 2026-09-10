package providerauth

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
)

// OAuthLoginRecovery identifies an observed token result, never a successful
// original save or runtime acknowledgement. It contains no credential data.
type OAuthLoginRecovery struct {
	OriginalWorkspaceID string `json:"original_workspace_id"`
	OriginalOperationID string `json:"original_operation_id"`
}
type OAuthLoginRecoveryRequest struct {
	OriginalWorkspaceID string            `json:"original_workspace_id"`
	Login               OAuthLoginRequest `json:"login"`
	OriginalOperationID string            `json:"original_operation_id"`
}

func (r OAuthLoginRecoveryRequest) Validate() error {
	if err := r.Login.Validate(); err != nil {
		return err
	}
	if !validText(r.OriginalWorkspaceID, 512, true) {
		return errors.New("OAuth recovery requires the original workspace")
	}
	if !validOperationID(r.OriginalOperationID) || r.OriginalOperationID == r.Login.OperationID || r.OriginalOperationID == r.Login.LoginID {
		return errors.New("OAuth recovery requires a distinct original operation")
	}
	return nil
}

func sameOAuthRecovery(a, b *OAuthLoginRecovery) bool {
	return a == nil && b == nil || a != nil && b != nil && *a == *b
}

// MatchesOAuthLoginRecovery keeps a fresh session's recovery provenance exact
// across Begin/Recover retries and later state observations.
func (s OAuthLoginState) MatchesOAuthLoginRecovery(originalWorkspaceID, originalOperationID string) bool {
	if originalOperationID == "" {
		return originalWorkspaceID == "" && s.Recovery == nil
	}
	return s.Recovery != nil && s.Recovery.OriginalWorkspaceID == originalWorkspaceID && s.Recovery.OriginalOperationID == originalOperationID
}

type OAuthLoginRecordedResult struct {
	OriginalWorkspaceID string `json:"original_workspace_id"`
	OperationID         string `json:"operation_id"`
	State               string `json:"state"`
	Abandoned           bool   `json:"abandoned,omitempty"`
}
type OAuthLoginRecoveryList struct {
	Target  Target                     `json:"target"`
	Results []OAuthLoginRecordedResult `json:"results"`
}

func (r OAuthLoginRecoveryList) Validate() error {
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.Target.Owner.HasOAuth || len(r.Results) > 128 {
		return errors.New("invalid OAuth recovery listing")
	}
	seen := make(map[[2]string]struct{}, len(r.Results))
	for _, result := range r.Results {
		if !validText(result.OriginalWorkspaceID, 512, true) || !validOperationID(result.OperationID) {
			return errors.New("invalid recorded OAuth operation")
		}
		if _, exists := seen[[2]string{result.OriginalWorkspaceID, result.OperationID}]; exists {
			return errors.New("duplicate recorded OAuth operation")
		}
		seen[[2]string{result.OriginalWorkspaceID, result.OperationID}] = struct{}{}
		if result.Abandoned && result.State != "exchange-outcome-unknown" && result.State != "not-started" && result.State != "token-result-recorded" {
			return errors.New("invalid abandoned OAuth result")
		}
		switch result.State {
		case "not-started", "exchange-outcome-unknown", "token-result-recorded":
		default:
			return errors.New("invalid recorded OAuth state")
		}
	}
	return nil
}

func (s *Service) RecoverOAuthLogin(ctx context.Context, request OAuthLoginRecoveryRequest) (OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return OAuthLoginState{}, err
	}
	return s.beginOAuthLogin(ctx, request.Login, nil, nil, &OAuthLoginRecovery{OriginalWorkspaceID: request.OriginalWorkspaceID, OriginalOperationID: request.OriginalOperationID})
}

func (s *Service) RecoverOAuthLoginForAccepted(ctx context.Context, request OAuthLoginRecoveryRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginState, error) {
	if err := request.Validate(); err != nil {
		return OAuthLoginState{}, err
	}
	return s.beginOAuthLogin(ctx, request.Login, &accepted, view, &OAuthLoginRecovery{OriginalWorkspaceID: request.OriginalWorkspaceID, OriginalOperationID: request.OriginalOperationID})
}

func (s *Service) ListOAuthLoginResults(ctx context.Context, target Target) (OAuthLoginRecoveryList, error) {
	return s.listOAuthLoginResults(ctx, target, nil, nil)
}

func (s *Service) ListOAuthLoginResultsForAccepted(ctx context.Context, target Target, accepted config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginRecoveryList, error) {
	return s.listOAuthLoginResults(ctx, target, &accepted, view)
}

func (s *Service) listOAuthLoginResults(ctx context.Context, target Target, accepted *config.RemoteRuntimeProposal, view *config.Config) (OAuthLoginRecoveryList, error) {
	result := OAuthLoginRecoveryList{Target: target, Results: []OAuthLoginRecordedResult{}}
	if err := target.Validate(); err != nil {
		return result, err
	}
	if !target.Owner.HasOAuth {
		return result, ErrOwner
	}
	ctx, done := s.operationContext(ctx)
	defer done()
	if err := s.acquire(ctx); err != nil {
		return result, err
	}
	defer func() { <-s.gate }()
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return result, err
	}
	if target.WorkspaceID != s.workspaceID || target.Generation != snapshot.Generation {
		return result, ErrStale
	}
	for _, provider := range before.Providers() {
		if PublicOwner(provider.Owner) != target.Owner {
			continue
		}
		records, err := s.store.PendingOAuthLoginResultsForScope(ctx)
		if err != nil {
			return result, err
		}
		for _, record := range records {
			if record.Owner == provider.Owner {
				result.Results = append(result.Results, OAuthLoginRecordedResult{OriginalWorkspaceID: record.OriginalWorkspaceID, OperationID: record.OperationID, State: record.State, Abandoned: record.Abandoned})
			}
		}
		latest, _, err := s.capture(ctx, accepted, view)
		if err != nil {
			return OAuthLoginRecoveryList{Target: target}, err
		}
		if latest.Generation != target.Generation {
			return OAuthLoginRecoveryList{Target: target}, ErrStale
		}
		if err := result.Validate(); err != nil {
			return OAuthLoginRecoveryList{Target: target}, err
		}
		return result, ctx.Err()
	}
	return result, ErrOwner
}
