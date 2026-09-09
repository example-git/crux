package config

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"

	"github.com/example-git/crux/internal/csync"
)

// SaveCheckedAPIKey persists only the entered source and exact owner references.
// The checked literal and endpoint are published in memory, with no account
// mutation, source execution, connection probe, model selection or agent reset.
func (s *ConfigStore) SaveCheckedAPIKey(ctx context.Context, scope Scope, prepared CheckedAPIKeyPreparation) (result AuthenticationMutationResult, err error) {
	if !prepared.valid || prepared.before.runtime.publicationStore != s || !checkedProviderCredentialMatches(prepared.provider, prepared.slot) || !prepared.provider.resolvedEndpoint.matches(prepared.provider) {
		return result, errors.New("valid checked API key preparation is required")
	}
	before, owner := prepared.before, prepared.owner
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
	if !reflect.DeepEqual(prepared.settings, s.checkedAPIKeySettingsLocked(before)) {
		return result, errors.New("checked provider settings changed")
	}
	admitted, err := s.authenticationAdmissionLocked(ctx, scope, before, owner)
	if err != nil {
		return result, err
	}
	ctx, journal, err := s.beginLocalAuthenticationChangeLocked(ctx, before, admitted, owner, "api-key", "", "")
	if err != nil {
		return result, err
	}
	defer func() { journal.finish(ctx, result, &err) }()
	if journal != nil {
		effect, effectErr := prepared.ConfiguredCredentialEffectID()
		if effectErr != nil {
			return result, effectErr
		}
		journal.capture.disk.CredentialEffectID = effect
		if err := journal.save(ctx); err != nil {
			return result, err
		}
	}
	layers := prepared.layers
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
	edit, err := projected.stageCheckedAPIKey(ctx, admitted.path, prepared.provider, admitted.topology.writtenPaths, prepared.slot)
	if err != nil {
		return result, err
	}
	if err := validateCheckedCredentialTopology(layers, edit, prepared.provider, admitted.topology.writtenPaths, prepared.slot); err != nil {
		return result, err
	}
	next := before.runtime.config.cloneForWrite()
	if next.Providers == nil {
		next.Providers = csync.NewMap[string, ProviderConfig]()
	}
	next.Providers.Set(owner.ProviderID, cloneProviderConfig(prepared.provider))
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
		next.advanceAuthenticationBasisCheckedAPIKey(written, prepared.provider, prepared.slot)
	}
	if !next.authenticationBasis.valid {
		return result, errAuthenticationBasisUnavailable
	}
	finalize := func() error {
		if prepared.slot.Property != "" {
			return before.finalizeRuntimeConfigurationCredential(next, owner, prepared.slot)
		}
		return before.finalizeRuntimeCheckedAPIKey(next, owner)
	}
	if err := finalize(); err != nil {
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
	pending, err := before.accounts.BeginCheck(ctx)
	if err != nil {
		return result, err
	}
	defer pending.Close()
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
	accountsBefore, err := pending.Commit(ctx)
	if err != nil {
		return result, err
	}
	if accountsBefore.Written || !before.accounts.SameObservation(accountsBefore.Snapshot) {
		return result, errors.New("checked provider account observation changed")
	}
	post, written, err := staged.Commit(ctx)
	result.ConfigSaved = written
	err = errors.Join(err, journal.configSaved(ctx, written, staged.completionDeadline))
	if err != nil {
		return result, err
	}
	finish, cancel := context.WithDeadline(context.WithoutCancel(ctx), staged.completionDeadline)
	defer cancel()
	finalInputs, err := s.verifyAuthenticationPostimagesLocked(finish, before, admitted.topology, post)
	if err != nil {
		return result, err
	}
	accountAfter, err := pending.VerifyCommitted(finish)
	if err != nil {
		return result, err
	}
	if !before.accounts.SameObservation(accountAfter) {
		return result, errors.New("checked provider account observation changed")
	}
	if err := s.validateAuthenticationPublication(before, owner); err != nil {
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
	if !before.accounts.SameObservation(verifiedAccounts) || !finalInputs.sameObservation(verifiedInputs) {
		return result, errors.New("checked provider state changed after local publication")
	}
	finalAccounts, err := pending.VerifyCommitted(finish)
	if err != nil {
		return result, err
	}
	if !before.accounts.SameObservation(finalAccounts) {
		return result, errors.New("checked provider account observation changed after publication")
	}
	s.recordAuthenticationWatcherState(verifiedInputs)
	result.After = AuthenticationCapture{runtime: afterRuntime, accounts: finalAccounts, owners: slices.Clone(before.owners), inputs: verifiedInputs}
	return result, nil
}
