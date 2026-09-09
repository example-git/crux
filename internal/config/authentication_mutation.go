package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/lock"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

// AuthenticationMutationResult is private local progress. Remote publication
// acknowledgement is a separate operation. After is valid only on full success;
// flags retain durable effects even when a later step fails.
type AuthenticationMutationResult struct {
	After                                                          AuthenticationCapture
	AccountRefreshed, AccountsSaved, ConfigSaved, RuntimePublished bool
	accountOnly                                                    bool
}

func (AuthenticationMutationResult) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication mutation results are private")
}
func (AuthenticationMutationResult) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private authentication mutation result]"))
}
func (r AuthenticationMutationResult) RuntimeSnapshot() (RuntimeSnapshot, bool) {
	return r.After.runtime, (r.RuntimePublished || r.accountOnly && r.AccountsSaved) && r.After.inputs.valid && r.After.runtime.publicationStore != nil
}

type authenticationAdmission struct {
	topology                                    authenticationScopeTopology
	path, globalPath, workspacePath, workingDir string
	base                                        env.Env
	ephemeral                                   map[string]ProviderConfig
	overrides                                   RuntimeOverrides
}

func (authenticationAdmission) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication admission is private")
}
func (authenticationAdmission) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private authentication admission]"))
}

func (s *ConfigStore) SwitchAuthenticationAccount(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner, accountID string) (AuthenticationMutationResult, error) {
	if accountID == "" || owner.AccountNamespace == "" || !owner.HasOAuth {
		return AuthenticationMutationResult{}, errors.New("authentication switch requires an exact OAuth account")
	}
	return s.mutateAuthentication(ctx, scope, before, owner, accountID)
}

func (s *ConfigStore) LogoutAuthentication(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner) (AuthenticationMutationResult, error) {
	return s.mutateAuthentication(ctx, scope, before, owner, "")
}

func (s *ConfigStore) lockAuthenticationWrite(ctx context.Context) error {
	return lockAuthenticationMutex(ctx, s.writeMu.TryLock, s.writeMu.Unlock)
}
func lockAuthenticationMutex(ctx context.Context, try func() bool, unlock func()) error {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if try() {
			if err := ctx.Err(); err != nil {
				unlock()
				return err
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// validateAuthenticationPublication uses only immutable captured authority and
// configMu; it is safe as a validator during an account refresh's held lease.
func (s *ConfigStore) validateAuthenticationPublication(before AuthenticationCapture, owner providerregistry.RegistrationOwner) error {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	if before.runtime.IsClientOwned() {
		return ErrClientRuntimeManaged
	}
	if before.runtime.config == nil || before.runtime.publicationStore != s || before.runtime.publicationSequence == 0 || before.runtime.publicationSequence != s.publicationSequence || before.runtime.config != s.config {
		return errors.New("authentication configuration publication changed")
	}
	if !slices.Contains(before.owners, owner) {
		return errors.New("authentication provider owner changed")
	}
	actual, ok := providerOwnerForConfig(s.config, before.runtime.registry, owner.ProviderID)
	if !ok || actual != owner {
		return errors.New("authentication provider owner changed")
	}
	if s.publicationSequence == ^uint64(0) {
		return errors.New("authentication configuration publication sequence exhausted")
	}
	return nil
}

func (s *ConfigStore) authenticationAdmissionLocked(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner) (authenticationAdmission, error) {
	if err := s.validateAuthenticationPublication(before, owner); err != nil {
		return authenticationAdmission{}, err
	}
	path, err := s.configPath(scope)
	if err != nil {
		return authenticationAdmission{}, err
	}
	if !filepath.IsAbs(path) || !filepath.IsAbs(s.globalDataPath) {
		return authenticationAdmission{}, errors.New("authentication configuration paths must be absolute")
	}
	path = filepath.Clean(path)
	if _, found := before.inputs.file(path); !found || !before.inputs.valid {
		return authenticationAdmission{}, errors.New("authentication writable scope was not captured")
	}
	base := s.baseEnvironment
	if base == nil {
		base = before.runtime.environment
	}
	admitted := authenticationAdmission{path: path, globalPath: s.globalDataPath, workspacePath: s.workspacePath, workingDir: s.workingDir, base: cloneEnvironment(base), overrides: cloneRuntimeOverrides(s.overrides), ephemeral: map[string]ProviderConfig{}}
	for id, provider := range s.ephemeralProviderConfigs {
		admitted.ephemeral[id] = cloneProviderConfig(provider)
	}
	topology, err := prepareAuthenticationScopeTopology(ctx, before.runtime, before.inputs, path, admitted.workingDir, admitted.workspacePath, admitted.base)
	if err != nil {
		return authenticationAdmission{}, err
	}
	for _, written := range topology.writtenPaths {
		if isShellConfig(written) {
			return authenticationAdmission{}, errors.New("authentication JSON scope is also a shell configuration source; use separate files for JSON and shell inputs")
		}
	}
	admitted.topology = topology
	return admitted, nil
}

func (s *ConfigStore) validateAuthenticationAdmissionLocked(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner, admitted authenticationAdmission) error {
	current, err := s.authenticationAdmissionLocked(ctx, scope, before, owner)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, admitted) {
		return errors.New("authentication captured settings changed")
	}
	inputs, err := s.captureAuthenticationInputsLocked(ctx, before.runtime)
	if err != nil {
		return err
	}
	if !before.inputs.sameObservation(inputs) {
		return errAuthenticationInputsChanged
	}
	return nil
}

func (s *ConfigStore) mutateAuthentication(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner, accountID string) (result AuthenticationMutationResult, err error) {
	return s.mutateAuthenticationChange(ctx, scope, before, owner, accountID, "")
}

func (s *ConfigStore) mutateAuthenticationChange(ctx context.Context, scope Scope, before AuthenticationCapture, owner providerregistry.RegistrationOwner, accountID, removeID string) (result AuthenticationMutationResult, err error) {
	ctx, releaseOperation, err := s.acquireLocalAuthenticationOperation(ctx)
	if err != nil {
		return result, err
	}
	defer releaseOperation()
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	admitted, err := s.authenticationAdmissionLocked(ctx, scope, before, owner)
	var journal *localAuthenticationWriter
	if err == nil {
		action := "logout"
		if accountID != "" {
			action = "switch"
		}
		if removeID != "" {
			action = "remove"
		}
		ctx, journal, err = s.beginLocalAuthenticationChangeLocked(ctx, before, admitted, owner, action, accountID, removeID)
	}
	s.writeMu.Unlock()
	defer func() { journal.finish(ctx, result, &err) }()
	if err != nil {
		return result, err
	}
	layers, err := evaluateAuthenticationLayers(ctx, before.inputs, admitted.base, admitted.ephemeral, admitted.overrides)
	if err != nil {
		return result, err
	}
	if err := before.validateConfigBasis(layers, owner.ProviderID); err != nil {
		return result, err
	}
	// Use the same captured provider lock path as existing refresh writers, but
	// require acquisition: authentication changes never proceed unlocked.
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
	err = s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted)
	s.writeMu.Unlock()
	if err != nil {
		return result, err
	}
	validate := func() error { return s.validateAuthenticationPublication(before, owner) }
	if err := validate(); err != nil {
		return result, err
	}
	var desired *ProviderConfig
	var selected *accounts.Entry
	if accountID != "" {
		registration, err := authenticationRegistration(before, owner)
		if err != nil {
			return result, err
		}
		if registration.OAuth == nil {
			return result, errors.New("authentication provider does not support OAuth")
		}
		refreshed, err := before.accounts.RefreshInactive(ctx, owner.AccountNamespace, accountID, journal.refresher(registration.OAuth.Refresh), validate)
		result.AccountRefreshed = refreshed.Written
		if err != nil {
			return result, err
		}
		if refreshed.Entry.AccessToken == "" {
			return result, errors.New("selected authentication account has no access token; sign in again")
		}
		before.accounts = refreshed.Snapshot
		for _, entry := range before.accounts.Entries(owner.AccountNamespace) {
			if entry.ID == accountID {
				selected = &entry
				break
			}
		}
		if selected == nil || selected.AccessToken != refreshed.Entry.AccessToken || selected.RefreshToken != refreshed.Entry.RefreshToken || selected.ExpiresAt != refreshed.Entry.ExpiresAt {
			return result, errors.New("refreshed authentication account has no matching successor observation")
		}
		provider, err := prepareAuthenticationProvider(ctx, before, owner, selected.Token())
		if err != nil {
			return result, err
		}
		desired = &provider
	}
	preparedLayers := layers
	preparedLayers.order = slices.Clone(admitted.topology.inputs.order)
	preparedLayers.values = maps.Clone(layers.values)
	for _, file := range admitted.topology.inputs.files {
		if _, exists := preparedLayers.values[file.path]; !exists && slices.Contains(preparedLayers.order, file.path) {
			preparedLayers.values[file.path] = slices.Clone(file.data)
		}
	}
	edit, err := preparedLayers.stageCredentialsAtPaths(ctx, admitted.path, owner.ProviderID, desired, admitted.topology.writtenPaths)
	if err != nil {
		return result, err
	}
	if err := validateAuthenticationTopologyEffect(layers, edit, owner.ProviderID, desired, admitted.topology.writtenPaths); err != nil {
		return result, err
	}
	if desired == nil {
		if err := validateAuthenticationLogoutFallback(ctx, before.runtime, edit.after, owner); err != nil {
			return result, err
		}
	}
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	defer func() {
		s.writeMu.Unlock()
		if result.RuntimePublished {
			s.SignalAuthComplete(owner)
		}
	}()
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	if err := before.validateConfigBasis(layers, owner.ProviderID); err != nil {
		return result, err
	}
	next, err := authenticationConfigCandidate(before, owner, desired)
	if err != nil {
		return result, err
	}
	next.authenticationBasis = next.authenticationBasis.clone()
	if next.authenticationBasis == nil {
		return result, errAuthenticationBasisUnavailable
	}
	next.authenticationBasis.order = slices.Clone(admitted.topology.inputs.order)
	for _, written := range admitted.topology.writtenPaths {
		if _, exists := next.authenticationBasis.sources[written]; !exists {
			if !slices.Contains(admitted.topology.inputs.order, written) {
				continue
			}
			file, found := admitted.topology.inputs.file(written)
			if !found {
				return result, errAuthenticationBasisUnavailable
			}
			next.authenticationBasis.sources[written] = authenticationBasisSource{exists: file.info.exists, raw: slices.Clone(file.data), evaluated: slices.Clone(file.data)}
		}
		next.advanceAuthenticationBasisCredentials(written, owner.ProviderID, desired)
	}
	if next.authenticationBasis == nil || !next.authenticationBasis.valid {
		return result, errAuthenticationBasisUnavailable
	}
	if err := before.finalizeRuntimeAuthenticationAccounts(next, owner, selected); err != nil {
		return result, err
	}
	// Freeze next before invoking the preparer: asynchronous prompt/tool readers
	// and eventual publication all retain exactly this Config pointer.
	s.configMu.Lock()
	prospective := s.runtimeSnapshotLocked(next, before.runtime.resolver, before.runtime.registry, before.runtime.environment)
	s.configMu.Unlock()
	runtimeCandidate, err := s.prepareRuntimeGeneration(ctx, prospective)
	if err != nil {
		return result, err
	}
	runtimeCommitted := false
	defer func() {
		if !runtimeCommitted && runtimeCandidate.Abort != nil {
			runtimeCandidate.Abort()
		}
	}()
	if err := s.validateAuthenticationAdmissionLocked(ctx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	var pending *accounts.PendingChange
	switch {
	case removeID != "":
		pending, err = before.accounts.BeginRemove(ctx, owner.AccountNamespace, removeID)
	case selected != nil:
		pending, err = before.accounts.BeginSwitch(ctx, owner.AccountNamespace, accountID)
	case owner.AccountNamespace != "":
		pending, err = before.accounts.BeginLogout(ctx, owner.AccountNamespace)
	default:
		pending, err = before.accounts.BeginCheck(ctx)
	}
	if err != nil {
		return result, err
	}
	defer pending.Close()
	if selected != nil {
		actual, ok := pending.SelectedEntry()
		if !ok || !reflect.DeepEqual(actual, *selected) {
			return result, errors.New("selected authentication account changed during preparation")
		}
	}
	if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	releaseScopes, err := lockAuthenticationScopes(ctx, admitted)
	if err != nil {
		return result, err
	}
	defer releaseScopes()
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
	if err := ctx.Err(); err != nil {
		return result, err
	}
	committed, err := pending.Commit(ctx)
	result.AccountsSaved = committed.Written
	if err != nil {
		return result, err
	}
	commitCtx := ctx
	cancelCompletion := func() {}
	if result.AccountsSaved {
		commitCtx, cancelCompletion = context.WithDeadline(context.WithoutCancel(ctx), committed.CompletionDeadline())
	}
	defer cancelCompletion()
	if err := s.validateAuthenticationAdmissionLocked(commitCtx, scope, before, owner, admitted); err != nil {
		return result, err
	}
	postimage, written, err := staged.commit(commitCtx, committed.CompletionDeadline())
	result.ConfigSaved = written
	err = errors.Join(err, journal.configSaved(ctx, written, staged.completionDeadline))
	if err != nil {
		return result, err
	}
	if !result.AccountsSaved {
		commitCtx, cancelCompletion = context.WithDeadline(context.WithoutCancel(ctx), staged.completionDeadline)
		defer cancelCompletion()
	}
	finalInputs, err := s.verifyAuthenticationPostimagesLocked(commitCtx, before, admitted.topology, postimage)
	if err != nil {
		return result, err
	}
	accountAfter, err := pending.VerifyCommitted(commitCtx)
	if err != nil {
		return result, err
	}
	if err := validate(); err != nil {
		return result, err
	}
	if err := commitCtx.Err(); err != nil {
		return result, err
	}
	registerConfigSecrets(next)
	s.configMu.Lock()
	s.publishConfigLocked(next)
	afterRuntime := s.runtimeSnapshotLocked(next, before.runtime.resolver, before.runtime.registry, before.runtime.environment)
	s.configMu.Unlock()
	if runtimeCandidate.Commit != nil {
		runtimeCandidate.Commit()
	}
	runtimeCommitted = true
	result.RuntimePublished = true
	// Detect observed out-of-protocol writes without authorizing a fresh capture.
	verifiedAccounts, err := pending.VerifyCommitted(commitCtx)
	if err != nil {
		return result, err
	}
	verifiedInputs, err := s.captureAuthenticationInputsLocked(commitCtx, afterRuntime)
	if err != nil {
		return result, err
	}
	if !accountAfter.SameObservation(verifiedAccounts) || !finalInputs.sameObservation(verifiedInputs) {
		return result, errors.New("authentication state changed after local publication")
	}
	finalAccounts, err := pending.VerifyCommitted(commitCtx)
	if err != nil {
		return result, err
	}
	if !verifiedAccounts.SameObservation(finalAccounts) {
		return result, errors.New("authentication account state changed after local publication")
	}
	s.recordAuthenticationWatcherState(verifiedInputs)
	result.After = AuthenticationCapture{runtime: afterRuntime, accounts: finalAccounts, owners: slices.Clone(before.owners), inputs: verifiedInputs}
	return result, nil
}

func lockAuthenticationScopes(ctx context.Context, admitted authenticationAdmission) (func(), error) {
	paths := []string{}
	identities := []os.FileInfo{}
	for _, path := range []string{admitted.globalPath, admitted.workspacePath} {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			return nil, errors.New("authentication scope path must be absolute")
		}
		// Resolve the lock entry, not the config leaf: a config leaf symlink
		// is replaced by rename and therefore still needs its own scope lock.
		path = filepath.Clean(path) + ".lock"
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, authenticationInputError(err)
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return nil, authenticationInputError(err)
		}
		info, statErr := file.Stat()
		closeErr := file.Close()
		if err := errors.Join(statErr, closeErr); err != nil {
			return nil, authenticationInputError(err)
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, authenticationInputError(err)
		}
		duplicate := slices.Contains(paths, resolved)
		for _, previous := range identities {
			duplicate = duplicate || os.SameFile(previous, info)
		}
		if !duplicate {
			paths = append(paths, resolved)
			identities = append(identities, info)
		}
	}
	slices.Sort(paths)
	releases := []func(){}
	release := func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}
	bounded, cancel := context.WithTimeout(ctx, configLockDeadline)
	defer cancel()
	for _, path := range paths {
		unlock, err := lock.File(bounded, path)
		if err != nil {
			release()
			return nil, authenticationInputError(err)
		}
		releases = append(releases, unlock)
	}
	return release, nil
}

func (s *ConfigStore) verifyAuthenticationPostimagesLocked(ctx context.Context, before AuthenticationCapture, topology authenticationScopeTopology, post authenticationInputFile) (authenticationConfigInputs, error) {
	actual, err := s.captureAuthenticationInputsLocked(ctx, before.runtime)
	if err != nil {
		return authenticationConfigInputs{}, err
	}
	expected := topology.inputs
	expected.files = slices.Clone(expected.files)
	for i, file := range expected.files {
		if slices.Contains(topology.writtenPaths, file.path) {
			expected.files[i] = post
			expected.files[i].path = file.path
		}
	}
	if !expected.sameObservation(actual) {
		return authenticationConfigInputs{}, errAuthenticationInputsChanged
	}
	return actual, nil
}

func (s *ConfigStore) recordAuthenticationWatcherState(inputs authenticationConfigInputs) {
	s.trackedConfigPaths = make([]string, 0, len(inputs.files))
	s.snapshots = make(map[string]fileSnapshot, len(inputs.files))
	for _, file := range inputs.files {
		s.trackedConfigPaths = append(s.trackedConfigPaths, file.path)
		s.snapshots[file.path] = fileSnapshot{Path: file.path, Exists: file.info.exists, Size: file.info.size, ModTime: file.info.modified}
	}
	slices.Sort(s.trackedConfigPaths)
}

// The intended discovery delta may expose the same authored file more than
// once. Permit that only when it preserves every noncredential effective field.
func validateAuthenticationTopologyEffect(original authenticationLayers, edit authenticationCredentialEdit, providerID string, desired *ProviderConfig, writtenPaths []string) error {
	replacements := make(map[string][]byte, len(writtenPaths))
	for _, path := range writtenPaths {
		replacements[path] = edit.data
	}
	baseline, err := original.mergedReplacements(replacements)
	if err != nil {
		return err
	}
	left, err := json.Marshal(baseline)
	if err != nil {
		return errors.New("authentication scope effect cannot be checked")
	}
	right, err := json.Marshal(edit.after)
	if err != nil {
		return errors.New("authentication scope effect cannot be checked")
	}
	fields := []string{"api_key", "oauth"}
	if desired != nil {
		fields = append(fields, "owner", "plugin", "preset")
	}
	for _, field := range fields {
		left, err = runtimeControlChangeField(left, []string{"providers", providerID, field}, nil, true)
		if err != nil {
			return errors.New("authentication scope effect cannot be checked")
		}
		right, err = runtimeControlChangeField(right, []string{"providers", providerID, field}, nil, true)
		if err != nil {
			return errors.New("authentication scope effect cannot be checked")
		}
	}
	if !RuntimeControlJSONEqual(left, right) {
		return errors.New("authentication scope creation would change unrelated effective configuration")
	}
	return nil
}
