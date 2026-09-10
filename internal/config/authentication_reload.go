package config

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/example-git/crux/internal/providerregistry"
)

// ReloadAuthenticationFromDisk admits one explicit reload against the current
// publication and complete owner. Saved files may have changed: that is the
// input this action accepts. It never refreshes or repeats a credential mutation.
// The bool records local publication even if the final observation fails.
func (s *ConfigStore) ReloadAuthenticationFromDisk(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner) (AuthenticationCapture, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if before.runtime.IsClientOwned() || before.runtime.publicationStore != s {
		return AuthenticationCapture{}, false, ErrClientRuntimeManaged
	}
	if s.workingDir == "" {
		return AuthenticationCapture{}, false, errors.New("cannot reload authentication: working directory is unavailable")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock); err != nil {
		return AuthenticationCapture{}, false, err
	}
	defer s.writeMu.Unlock()
	if err := lockAuthenticationMutex(ctx, s.configMu.TryLock, s.configMu.Unlock); err != nil {
		return AuthenticationCapture{}, false, err
	}
	current := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	if current.IsClientOwned() {
		return AuthenticationCapture{}, false, ErrClientRuntimeManaged
	}
	expectedEnv, currentEnv := before.runtime.Environment(), current.Environment()
	slices.Sort(expectedEnv)
	slices.Sort(currentEnv)
	actual, active := current.ProviderOwner(owner.ProviderID)
	if !before.runtime.SamePublication(current) || !slices.Equal(expectedEnv, currentEnv) || !active || actual != owner || !slices.Contains(before.owners, owner) {
		return AuthenticationCapture{}, false, ErrAuthenticationReconciliationConflict
	}
	if err := s.reloadFromDiskWithCredentialCaptureLocked(ctx, true); err != nil {
		return AuthenticationCapture{}, false, err
	}
	after, err := s.captureAuthenticationLocked(ctx)
	return after, true, err
}

// A temporary wrapper used only during explicit reload configuration. The
// long-lived store keeps the underlying resolver, never this canceled context.
type authenticationReloadResolver struct {
	ctx      context.Context
	resolver VariableResolver
}

func (r authenticationReloadResolver) ResolveValue(source string) (string, error) {
	return r.ResolveValueContext(r.ctx, source)
}

func (r authenticationReloadResolver) ResolveValueContext(ctx context.Context, source string) (string, error) {
	if err := r.ctx.Err(); err != nil {
		return "", err
	}
	resolver, ok := r.resolver.(contextVariableResolver)
	if !ok {
		return "", errors.New("authentication reload resolver does not support cancellation")
	}
	bound, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(r.ctx, cancel)
	defer func() { stop(); cancel() }()
	return resolver.ResolveValueContext(bound, source)
}
