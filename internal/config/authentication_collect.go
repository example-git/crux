package config

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

// CollectRemoteRuntimeForAuthentication collects the exact local observation
// returned by an authentication transaction. It neither refreshes accounts nor
// adopts a later configuration/account observation. The caller must separately
// publish and acknowledge this proposal on the execution host.
func (s *ConfigStore) CollectRemoteRuntimeForAuthentication(ctx context.Context, before AuthenticationCapture, revision uint64, removed map[providerregistry.RegistrationOwner]bool) (RemoteRuntimeProposal, error) {
	if before.runtime.IsClientOwned() {
		return RemoteRuntimeProposal{}, ErrClientRuntimeManaged
	}
	if before.runtime.publicationStore != s || !before.inputs.valid || revision == 0 {
		return RemoteRuntimeProposal{}, errors.New("authentication collection requires its exact owning store capture")
	}
	if err := before.runtime.prepareNativeIdentities(ctx); err != nil {
		return RemoteRuntimeProposal{}, err
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return RemoteRuntimeProposal{}, err
	}
	defer s.writeMu.RUnlock()
	if err := s.verifyAuthenticationCollectionLocked(ctx, before); err != nil {
		return RemoteRuntimeProposal{}, err
	}
	resolve := func(value string) (string, error) {
		resolver, ok := before.runtime.resolver.(contextVariableResolver)
		if !ok {
			return "", errors.New("authentication collection resolver does not support cancellation")
		}
		return resolver.ResolveValueContext(ctx, value)
	}
	proposal, err := collectRemoteRuntime(ctx, before.runtime, revision, maps.Clone(removed), resolve, func(_ context.Context, owner providerregistry.RegistrationOwner) (*accounts.Entry, error) {
		if !slices.Contains(before.owners, owner) {
			return nil, errors.New("authentication collection owner was not captured")
		}
		active := before.accounts.ActiveID(owner.AccountNamespace)
		for _, entry := range before.accounts.Entries(owner.AccountNamespace) {
			if entry.ID == active {
				return &entry, nil
			}
		}
		return nil, nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return RemoteRuntimeProposal{}, ctx.Err()
		}
		return RemoteRuntimeProposal{}, err
	}
	if err := s.verifyAuthenticationCollectionLocked(ctx, before); err != nil {
		return RemoteRuntimeProposal{}, err
	}
	return proposal, nil
}

// Hold writeMu for reading, but take an account lease only for verification.
// Ordinary configured key/context expressions run between these checks without
// account locks. The lease and final input check cannot bless changed state.
func (s *ConfigStore) verifyAuthenticationCollectionLocked(ctx context.Context, before AuthenticationCapture) error {
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return err
	}
	current := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	if !before.runtime.SamePublication(current) {
		return errors.New("authentication publication changed before collection")
	}
	check, err := before.accounts.BeginCheck(ctx)
	if err != nil {
		return err
	}
	defer check.Close()
	committed, err := check.Commit(ctx)
	if err != nil {
		return err
	}
	inputs, err := s.captureAuthenticationInputsLocked(ctx, current)
	if err != nil {
		return err
	}
	verified, err := check.VerifyCommitted(ctx)
	if err != nil {
		return err
	}
	if !before.accounts.SameObservation(committed.Snapshot) || !before.accounts.SameObservation(verified) || !before.inputs.sameObservation(inputs) {
		return errors.New("authentication inputs changed before collection")
	}
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return err
	}
	current = s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	if !before.runtime.SamePublication(current) {
		return errors.New("authentication publication changed during collection")
	}
	return ctx.Err()
}
