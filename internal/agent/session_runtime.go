package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/example-git/crux/internal/config"
)

type (
	sessionRuntimeKey          struct{}
	sessionInstalledRuntimeKey struct{}
)

func (a *sessionAgent) runtimeForContext(ctx context.Context) InstalledRuntime {
	if runtime, ok := ctx.Value(sessionInstalledRuntimeKey{}).(InstalledRuntime); ok {
		return runtime
	}
	return a.Runtime()
}

func (c *coordinator) modelSnapshotForContext(ctx context.Context) config.RuntimeSnapshot {
	if snapshot, ok := ctx.Value(sessionRuntimeKey{}).(config.RuntimeSnapshot); ok {
		return snapshot
	}
	return c.cfg.RuntimeSnapshot()
}

type sessionModelSelectionError struct {
	cause error
}

func (e *sessionModelSelectionError) Error() string {
	return fmt.Sprintf("session model selection is unavailable: %v", e.cause)
}

func (e *sessionModelSelectionError) Unwrap() error {
	return e.cause
}

func (e *sessionModelSelectionError) NonRetryable() bool {
	return true
}

func (c *coordinator) runtimeForSession(ctx context.Context, sessionID string) (InstalledRuntime, error) {
	current, err := c.sessions.Get(ctx, sessionID)
	if err != nil {
		return InstalledRuntime{}, err
	}
	if len(current.Models) == 0 {
		return InstalledRuntime{}, &sessionModelSelectionError{cause: errors.New("select the intended models for this session")}
	}
	var models config.AgentModelState
	decoder := json.NewDecoder(bytes.NewReader(current.Models))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&models); err != nil {
		return InstalledRuntime{}, &sessionModelSelectionError{cause: err}
	}
	var runtime InstalledRuntime
	err = c.cfg.WithRuntimeSnapshot(func(snapshot config.RuntimeSnapshot) error {
		scoped, err := snapshot.WithAgentModels(models)
		if err != nil {
			return &sessionModelSelectionError{cause: err}
		}
		if err := c.lockRuntimeGeneration(ctx); err != nil {
			return err
		}
		defer c.updateMu.Unlock()
		generation, err := c.buildRuntimeGeneration(ctx, scoped)
		if err != nil {
			return err
		}
		runtime = generation.installed
		runtime.SessionID, runtime.ModelRevision = current.ID, current.ModelRevision
		return nil
	})
	return runtime, err
}
