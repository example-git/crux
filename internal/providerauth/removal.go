package providerauth

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

type RemoveRequest struct {
	OperationID string `json:"operation_id"`
	Target      Target `json:"target"`
	AccountID   string `json:"account_id"`
}

func (r RemoveRequest) Validate() error {
	if !validOperationID(r.OperationID) || !validText(r.AccountID, 4096, true) {
		return errors.New("invalid authentication removal request")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.Target.Owner.HasOAuth {
		return errors.New("authentication removal requires an OAuth owner")
	}
	return nil
}
func (o MutationOutcome) ValidateRemove(r RemoveRequest) error {
	if err := r.Validate(); err != nil {
		return err
	}
	return o.validateRequest(mutationRequest{operationID: r.OperationID, target: r.Target, accountID: r.AccountID, removedAccountID: r.AccountID})
}

type authenticationRemover interface {
	RemoveAuthenticationAccount(context.Context, config.Scope, config.AuthenticationCapture, providerregistry.RegistrationOwner, string) (config.AuthenticationMutationResult, error)
}

func (s *Service) Remove(ctx context.Context, request RemoveRequest) (MutationResult, error) {
	return s.remove(ctx, request, nil, nil)
}
func (s *Service) RemoveForAccepted(ctx context.Context, request RemoveRequest, accepted config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	return s.remove(ctx, request, &accepted, view)
}
func (s *Service) remove(ctx context.Context, request RemoveRequest, accepted *config.RemoteRuntimeProposal, view *config.Config) (MutationResult, error) {
	if err := request.Validate(); err != nil {
		return MutationResult{}, err
	}
	return s.mutate(ctx, mutationRequest{operationID: request.OperationID, target: request.Target, accountID: request.AccountID, removedAccountID: request.AccountID}, accepted, view)
}
func validateRemovalEffect(outcome MutationOutcome) error {
	current := outcome.Change.Current
	if !outcome.Progress.AccountsSaved || current.Status.ActiveAccountID == outcome.RemovedAccountID {
		return errors.New("authentication removal has no completed account effect")
	}
	for _, account := range current.Accounts {
		if account.ID == outcome.RemovedAccountID {
			return errors.New("authentication removal retained the requested account")
		}
	}
	if !outcome.Progress.RuntimePublished {
		return nil
	}
	if current.Status.ActiveAccountID == "" {
		if current.Status.AccountState != "none" || len(current.Accounts) != 0 {
			return errors.New("authentication removal did not clear the last selection")
		}
		for _, credential := range current.Status.Credentials {
			if credential.State != "absent" {
				return errors.New("authentication removal retained removed credentials")
			}
		}
		return nil
	}
	if !current.Status.Configured || current.Status.AccountState != "in-sync" {
		return errors.New("authentication removal did not publish its successor")
	}
	for _, credential := range current.Status.Credentials {
		if credential.Kind == "api-key" && credential.State != "configured" || credential.Kind == "oauth" && credential.State != "present" {
			return errors.New("authentication removal has no selected successor credential")
		}
	}
	return nil
}

// The public response proves removal and coherent status. The service additionally
// checks exact stored order and selection against its private admitted capture.
func validateCapturedRemoval(before config.AuthenticationCapture, owner providerregistry.RegistrationOwner, outcome MutationOutcome) error {
	accounts, err := before.Accounts(owner)
	if err != nil {
		return err
	}
	active, successor, err := before.RemovalSelection(owner, outcome.RemovedAccountID)
	if err != nil {
		return err
	}
	expected := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if account.ID != outcome.RemovedAccountID {
			expected = append(expected, account.ID)
		}
	}
	current := outcome.Change.Current
	if current.Status.ActiveAccountID != successor || len(current.Accounts) != len(expected) || outcome.Progress.RuntimePublished != (active == outcome.RemovedAccountID) {
		return errors.New("authentication removal changed an unrelated selection")
	}
	for i, id := range expected {
		if current.Accounts[i].ID != id {
			return errors.New("authentication removal changed unrelated accounts")
		}
	}
	return nil
}

// OriginalRemovalSelection retains admitted intent even on a partial account or
// config write; it never authorizes adopting a newer capture.
func (r MutationResult) OriginalRemovalSelection() (successor string, active, ok bool) {
	if r.removal == nil {
		return "", false, false
	}
	return r.removal.successor, r.removal.active, true
}

type removalIntent struct {
	successor string
	active    bool
}
