package config

import (
	"context"
	"errors"
)

var ErrRuntimeRevoked = errors.New("workspace credential authority was revoked")

// RevokeRuntime retires this store and every snapshot captured from it. It does
// not erase history, change account files, or fall back to execution-host secrets.
func (s *ConfigStore) RevokeRuntime() {
	// Publication checks this flag while holding the same lock. Cancel work
	// before joining writeMu, but never let a prepared replacement publish
	// after the revocation boundary won.
	s.configMu.Lock()
	s.runtimeRevoked.Store(true)
	s.configMu.Unlock()
	s.ensureRuntimeLifetime()
	s.runtimeCancel()
	s.writeMu.Lock()
	s.writeMu.Unlock()
	s.clientRefreshMu.Lock()
	s.clientRefreshPublisher = nil
	for _, call := range s.clientRefreshes {
		if !call.completed {
			call.err = ErrRuntimeRevoked
			call.completed = true
			close(call.done)
		}
	}
	s.clientRefreshMu.Unlock()
}

func (s *ConfigStore) RuntimeRevocation() error {
	if s != nil && s.runtimeRevoked.Load() {
		return ErrRuntimeRevoked
	}
	if s != nil && s.runtimeParent != nil {
		return s.runtimeParent.RuntimeRevocation()
	}
	return nil
}

func (s RuntimeSnapshot) RuntimeRevocation() error {
	if s.lifetimeStore != nil {
		return s.lifetimeStore.RuntimeRevocation()
	}
	return s.publicationStore.RuntimeRevocation()
}

func (s *ConfigStore) ensureRuntimeLifetime() {
	s.runtimeLifetimeOnce.Do(func() { s.runtimeContext, s.runtimeCancel = context.WithCancel(context.Background()) })
	if s.runtimeRevoked.Load() {
		s.runtimeCancel()
	}
}

// BindRuntimeContext must also wrap a caller-insulated exchange context. Only
// this exact ConfigStore's lifetime is canceled; another workspace remains live.
func (s *ConfigStore) BindRuntimeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s.runtimeParent != nil {
		return s.runtimeParent.BindRuntimeContext(ctx)
	}
	s.ensureRuntimeLifetime()
	work, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.runtimeContext, cancel)
	if s.runtimeContext.Err() != nil {
		cancel()
	}
	return work, func() { stop(); cancel() }
}
