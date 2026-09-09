package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const authenticationCompletionTimeout = 10 * time.Second

// authenticationScopeWrite owns only the exact staged credential edit. It
// neither selects a path nor transforms a file at commit time. The caller holds
// the persistence mutex and sorted scope locks until verification/publication.
type authenticationScopeWrite struct {
	before             authenticationInputFile
	data               []byte
	temporary          string
	used               bool
	completionDeadline time.Time
}

func (authenticationScopeWrite) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authentication scope writes are private")
}
func (authenticationScopeWrite) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private authentication scope write]"))
}

func stageAuthenticationScopeWrite(ctx context.Context, before authenticationInputFile, edit authenticationCredentialEdit) (*authenticationScopeWrite, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(before.path) || filepath.Clean(before.path) != before.path || before.path != edit.path || isShellConfig(before.path) || !authenticationLayerObject(edit.data) {
		return nil, errors.New("authentication scope staging requires its exact captured JSON edit")
	}
	if err := checkAuthenticationScopePreimage(ctx, before); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(filepath.Dir(before.path), ".authentication-change-*")
	if err != nil {
		return nil, authenticationInputError(err)
	}
	stage := &authenticationScopeWrite{before: before, data: bytes.Clone(edit.data), temporary: file.Name()}
	ready := false
	defer func() {
		_ = file.Close()
		if !ready {
			stage.Close()
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, authenticationInputError(err)
	}
	if _, err := io.Copy(file, authenticationInputReader{ctx: ctx, reader: bytes.NewReader(stage.data)}); err != nil {
		return nil, authenticationInputError(err)
	}
	if err := file.Sync(); err != nil {
		return nil, authenticationInputError(err)
	}
	if err := file.Close(); err != nil {
		return nil, authenticationInputError(err)
	}
	if err := checkAuthenticationScopePreimage(ctx, before); err != nil {
		return nil, err
	}
	ready = true
	return stage, nil
}

func checkAuthenticationScopePreimage(ctx context.Context, before authenticationInputFile) error {
	actual, err := readAuthenticationInput(ctx, before.path)
	if err != nil {
		return authenticationInputError(err)
	}
	if actual.info != before.info || !bytes.Equal(actual.data, before.data) {
		return errAuthenticationInputsChanged
	}
	return nil
}

// Commit reports written immediately after rename. No cancellation, sync or
// observed postimage failure may turn that durable effect back into false.
func (stage *authenticationScopeWrite) Commit(ctx context.Context) (after authenticationInputFile, written bool, err error) {
	return stage.commit(ctx, time.Time{})
}

// inheritedDeadline is set only after a prior durable write in the same
// transaction. Never renew that deadline while completing the paired write.
func (stage *authenticationScopeWrite) commit(ctx context.Context, inheritedDeadline time.Time) (after authenticationInputFile, written bool, err error) {
	if stage == nil || stage.used || stage.temporary == "" {
		return after, false, errors.New("authentication scope write is closed or already used")
	}
	stage.used = true
	if !inheritedDeadline.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, inheritedDeadline)
		defer cancel()
	}
	deadline := time.Now().Add(renameRetryBudget)
	delay := time.Millisecond
	for {
		if err := checkAuthenticationScopePreimage(ctx, stage.before); err != nil {
			return after, false, err
		}
		if err := ctx.Err(); err != nil {
			return after, false, err
		}
		err := os.Rename(stage.temporary, stage.before.path)
		if err == nil {
			break
		}
		if !isTransientRenameError(err) || !time.Now().Before(deadline) {
			return after, false, authenticationInputError(err)
		}
		timer := time.NewTimer(min(delay, time.Until(deadline)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return after, false, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 50*time.Millisecond)
	}
	stage.temporary = ""
	stage.completionDeadline = inheritedDeadline
	if stage.completionDeadline.IsZero() {
		stage.completionDeadline = time.Now().Add(authenticationCompletionTimeout)
	}
	completion, cancel := context.WithDeadline(context.WithoutCancel(ctx), stage.completionDeadline)
	defer cancel()
	if err := completion.Err(); err != nil {
		return after, true, err
	}
	if err := syncAuthenticationScopeDirectory(filepath.Dir(stage.before.path)); err != nil {
		return after, true, authenticationInputError(err)
	}
	after, err = readAuthenticationInput(completion, stage.before.path)
	if err != nil {
		return authenticationInputFile{}, true, authenticationInputError(err)
	}
	if !after.info.exists || !bytes.Equal(after.data, stage.data) {
		return authenticationInputFile{}, true, errAuthenticationInputsChanged
	}
	return after, true, nil
}

func (stage *authenticationScopeWrite) Close() {
	if stage == nil {
		return
	}
	if stage.temporary != "" {
		_ = os.Remove(stage.temporary)
	}
	stage.temporary = ""
	stage.data = nil
	stage.before = authenticationInputFile{}
	stage.used = true
}

func syncAuthenticationScopeDirectory(path string) error {
	// Windows provides rename visibility but no Unix directory-fsync contract.
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
