package providerauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

const (
	mutationReceiptLimit      = 128
	mutationVerificationLimit = 5 * time.Second
)

type authenticationMutator interface {
	SwitchAuthenticationAccount(context.Context, config.Scope, config.AuthenticationCapture, providerregistry.RegistrationOwner, string) (config.AuthenticationMutationResult, error)
	LogoutAuthentication(context.Context, config.Scope, config.AuthenticationCapture, providerregistry.RegistrationOwner) (config.AuthenticationMutationResult, error)
}

type mutationRequest struct {
	operationID string
	target      Target
	accountID   string
	logout      bool
}

type mutationReceipt struct {
	request       mutationRequest
	outcome       MutationOutcome
	after         config.AuthenticationCapture
	runtime       config.RuntimeSnapshot
	originalOwner providerregistry.RegistrationOwner
	err           error
}

func (mutationReceipt) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication mutation receipts are private")
}

func (mutationReceipt) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private authentication mutation receipt]"))
}

// Switch and Logout preserve the existing account menu's global scope. They
// serialize with status/account reads and invoke one fixed owner-bound config
// transaction; callers cannot supply a path, namespace or arbitrary callback.
func (s *Service) Switch(ctx context.Context, request SwitchRequest) (MutationResult, error) {
	return s.switchAccount(ctx, request, nil, nil)
}

func (s *Service) Logout(ctx context.Context, request LogoutRequest) (MutationResult, error) {
	return s.logout(ctx, request, nil, nil)
}

// ForAccepted validates initial mutation admission against the owning client's
// retained accepted runtime. Receipt replay recovers only the exact local result;
// it does not claim that the client has published or acknowledged that result.
func (s *Service) SwitchForAccepted(ctx context.Context, request SwitchRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	return s.switchAccount(ctx, request, &accepted, view)
}

func (s *Service) LogoutForAccepted(ctx context.Context, request LogoutRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	return s.logout(ctx, request, &accepted, view)
}

func (s *Service) switchAccount(ctx context.Context, request SwitchRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	if err := request.Validate(); err != nil {
		return MutationResult{}, err
	}
	return s.mutate(ctx, mutationRequest{operationID: request.OperationID, target: request.Target, accountID: request.AccountID}, accepted, view)
}

func (s *Service) logout(ctx context.Context, request LogoutRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	if err := request.Validate(); err != nil {
		return MutationResult{}, err
	}
	return s.mutate(ctx, mutationRequest{operationID: request.OperationID, target: request.Target, logout: true}, accepted, view)
}

func (s *Service) mutate(ctx context.Context, request mutationRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	initial := MutationResult{Outcome: MutationOutcome{OperationID: request.operationID, Previous: request.target}}
	if request.target.WorkspaceID != s.workspaceID || request.target.Generation.Epoch != s.epoch {
		return initial, ErrStale
	}
	if err := s.acquire(ctx); err != nil {
		return initial, err
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return initial, err
	}
	if receipt, found := s.receipts[request.operationID]; found {
		if receipt.request != request {
			return initial, ErrOperationConflict
		}
		return s.replay(ctx, receipt)
	}
	snapshot, before, err := s.capture(ctx, accepted, view)
	if err != nil {
		return initial, err
	}
	if snapshot.Generation != request.target.Generation {
		return initial, ErrStale
	}
	var owner providerregistry.RegistrationOwner
	for _, provider := range before.Providers() {
		if PublicOwner(provider.Owner) == request.target.Owner {
			owner = provider.Owner
			break
		}
	}
	if owner.ProviderID == "" {
		return initial, ErrOwner
	}
	if !request.logout {
		if !owner.HasOAuth || owner.AccountNamespace == "" {
			return initial, ErrOwner
		}
		state, err := accountsState(snapshot, before, request.target.Owner)
		if err != nil {
			return initial, err
		}
		found := false
		for _, account := range state.Accounts {
			found = found || account.ID == request.accountID
		}
		if !found {
			return initial, ErrAccount
		}
	}
	if s.sequence == math.MaxUint64 {
		return initial, errors.New("authentication generation exhausted; reopen the workspace")
	}
	if s.mutations == nil {
		return initial, errors.New("authentication mutation service is unavailable")
	}
	// Capture provenance at admission, before the transaction can change files
	// or fail without a coherent After. Never derive this from a later capture.
	receipt := mutationReceipt{request: request, originalOwner: owner}
	var transaction config.AuthenticationMutationResult
	if request.logout {
		transaction, err = s.mutations.LogoutAuthentication(ctx, config.ScopeGlobal, before, owner)
	} else {
		transaction, err = s.mutations.SwitchAuthenticationAccount(ctx, config.ScopeGlobal, before, owner, request.accountID)
	}
	receipt.outcome = MutationOutcome{OperationID: request.operationID, Previous: request.target, Progress: MutationProgress{
		AccountRefreshed: transaction.AccountRefreshed, AccountsSaved: transaction.AccountsSaved,
		ConfigSaved: transaction.ConfigSaved, RuntimePublished: transaction.RuntimePublished,
	}}
	runtime, coherent := transaction.RuntimeSnapshot()
	if err != nil || !coherent || transaction.After.SameObservation(before) {
		// Even an unchanged failure consumes its initiating target. Therefore an
		// evicted receipt cannot repeat effects under that old generation. This
		// is deliberately not a lifetime operation-ID uniqueness guarantee.
		s.sequence++
		if err == nil {
			err = errors.New("authentication transaction returned no coherent changed publication")
		}
		receipt.err = safeMutationError(err)
		s.retain(receipt)
		return s.replay(ctx, receipt)
	}
	post, observeErr := s.observe(transaction.After)
	if observeErr == nil {
		var current AccountsState
		current, observeErr = accountsState(post, transaction.After, request.target.Owner)
		if observeErr == nil {
			receipt.outcome.Change = &Change{OperationID: request.operationID, Previous: request.target, Current: current, Models: publicModels(runtime)}
			observeErr = validateMutationEffect(request, receipt.outcome)
		}
	}
	if observeErr != nil {
		// Publication already happened. Preserve progress, but never turn an
		// arbitrary later capture into a substitute successful transaction.
		receipt.outcome.Change = nil
		receipt.err = safeMutationError(observeErr)
	} else {
		receipt.after, receipt.runtime = transaction.After, runtime
	}
	s.retain(receipt)
	// Config completion may have outlived the original request after its first
	// rename. Preserve that success instead of reclassifying it as rollback.
	completion, cancel := context.WithTimeout(context.WithoutCancel(ctx), mutationVerificationLimit)
	defer cancel()
	return s.replay(completion, receipt)
}

func (s *Service) retain(receipt mutationReceipt) {
	if len(s.receiptIDs) == mutationReceiptLimit {
		delete(s.receipts, s.receiptIDs[0])
		s.receiptIDs = s.receiptIDs[1:]
	}
	s.receiptIDs = append(s.receiptIDs, receipt.request.operationID)
	s.receipts[receipt.request.operationID] = receipt
}

func (s *Service) replay(ctx context.Context, receipt mutationReceipt) (MutationResult, error) {
	outcome, err := cloneMutationOutcome(receipt.outcome)
	if err != nil {
		return MutationResult{Outcome: MutationOutcome{OperationID: receipt.request.operationID, Previous: receipt.request.target, Progress: receipt.outcome.Progress}, originalOwner: receipt.originalOwner}, safeMutationError(err)
	}
	result := MutationResult{Outcome: outcome, originalOwner: receipt.originalOwner}
	if receipt.err != nil {
		return result, receipt.err
	}
	snapshot, current, err := s.capture(ctx, nil, nil)
	if err != nil {
		return result, &mutationError{public: ErrReceiptUnverified, cause: err}
	}
	if !current.SameObservation(receipt.after) || snapshot.Generation != receipt.outcome.Change.Current.Target.Generation {
		result.Outcome.Superseded = true
		return result, nil
	}
	result.runtime, result.after, result.current = receipt.runtime, receipt.after, true
	return result, nil
}

func cloneMutationOutcome(value MutationOutcome) (MutationOutcome, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return MutationOutcome{}, err
	}
	var copy MutationOutcome
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&copy); err != nil {
		return MutationOutcome{}, err
	}
	return copy, copy.Validate()
}

func publicModels(snapshot config.RuntimeSnapshot) ModelState {
	private := snapshot.AgentModelState()
	convert := func(selected *config.OwnedSelectedModel) *OwnedModelState {
		if selected == nil {
			return nil
		}
		result := &OwnedModelState{Model: selected.Model}
		if selected.Owner.ProviderID != "" {
			owner := PublicOwner(selected.Owner)
			result.Owner = &owner
		}
		return result
	}
	return ModelState{Large: convert(private.Large), Small: convert(private.Small)}
}

func validateMutationEffect(request mutationRequest, outcome MutationOutcome) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	if outcome.Change == nil || outcome.Change.OperationID != request.operationID || outcome.Change.Previous != request.target {
		return errors.New("authentication transaction has no matching change receipt")
	}
	current := outcome.Change.Current
	if request.logout {
		if current.Status.ActiveAccountID != "" || current.Status.AccountState != "none" || len(current.Accounts) != 0 {
			return errors.New("authentication logout did not clear the selected owner's accounts")
		}
		for _, credential := range current.Status.Credentials {
			if credential.State != "absent" {
				return errors.New("authentication logout retained credentials")
			}
		}
		return nil
	}
	if !current.Status.Configured || current.Status.ActiveAccountID != request.accountID || current.Status.AccountState != "in-sync" {
		return errors.New("authentication switch did not install the selected account")
	}
	for _, credential := range current.Status.Credentials {
		if credential.Kind == "api-key" && credential.State != "configured" || credential.Kind == "oauth" && credential.State != "present" {
			return errors.New("authentication switch has no selected credential")
		}
	}
	return nil
}

// Causes remain inspectable with errors.Is, but paths, account metadata,
// refresh payloads and schema errors never enter the public error string.
type mutationError struct {
	public error
	cause  error
}

func safeMutationError(cause error) error { return &mutationError{public: ErrMutation, cause: cause} }
func (e *mutationError) Error() string    { return e.public.Error() }
func (e *mutationError) Unwrap() []error  { return []error{e.public, e.cause} }
func (e *mutationError) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte(e.Error()))
}
