package providerauth

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

type checkedAPIKeyStore interface {
	PrepareCheckedAPIKey(context.Context, config.AuthenticationCapture, providerregistry.RegistrationOwner, string, string) (config.CheckedAPIKeyPreparation, error)
	SaveCheckedAPIKey(context.Context, config.Scope, config.CheckedAPIKeyPreparation) (config.AuthenticationMutationResult, error)
}

type apiKeyCheckReceipt struct {
	request     APIKeyCheckRequest
	outcome     APIKeyCheckOutcome
	before      config.AuthenticationCapture
	owner       providerregistry.RegistrationOwner
	preparation config.CheckedAPIKeyPreparation
	err         error
}

func (apiKeyCheckReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("API key check receipts are private")
}

func (apiKeyCheckReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private API key check receipt]"))
}

func (s *Service) CheckAPIKey(ctx context.Context, request APIKeyCheckRequest) (APIKeyCheckOutcome, error) {
	return s.checkAPIKey(ctx, request, nil, nil)
}

// CheckAPIKeyForAccepted performs the check on the owning client. Its probe
// evidence describes that client, not connectivity from the remote executor.
func (s *Service) CheckAPIKeyForAccepted(ctx context.Context, request APIKeyCheckRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (APIKeyCheckOutcome, error) {
	return s.checkAPIKey(ctx, request, &accepted, view)
}

func (s *Service) checkAPIKey(ctx context.Context, request APIKeyCheckRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (APIKeyCheckOutcome, error) {
	initial := APIKeyCheckOutcome{CheckID: request.CheckID, Previous: request.Target, CredentialID: request.CredentialID,
		Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeNotProbed, Policy: config.ConnectionProbePolicyNone}}
	if err := request.Validate(); err != nil {
		return initial, err
	}
	if request.Target.WorkspaceID != s.workspaceID || request.Target.Generation.Epoch != s.epoch {
		return initial, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return initial, err
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if receipt, found := s.keyChecks[request.CheckID]; found {
		if receipt.request != request {
			return initial, ErrOperationConflict
		}
		// This is a receipt read. It must not evaluate input, issue a probe or
		// refresh its historical target from a newer configuration capture.
		return cloneAPIKeyCheckOutcome(receipt.outcome), receipt.err
	}
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return initial, err
	}
	if snapshot.Generation != request.Target.Generation {
		return initial, ErrStale
	}
	var owner providerregistry.RegistrationOwner
	for _, provider := range before.Providers() {
		if PublicOwner(provider.Owner) == request.Target.Owner {
			owner = provider.Owner
			break
		}
	}
	if owner.ProviderID == "" {
		return initial, ErrOwner
	}
	if s.apiKeys == nil {
		return initial, errors.New("API key check service is unavailable")
	}
	if s.sequence == math.MaxUint64 {
		return initial, errors.New("authentication generation exhausted; reopen the workspace")
	}
	// Consume the target before external effects. A failed or evicted check
	// cannot repeat those effects under the initiating generation.
	s.sequence++
	checkedTarget := request.Target
	checkedTarget.Generation.Sequence = s.sequence
	receipt := apiKeyCheckReceipt{request: request, outcome: initial, before: before, owner: owner}
	receipt.preparation, err = s.apiKeys.PrepareCheckedAPIKey(ctx, before, owner, request.CredentialID, request.Source)
	receipt.outcome.Probe = receipt.preparation.ProbeResult()
	if err == nil {
		// Reobserve only to validate the initiating capture. A different
		// observation is a conflict, never replacement check authority.
		var current config.AuthenticationCapture
		_, current, err = s.capture(ctx, nil, nil)
		if err == nil && (!current.SameObservation(before) || s.sequence != checkedTarget.Generation.Sequence) {
			err = ErrStale
		}
	}
	if err == nil {
		receipt.outcome.CheckedTarget = &checkedTarget
	}
	if validationErr := receipt.outcome.Validate(); validationErr != nil {
		// An invalid provider result cannot create save authority. Preserve
		// no unsupported public evidence or private preparation in this case.
		receipt.outcome = initial
		err = validationErr
	}
	if err != nil {
		receipt.err = &mutationError{public: ErrAPIKeyCheck, cause: err}
		receipt.preparation = config.CheckedAPIKeyPreparation{}
	}
	s.retainAPIKeyCheck(receipt)
	return cloneAPIKeyCheckOutcome(receipt.outcome), receipt.err
}

func cloneAPIKeyCheckOutcome(outcome APIKeyCheckOutcome) APIKeyCheckOutcome {
	if outcome.CheckedTarget != nil {
		target := *outcome.CheckedTarget
		outcome.CheckedTarget = &target
	}
	return outcome
}

func (s *Service) retainAPIKeyCheck(receipt apiKeyCheckReceipt) {
	if s.keyChecks == nil {
		s.keyChecks = map[string]apiKeyCheckReceipt{}
	}
	if len(s.keyCheckIDs) == mutationReceiptLimit {
		delete(s.keyChecks, s.keyCheckIDs[0])
		s.keyCheckIDs = s.keyCheckIDs[1:]
	}
	s.keyCheckIDs = append(s.keyCheckIDs, receipt.request.CheckID)
	s.keyChecks[receipt.request.CheckID] = receipt
}
