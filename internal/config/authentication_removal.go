package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/providerregistry"
)

// RemoveAuthenticationAccount removes one captured account. Only removal of the
// selected account changes credentials; its successor is the first remaining
// account in stored order, matching the account database's selection semantics.
func (s *ConfigStore) RemoveAuthenticationAccount(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner, accountID string) (AuthenticationMutationResult, error) {
	if scope != ScopeGlobal && scope != ScopeWorkspace {
		return AuthenticationMutationResult{}, errors.New("invalid authentication removal scope")
	}
	if accountID == "" || owner.AccountNamespace == "" || !owner.HasOAuth {
		return AuthenticationMutationResult{}, errors.New("authentication removal requires an exact OAuth account")
	}
	active, successor, err := before.RemovalSelection(owner, accountID)
	if err != nil {
		return AuthenticationMutationResult{}, err
	}
	if active == accountID {
		return s.mutateAuthenticationChange(ctx, scope, before, owner, successor, accountID)
	}
	return s.removeInactiveAuthenticationAccount(ctx, before, owner, accountID)
}

// RemovalSelection uses captured storage order, which is distinct from the
// sorted public account list. It never reads ambient account state.
func (before AuthenticationCapture) RemovalSelection(owner providerregistry.RegistrationOwner, accountID string) (active, successor string, err error) {
	if !slices.Contains(before.owners, owner) {
		return "", "", errors.New("authentication removal owner changed")
	}
	found := false
	seen := map[string]bool{}
	for _, entry := range before.accounts.Entries(owner.AccountNamespace) {
		if entry.ID == "" || seen[entry.ID] {
			return "", "", errors.New("authentication removal has ambiguous account identities")
		}
		seen[entry.ID] = true
		if entry.ID == accountID {
			found = true
		} else if successor == "" {
			successor = entry.ID
		}
	}
	if !found {
		return "", "", errors.New("authentication removal account is unavailable")
	}
	active = before.accounts.ActiveID(owner.AccountNamespace)
	if active != accountID {
		successor = active
	}
	return active, successor, nil
}

// An inactive removal never stages config, refreshes a token, or prepares a
// runtime. The successful result retains the unchanged publication and the
// exact new account observation, including on completion after caller cancel.
func (s *ConfigStore) removeInactiveAuthenticationAccount(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner, accountID string) (result AuthenticationMutationResult, err error) {
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	globalPath, workspacePath := s.globalDataPath, s.workspacePath
	err = s.validateAuthenticationPublication(before, owner)
	var journal *localAuthenticationWriter
	if err == nil {
		ctx, journal, err = s.beginLocalAuthenticationChangeLocked(ctx, before, authenticationAdmission{}, owner, "remove", "", accountID)
	}
	s.writeMu.Unlock()
	defer func() { journal.finish(ctx, result, &err) }()
	if err != nil {
		return result, err
	}
	if !filepath.IsAbs(globalPath) {
		return result, errors.New("authentication configuration path must be absolute")
	}
	digest := sha256.Sum256([]byte(owner.ProviderID))
	refreshPath := filepath.Join(filepath.Dir(globalPath), "locks", fmt.Sprintf("%x.refresh.lock", digest))
	if err := os.MkdirAll(filepath.Dir(refreshPath), 0700); err != nil {
		return result, authenticationInputError(err)
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, refreshLockDeadline)
	release, err := lock.File(lockCtx, refreshPath)
	cancelLock()
	if err != nil {
		return result, authenticationInputError(err)
	}
	defer release()
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	defer s.writeMu.Unlock()
	validate := func(ctx context.Context) (authenticationConfigInputs, error) {
		if globalPath != s.globalDataPath || workspacePath != s.workspacePath {
			return authenticationConfigInputs{}, errAuthenticationInputsChanged
		}
		if err := s.validateAuthenticationPublication(before, owner); err != nil {
			return authenticationConfigInputs{}, err
		}
		inputs, err := s.captureAuthenticationInputsLocked(ctx, before.runtime)
		if err != nil {
			return authenticationConfigInputs{}, err
		}
		if !before.inputs.sameObservation(inputs) {
			return authenticationConfigInputs{}, errAuthenticationInputsChanged
		}
		return inputs, ctx.Err()
	}
	if _, err := validate(ctx); err != nil {
		return result, err
	}
	pending, err := before.accounts.BeginRemove(ctx, owner.AccountNamespace, accountID)
	if err != nil {
		return result, err
	}
	defer pending.Close()
	if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	releaseScopes, err := lockAuthenticationScopes(ctx, authenticationAdmission{globalPath: globalPath, workspacePath: workspacePath})
	if err != nil {
		return result, err
	}
	defer releaseScopes()
	if _, err := validate(ctx); err != nil {
		return result, err
	}
	committed, err := pending.Commit(ctx)
	result.AccountsSaved = committed.Written
	if err != nil {
		return result, err
	}
	completion, cancel := context.WithDeadline(context.WithoutCancel(ctx), committed.CompletionDeadline())
	defer cancel()
	inputs, err := validate(completion)
	if err != nil {
		return result, err
	}
	after, err := pending.VerifyCommitted(completion)
	if err != nil {
		return result, err
	}
	if after.ActiveID(owner.AccountNamespace) != before.accounts.ActiveID(owner.AccountNamespace) {
		return result, errors.New("inactive removal changed the selected account")
	}
	result.After = AuthenticationCapture{runtime: before.runtime, accounts: after, owners: slices.Clone(before.owners), inputs: inputs}
	result.accountOnly = true
	return result, nil
}
