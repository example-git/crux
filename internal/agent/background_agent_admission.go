package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/lock"
)

const backgroundAgentAdmissionLockTimeout = 2 * time.Second

// backgroundAgentAdmission is a per-user, cross-process pool of kernel-held
// file locks. Each active agent owns one slot for its entire non-terminal
// lifetime. The operating system releases the slot if its process exits, so a
// crash cannot strand capacity.
type backgroundAgentAdmission struct {
	directory string
	limit     int
}

func newSystemBackgroundAgentAdmission(limit int) (*backgroundAgentAdmission, error) {
	globalDataDirectory, err := filepath.Abs(filepath.Dir(config.GlobalConfigData()))
	if err != nil {
		return nil, fmt.Errorf("resolve global data directory for background agent admission: %w", err)
	}
	return newBackgroundAgentAdmission(
		filepath.Join(globalDataDirectory, "locks", "background-agents"),
		limit,
	)
}

func newBackgroundAgentAdmission(directory string, limit int) (*backgroundAgentAdmission, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("global background agent admission limit must be positive")
	}
	if directory == "" {
		return nil, fmt.Errorf("global background agent admission directory is required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("initialize global background agent admission directory %q: %w", directory, err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect global background agent admission directory %q: %w", directory, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("global background agent admission path %q is not a directory", directory)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("protect global background agent admission directory %q: %w", directory, err)
	}
	return &backgroundAgentAdmission{directory: directory, limit: limit}, nil
}

func (a *backgroundAgentAdmission) acquire(parent context.Context) (func(), error) {
	if err := parent.Err(); err != nil {
		return nil, err
	}
	gatePath := filepath.Join(a.directory, "admission.lock")
	if err := validateBackgroundAgentLockPath(gatePath); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(parent, backgroundAgentAdmissionLockTimeout)
	defer cancel()
	releaseGate, err := lock.File(ctx, gatePath)
	if err != nil {
		return nil, fmt.Errorf("acquire global background agent admission lock: %w", err)
	}
	defer releaseGate()
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	for slot := range a.limit {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		slotPath := filepath.Join(a.directory, fmt.Sprintf("slot-%03d.lock", slot))
		if err := validateBackgroundAgentLockPath(slotPath); err != nil {
			return nil, err
		}
		release, err := lock.TryFile(slotPath)
		if err == nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				release()
				return nil, ctxErr
			}
			return release, nil
		}
		if errors.Is(err, lock.ErrContended) {
			continue
		}
		return nil, fmt.Errorf("acquire global background agent admission slot %d: %w", slot, err)
	}
	return nil, fmt.Errorf("global background agent capacity reached: %d active tasks across all Crux processes", a.limit)
}

func validateBackgroundAgentLockPath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect global background agent admission lock %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("global background agent admission lock %q is not a regular file", path)
	}
	return nil
}
