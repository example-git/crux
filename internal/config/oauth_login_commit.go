package config

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth/accounts"
)

// CommitOAuthLogin consumes a private authorization from this store. Copies
// return the retained original result/error; they cannot repeat partial writes.
// RuntimePublished is local publication, never receiver acknowledgement.
func (s *ConfigStore) CommitOAuthLogin(ctx context.Context, scope Scope, authorized AuthorizedOAuthPreparation) (AuthenticationMutationResult, error) {
	a := authorized.state
	if a == nil || a.preparation == nil || a.preparation.store != s {
		return AuthenticationMutationResult{}, errors.New("matching authorized OAuth preparation is required")
	}
	if err := ctx.Err(); err != nil {
		return AuthenticationMutationResult{}, err
	}
	a.scopeMu.Lock()
	if a.scopeSet && a.scope != scope {
		a.scopeMu.Unlock()
		return AuthenticationMutationResult{}, errors.New("OAuth login commit scope differs from its original attempt")
	}
	a.scope, a.scopeSet = scope, true
	a.scopeMu.Unlock()
	return a.commit.execute(ctx, func() (AuthenticationMutationResult, error) {
		result, err := s.commitOAuthLogin(ctx, scope, a)
		return result, oauthLoginFailure("commit", err)
	})
}

func (s *ConfigStore) commitOAuthLogin(ctx context.Context, scope Scope, authorized *authorizedOAuthPreparation) (result AuthenticationMutationResult, err error) {
	p := authorized.preparation
	before, owner := p.before, p.owner
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return result, err
	}
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	admitted, err := s.authenticationAdmissionLocked(ctx, scope, before, owner)
	var journal *localAuthenticationWriter
	if err == nil {
		ctx, journal, err = s.beginLocalAuthenticationChangeLocked(ctx, before, admitted, owner, "oauth-login", "", "")
	}
	s.writeMu.Unlock()
	defer func() { journal.finish(ctx, result, &err) }()
	if err != nil {
		return result, err
	}
	digest := sha256.Sum256([]byte(owner.ProviderID))
	refreshPath := filepath.Join(filepath.Dir(admitted.globalPath), "locks", fmt.Sprintf("%x.refresh.lock", digest))
	if err := os.MkdirAll(filepath.Dir(refreshPath), 0o700); err != nil {
		return result, authenticationInputError(err)
	}
	lockCtx, cancelLock := context.WithTimeout(ctx, refreshLockDeadline)
	releaseRefresh, err := lock.File(lockCtx, refreshPath)
	cancelLock()
	if err != nil {
		return result, authenticationInputError(err)
	}
	defer releaseRefresh()
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	defer func() {
		s.writeMu.Unlock()
		if result.RuntimePublished {
			s.SignalAuthComplete(owner)
		}
	}()
	if err := s.verifyAuthenticationCollectionLocked(ctx, before); err != nil {
		return result, err
	}
	if !reflect.DeepEqual(p.settings, s.checkedAPIKeySettingsLocked(before)) {
		return result, errors.New("OAuth login settings changed")
	}
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	layers := p.layers
	if err := before.validateConfigBasis(layers, ""); err != nil {
		return result, err
	}
	projected := layers
	projected.order = slices.Clone(admitted.topology.inputs.order)
	projected.values = maps.Clone(layers.values)
	for _, file := range admitted.topology.inputs.files {
		if _, found := projected.values[file.path]; !found && slices.Contains(projected.order, file.path) {
			projected.values[file.path] = slices.Clone(file.data)
		}
	}
	provider := authorized.provider
	edit, err := projected.stageCredentialsAtPaths(ctx, admitted.path, owner.ProviderID, &provider, admitted.topology.writtenPaths)
	if err != nil {
		return result, err
	}
	if err := validateAuthenticationTopologyEffect(layers, edit, owner.ProviderID, &provider, admitted.topology.writtenPaths); err != nil {
		return result, err
	}
	next, err := authenticationConfigCandidate(before, owner, &provider)
	if err != nil {
		return result, err
	}
	delete(next.authenticationRevocations, owner.ProviderID)
	next.authenticationBasis = next.authenticationBasis.clone()
	if next.authenticationBasis == nil {
		return result, errAuthenticationBasisUnavailable
	}
	next.authenticationBasis.order = slices.Clone(admitted.topology.inputs.order)
	for _, written := range admitted.topology.writtenPaths {
		if _, found := next.authenticationBasis.sources[written]; !found {
			if !slices.Contains(admitted.topology.inputs.order, written) {
				continue
			}
			file, found := admitted.topology.inputs.file(written)
			if !found {
				return result, errAuthenticationBasisUnavailable
			}
			next.authenticationBasis.sources[written] = authenticationBasisSource{exists: file.info.exists, raw: slices.Clone(file.data), evaluated: slices.Clone(file.data)}
		}
		next.advanceAuthenticationBasisCredentials(written, owner.ProviderID, &provider)
	}
	if !next.authenticationBasis.valid {
		return result, errAuthenticationBasisUnavailable
	}
	if owner.AccountNamespace == "" {
		// An OAuth owner without accounts still retains the other owners' exact
		// account observations; nil here is not a logout of its new credential.
		err = before.finalizeRuntimeAuthenticationAuthority(next, owner, nil)
	} else {
		err = before.finalizeRuntimeAuthenticationAccounts(next, owner, authorized.entry)
	}
	if err != nil {
		return result, err
	}
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return result, err
	}
	prospective := s.runtimeSnapshotLocked(next, before.runtime.resolver, before.runtime.registry, before.runtime.environment)
	s.configMu.Unlock()
	candidate, err := s.prepareRuntimeGeneration(ctx, prospective)
	if err != nil {
		return result, err
	}
	committedRuntime := false
	defer func() {
		if !committedRuntime && candidate.Abort != nil {
			candidate.Abort()
		}
	}()
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	validate := func() error { return s.validateAuthenticationPublication(before, owner) }
	var pending *accounts.PendingChange
	if owner.AccountNamespace == "" {
		pending, err = before.accounts.BeginCheck(ctx)
	} else {
		if authorized.entry == nil {
			return result, errors.New("authorized OAuth login has no captured account")
		}
		pending, err = before.accounts.BeginImport(ctx, owner.AccountNamespace, *authorized.entry, validate)
	}
	if err != nil {
		return result, err
	}
	defer pending.Close()
	if authorized.entry != nil {
		actual, ok := pending.SelectedEntry()
		if !ok || !oauthLoginAccountsEqual(actual, *authorized.entry) {
			return result, errors.New("authorized OAuth account changed during staging")
		}
	}
	if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	release, err := lockAuthenticationScopes(ctx, admitted)
	if err != nil {
		return result, err
	}
	defer release()
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	preimage, _ := before.inputs.file(admitted.path)
	staged, err := stageAuthenticationScopeWrite(ctx, preimage, edit)
	if err != nil {
		return result, err
	}
	defer staged.Close()
	if err := journal.stage(ctx, staged, admitted.topology, pending); err != nil {
		return result, err
	}
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	committed, err := pending.Commit(ctx)
	result.AccountsSaved = committed.Written
	if err != nil {
		return result, err
	}
	finish := ctx
	if committed.Written {
		var cancel context.CancelFunc
		finish, cancel = context.WithDeadline(context.WithoutCancel(ctx), committed.CompletionDeadline())
		defer cancel()
	}
	if err := s.validateAuthenticationAdmissionLocked(finish, scope, before, owner, admitted); err != nil {
		return result, err
	}
	post, written, err := staged.commit(finish, committed.CompletionDeadline())
	result.ConfigSaved = written
	err = errors.Join(err, journal.configSaved(ctx, written, staged.completionDeadline))
	if err != nil {
		return result, err
	}
	if !committed.Written {
		var cancel context.CancelFunc
		finish, cancel = context.WithDeadline(context.WithoutCancel(ctx), staged.completionDeadline)
		defer cancel()
	}
	finalInputs, err := s.verifyAuthenticationPostimagesLocked(finish, before, admitted.topology, post)
	if err != nil {
		return result, err
	}
	accountAfter, err := pending.VerifyCommitted(finish)
	if err != nil {
		return result, err
	}
	if err := validate(); err != nil {
		return result, err
	}
	if err := lockAuthenticationMutex(finish, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return result, err
	}
	registerConfigSecrets(next)
	s.publishConfigLocked(next)
	afterRuntime := s.runtimeSnapshotLocked(next, before.runtime.resolver, before.runtime.registry, before.runtime.environment)
	s.configMu.Unlock()
	if candidate.Commit != nil {
		candidate.Commit()
	}
	committedRuntime, result.RuntimePublished = true, true
	verifiedAccounts, err := pending.VerifyCommitted(finish)
	if err != nil {
		return result, err
	}
	verifiedInputs, err := s.captureAuthenticationInputsLocked(finish, afterRuntime)
	if err != nil {
		return result, err
	}
	if !accountAfter.SameObservation(verifiedAccounts) || !finalInputs.sameObservation(verifiedInputs) {
		return result, errors.New("OAuth login state changed after local publication")
	}
	finalAccounts, err := pending.VerifyCommitted(finish)
	if err != nil {
		return result, err
	}
	if !verifiedAccounts.SameObservation(finalAccounts) {
		return result, errors.New("OAuth login account changed after local publication")
	}
	s.recordAuthenticationWatcherState(verifiedInputs)
	result.After = AuthenticationCapture{runtime: afterRuntime, accounts: finalAccounts, owners: slices.Clone(before.owners), inputs: verifiedInputs}
	return result, nil
}

func oauthLoginAccountsEqual(a, b accounts.Entry) bool {
	return a.ID == b.ID && a.DisplayName == b.DisplayName && a.AccessToken == b.AccessToken &&
		a.RefreshToken == b.RefreshToken && a.ExpiresAt == b.ExpiresAt && authenticationMetadataEqual(a.Raw, b.Raw)
}
