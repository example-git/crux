package providerauth

import (
	"context"
	"errors"
	"math"
	"reflect"

	"github.com/example-git/crux/internal/config"
)

func (s *Service) SaveAPIKey(ctx context.Context, request APIKeySaveRequest) (MutationResult, error) {
	return s.saveAPIKey(ctx, request, nil, nil)
}

func (s *Service) SaveAPIKeyForAccepted(ctx context.Context, request APIKeySaveRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	return s.saveAPIKey(ctx, request, &accepted, view)
}

func (s *Service) saveAPIKey(ctx context.Context, request APIKeySaveRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	ctx, done := s.operationContext(ctx)
	defer done()
	initial := MutationResult{Outcome: MutationOutcome{OperationID: request.OperationID, CheckID: request.CheckID, Previous: request.Target}}
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
	mutation := mutationRequest{operationID: request.OperationID, target: request.Target, checkID: request.CheckID}
	if receipt, found := s.receipts[request.OperationID]; found {
		if receipt.request != mutation {
			return initial, ErrOperationConflict
		}
		return s.replay(ctx, receipt)
	}
	if s.oauthOperationReserved(request.OperationID) {
		return initial, ErrOperationConflict
	}
	check, found := s.keyChecks[request.CheckID]
	if !found || check.err != nil || check.outcome.CheckedTarget == nil {
		return initial, ErrAPIKeyCheckUnavailable
	}
	if *check.outcome.CheckedTarget != request.Target {
		return initial, ErrStale
	}
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return initial, err
	}
	if snapshot.Generation != request.Target.Generation || !before.SameObservation(check.before) {
		return initial, ErrStale
	}
	if s.sequence == math.MaxUint64 {
		return initial, errors.New("authentication generation exhausted; reopen the workspace")
	}
	if s.apiKeys == nil {
		return initial, errors.New("checked API key save service is unavailable")
	}
	// Admission is complete. The only mutator receives the retained private
	// preparation, never caller input or a newly resolved replacement value.
	ctx = config.ContextWithAuthenticationOperation(ctx, config.AuthenticationJournalKey{Kind: config.AuthenticationJournalLocal, WorkspaceID: s.workspaceID, OperationID: request.OperationID})
	effectID, effectErr := check.preparation.ConfiguredCredentialEffectID()
	transaction, err := s.apiKeys.SaveCheckedAPIKey(ctx, config.ScopeGlobal, check.preparation)
	receipt := mutationReceipt{request: mutation, originalOwner: check.owner, outcome: MutationOutcome{
		OperationID: request.OperationID, CheckID: request.CheckID, CredentialID: check.outcome.CredentialID, Previous: request.Target, Progress: MutationProgress{
			AccountRefreshed: transaction.AccountRefreshed, AccountsSaved: transaction.AccountsSaved,
			ConfigSaved: transaction.ConfigSaved, RuntimePublished: transaction.RuntimePublished,
		}}}
	if effectErr == nil {
		receipt.credentialEffectID = effectID
	}
	runtime, coherent := transaction.RuntimeSnapshot()
	if err != nil || !coherent || transaction.After.SameObservation(before) {
		s.sequence++
		if err == nil {
			err = errors.New("checked API key save returned no coherent changed publication")
		}
		receipt.err = safeMutationError(err)
		s.retain(receipt)
		return s.replay(ctx, receipt)
	}
	confirmedID, proofErr := transaction.After.ConfiguredCredentialEffectID(check.owner, check.outcome.CredentialID)
	if proofErr != nil || effectErr != nil || confirmedID != effectID {
		receipt.err = safeMutationError(errors.New("checked credential effect was not confirmed by its transaction"))
		s.retain(receipt)
		return s.replay(ctx, receipt)
	}
	post, observeErr := s.observe(transaction.After)
	if observeErr == nil {
		var current AccountsState
		current, observeErr = accountsState(post, transaction.After, request.Target.Owner)
		if observeErr == nil {
			receipt.outcome.Change = &Change{OperationID: request.OperationID, Previous: request.Target, Current: current, Models: publicModels(runtime)}
			observeErr = validateMutationEffect(mutation, receipt.outcome)
		}
	}
	if observeErr == nil {
		oldAccounts, oldErr := before.Accounts(check.owner)
		newAccounts, newErr := transaction.After.Accounts(check.owner)
		if oldErr != nil || newErr != nil || !reflect.DeepEqual(oldAccounts, newAccounts) {
			observeErr = errors.New("checked API key save changed saved account selection or summaries")
		}
	}
	if observeErr != nil {
		receipt.outcome.Change = nil
		receipt.err = safeMutationError(observeErr)
	} else {
		receipt.after, receipt.runtime = transaction.After, runtime
	}
	s.retain(receipt)
	completion, cancel := context.WithTimeout(context.WithoutCancel(ctx), mutationVerificationLimit)
	defer cancel()
	return s.replay(completion, receipt)
}

func validateAPIKeySaveEffect(outcome MutationOutcome) error {
	if outcome.Change == nil || outcome.Progress.AccountRefreshed || outcome.Progress.AccountsSaved ||
		!outcome.Progress.ConfigSaved || !outcome.Progress.RuntimePublished {
		return errors.New("checked API key save has inconsistent local progress")
	}
	status := outcome.Change.Current.Status
	if !status.Configured {
		return errors.New("checked API key save did not configure its provider")
	}
	if outcome.CredentialID == "" {
		return errors.New("checked credential save has no selected slot")
	}
	if outcome.CredentialID != "provider.api_key" {
		for _, slot := range status.CredentialSlots {
			if slot.ID == outcome.CredentialID && slot.Property != "" && slot.Configured {
				return nil
			}
		}
		return errors.New("checked configuration credential save did not install its selected slot")
	}
	for _, credential := range status.Credentials {
		if credential.Kind == "api-key" && credential.State != "configured" ||
			credential.Kind == "oauth" && credential.State != "absent" {
			return errors.New("checked API key save did not install its selected credential kind")
		}
	}
	return nil
}
