package config

import (
	"context"
	"errors"
)

// AbandonAuthenticationLocalChange retires only the retained recovery intent.
// Original observations remain unchanged, including an exchange with an unknown
// response. No token, account, configuration, or runtime effect is repeated.
func (s *ConfigStore) AbandonAuthenticationLocalChange(ctx context.Context, capture LocalAuthenticationChange, expectedRevision uint64) (result LocalAuthenticationRepairResult, err error) {
	result.Summary = capture.Summary()
	if capture.journal.store != s || expectedRevision == 0 {
		return result, errors.New("matching private local abandonment capture is required")
	}
	ctx, cancel := s.BindRuntimeContext(ctx)
	defer cancel()
	release, err := capture.journal.AcquireOperation(ctx, capture.disk.Key)
	if err != nil {
		return result, err
	}
	defer release()
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
	result.Summary = current.Summary()
	if d.Owner != capture.disk.Owner || d.GlobalPath != s.globalDataPath || d.WorkspacePath != s.workspacePath || d.WorkingDir != s.workingDir {
		return result, errors.New("local authentication abandonment owner or captured paths changed")
	}
	if d.Abandoned {
		if d.AbandonBase != expectedRevision {
			return result, errors.New("local authentication abandonment revision differs from the retained action")
		}
		result.Abandoned = true
		return result, nil
	}
	if current.revision != expectedRevision {
		return result, errors.New("local authentication abandonment revision changed; review the operation again")
	}
	if d.completed() {
		return result, errors.New("this local authentication record is already complete; no abandonment is needed")
	}
	writer := &localAuthenticationWriter{capture: current}
	writer.capture.disk.Abandoned = true
	writer.capture.disk.AbandonBase = expectedRevision
	if err := writer.save(ctx); err != nil {
		return result, err
	}
	result.Summary = writer.capture.Summary()
	result.Abandoned = true
	return result, nil
}
