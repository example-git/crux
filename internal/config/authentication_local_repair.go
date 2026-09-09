package config

import (
	"bytes"
	"context"
	"errors"
	"slices"

	"github.com/example-git/crux/internal/oauth/accounts"
)

// RepairAuthenticationLocalChange completes only recorded fixed disk writes.
// It never publishes a runtime, replays an exchange, or substitutes a fresh
// generation for the original operation. Callers explicitly reload afterward.
func (s *ConfigStore) RepairAuthenticationLocalChange(ctx context.Context, capture LocalAuthenticationChange, expectedRevision ...uint64) (result LocalAuthenticationRepairResult, err error) {
	result.Summary = capture.Summary()
	expected := capture.revision
	if len(expectedRevision) > 1 {
		return result, errors.New("one exact local repair revision is required")
	}
	if len(expectedRevision) == 1 {
		expected = expectedRevision[0]
	}
	if capture.journal.store != s || expected == 0 {
		return result, errors.New("matching private local repair capture is required")
	}
	ctx, cancel := s.BindRuntimeContext(ctx)
	defer cancel()
	if err := s.lockAuthenticationWrite(ctx); err != nil {
		return result, err
	}
	defer s.writeMu.Unlock()
	current, found, err := loadAuthenticationLocalChange(ctx, capture.journal, capture.disk.Key)
	if err != nil {
		return result, err
	}
	if !found {
		return result, errors.New("local authentication operation is unavailable")
	}
	d := current.disk
	if d.GlobalPath != s.globalDataPath || d.WorkspacePath != s.workspacePath || d.WorkingDir != s.workingDir {
		return result, errors.New("local authentication operation belongs to different captured configuration paths")
	}
	if current.revision != expected && d.RepairBase != expected {
		return result, errors.New("local authentication repair revision changed; review it again")
	}
	if d.Repair.NeedsReload {
		result = d.Repair
		result.Summary = current.Summary()
		return result, nil
	}
	result = d.Repair
	result.Summary = current.Summary()
	s.configMu.RLock()
	owner, known := providerOwnerForConfig(s.config, s.providerRegistry, d.Owner.ProviderID)
	s.configMu.RUnlock()
	if !known || owner != d.Owner {
		return result, errors.New("original authentication provider owner changed; review current saved state")
	}
	if d.RefreshStarted && d.RefreshToken == nil {
		return result, ErrLocalAuthenticationRefreshUnresolved
	}
	writer := &localAuthenticationWriter{capture: current}
	// If the response was retained before account staging, construct that one
	// successor from the original account preimage, without any network work.
	if len(d.RefreshAccounts) == 0 && d.RefreshToken != nil && len(d.ConfigAfter) == 0 {
		original, err := accounts.DecodeDurableChange(d.InitialAccounts)
		if err != nil {
			return result, err
		}
		refresh, err := original.WithObservedRefresh(d.Owner.AccountNamespace, d.AccountID, d.RefreshToken)
		if err != nil {
			return result, err
		}
		raw, err := refresh.Encode()
		if err != nil {
			return result, err
		}
		d.RefreshAccounts = raw
		writer.capture.disk.RefreshAccounts = raw
	}
	raw := d.Accounts
	if len(d.ConfigAfter) == 0 && len(d.RefreshAccounts) > 0 {
		raw = d.RefreshAccounts
	}
	if len(raw) == 0 {
		return result, errors.New("operation stopped before a fixed disk change was staged; reload and review saved state")
	}
	change, err := accounts.DecodeDurableChange(raw)
	if err != nil {
		return result, err
	}
	pending, post, err := change.BeginRepair(ctx)
	if err != nil {
		return result, err
	}
	defer pending.Close()
	if err := lockAuthenticationMutex(ctx, s.mu.TryLock, s.mu.Unlock); err != nil {
		return result, err
	}
	defer s.mu.Unlock()
	release, err := lockAuthenticationScopes(ctx, authenticationAdmission{globalPath: d.GlobalPath, workspacePath: d.WorkspacePath})
	if err != nil {
		return result, err
	}
	defer release()
	configPost, err := s.verifyLocalRepairInputsLocked(ctx, d)
	if err != nil {
		return result, err
	}
	var stage *authenticationScopeWrite
	if len(d.ConfigAfter) > 0 && !configPost {
		var before authenticationInputFile
		for _, file := range d.Inputs {
			if file.Path == d.ScopePath {
				before = file.input()
				break
			}
		}
		stage, err = stageAuthenticationScopeWrite(ctx, before, authenticationCredentialEdit{path: d.ScopePath, data: bytes.Clone(d.ConfigAfter)})
		if err != nil {
			return result, err
		}
		defer stage.Close()
	}
	// Persist repair admission before either write. The base revision permits
	// an exact lost-reply retry without admitting another historical operation.
	writer.capture.disk.RepairStarted = true
	writer.capture.disk.RepairBase = expected
	if err := writer.save(ctx); err != nil {
		return result, err
	}
	defer func() {
		writer.capture.disk.Repair = result
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), authenticationCompletionTimeout)
		defer stop()
		err = errors.Join(err, writer.save(finish))
		result.Summary = writer.capture.Summary()
	}()
	result.AccountsMatched = post
	result.ConfigMatched = configPost && len(d.ConfigAfter) > 0
	committed, err := pending.Commit(ctx)
	result.AccountsWritten = result.AccountsWritten || committed.Written
	if err != nil {
		return result, err
	}
	finish := ctx
	stop := func() {}
	if committed.Written {
		finish, stop = context.WithDeadline(context.WithoutCancel(ctx), committed.CompletionDeadline())
	}
	defer stop()
	if _, err := s.verifyLocalRepairInputsLocked(finish, d); err != nil {
		return result, err
	}
	if stage != nil {
		_, written, writeErr := stage.commit(finish, committed.CompletionDeadline())
		result.ConfigWritten = result.ConfigWritten || written
		if writeErr != nil {
			return result, writeErr
		}
		if !committed.Written {
			finish, stop = context.WithDeadline(context.WithoutCancel(ctx), stage.completionDeadline)
			defer stop()
		}
	}
	if len(d.ConfigAfter) > 0 {
		post, err := s.verifyLocalRepairInputsLocked(finish, d)
		if err != nil {
			return result, err
		}
		if !post {
			return result, errAuthenticationInputsChanged
		}
	}
	if _, err := pending.VerifyCommitted(finish); err != nil {
		return result, err
	}
	if err := s.RuntimeRevocation(); err != nil {
		return result, err
	}
	result.NeedsReload = true
	return result, nil
}

// The same recorded set of configuration inputs must still be present. Written
// aliases must all read either their original observation or the fixed postimage;
// unrelated inputs always require their original identity and bytes.
func (s *ConfigStore) verifyLocalRepairInputsLocked(ctx context.Context, d localAuthenticationDisk) (bool, error) {
	allPost := len(d.ConfigAfter) > 0
	allBefore := true
	for _, file := range d.Inputs {
		actual, err := readAuthenticationInput(ctx, file.Path)
		if err != nil {
			return false, err
		}
		before := file.input()
		matchesBefore := actual.info == before.info && bytes.Equal(actual.data, before.data)
		written := len(d.ConfigAfter) > 0 && slices.Contains(d.WrittenPaths, file.Path)
		matchesPost := written && actual.info.exists && bytes.Equal(actual.data, d.ConfigAfter)
		if !matchesBefore && !matchesPost {
			return false, errAuthenticationInputsChanged
		}
		if written {
			allPost = allPost && matchesPost
			allBefore = allBefore && matchesBefore
		}
	}
	if !allPost && !allBefore {
		return false, errAuthenticationInputsChanged
	}
	s.configMu.Lock()
	snapshot := s.runtimeSnapshotLocked(s.config, s.resolver, s.providerRegistry, s.effectiveEnvironment)
	s.configMu.Unlock()
	order, paths, err := s.authenticationInputPathsLocked(ctx, snapshot)
	if err != nil {
		return false, err
	}
	// A not-yet-created scope may add its captured projected order only after the
	// rename. Existing paths may not be added outside that exact scope topology.
	recorded := make([]string, 0, len(d.Inputs))
	for _, f := range d.Inputs {
		recorded = append(recorded, f.Path)
	}
	for _, path := range paths {
		if !slices.Contains(recorded, path) {
			return false, errAuthenticationInputsChanged
		}
	}
	if allPost && !slices.Equal(order, d.Order) || !allPost && !slices.Equal(order, d.BeforeOrder) {
		return false, errAuthenticationInputsChanged
	}
	return allPost, ctx.Err()
}

// CheckLocalAuthenticationRepairScope prevents historical records from another
// workspace directory being exposed through a shared global journal.
func (s *ConfigStore) CheckLocalAuthenticationRepairScope(ctx context.Context, capture LocalAuthenticationChange) error {
	if capture.journal.store != s {
		return errors.New("local authentication capture belongs to another owner")
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return err
	}
	defer s.writeMu.RUnlock()
	d := capture.disk
	if d.GlobalPath != s.globalDataPath || d.WorkspacePath != s.workspacePath || d.WorkingDir != s.workingDir {
		return errors.New("local authentication operation belongs to another workspace scope")
	}
	return s.RuntimeRevocation()
}
