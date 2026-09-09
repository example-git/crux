package backend

import (
	"context"
	"errors"
	"sync"
)

var ErrPrincipalRevoked = errors.New("authenticated principal was revoked")

type principalLifetime struct {
	ctx        context.Context
	cancel     context.CancelFunc
	revoked    bool
	creations  sync.WaitGroup
	drained    chan struct{}
	workspaces map[string]*Workspace
	err        error
}

func (b *Backend) principalLocked(principal string) *principalLifetime {
	if principal == "" {
		return nil
	}
	if b.principals == nil {
		b.principals = make(map[string]*principalLifetime)
	}
	if existing := b.principals[principal]; existing != nil {
		return existing
	}
	ctx, cancel := context.WithCancel(b.ctx)
	lifetime := &principalLifetime{ctx: ctx, cancel: cancel, drained: make(chan struct{}), workspaces: map[string]*Workspace{}}
	b.principals[principal] = lifetime
	return lifetime
}

// AdmitPrincipal is called only after a current persisted grant is verified.
// Reapproval of the same certificate starts a new lifetime only after the old
// lifetime has fully drained; old UUIDs remain retired.
func (b *Backend) AdmitPrincipal(principal string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return ErrServerShuttingDown
	}
	lifetime := b.principalLocked(principal)
	if lifetime == nil {
		return ErrWorkspaceAuthority
	}
	if lifetime.revoked {
		select {
		case <-lifetime.drained:
			if lifetime.err != nil {
				return ErrPrincipalRevoked
			}
			delete(b.principals, principal)
			b.principalLocked(principal)
		default:
			return ErrPrincipalRevoked
		}
	}
	return nil
}

// RevokePrincipal fences admission, removes all exclusive-owner claims, and
// cancels work before waiting. Cleanup survives the caller's wait cancellation;
// a later administrative retry joins the same retained drain.
func (b *Backend) RevokePrincipal(ctx context.Context, principal string) error {
	b.mu.Lock()
	lifetime := b.principalLocked(principal)
	if lifetime == nil {
		b.mu.Unlock()
		return ErrWorkspaceAuthority
	}
	if !lifetime.revoked {
		lifetime.revoked = true
		lifetime.cancel()
		var workspaces []*Workspace
		if b.drainingPaths == nil {
			b.drainingPaths = make(map[string]*Workspace)
		}
		if b.retired == nil {
			b.retired = make(map[string]struct{})
		}
		for clientID, owner := range b.clientPrincipals {
			if owner == principal {
				b.retired[clientID] = struct{}{}
			}
		}
		for _, ws := range lifetime.workspaces {
			ws.runMu.Lock()
			ws.closing = true
			ws.runMu.Unlock()
			if ws.cancel != nil {
				ws.cancel()
			}
			ws.clientsMu.Lock()
			for _, claim := range ws.clients {
				if claim.holdTimer != nil {
					claim.holdTimer.Stop()
				}
			}
			clear(ws.clients)
			ws.clientsMu.Unlock()
			if b.pathIndex[ws.resolvedPath] == ws.ID {
				delete(b.pathIndex, ws.resolvedPath)
			}
			b.workspaces.Del(ws.ID)
			b.drainingPaths[ws.resolvedPath] = ws
			workspaces = append(workspaces, ws)
		}
		go func() {
			var drains sync.WaitGroup
			for _, ws := range workspaces {
				drains.Go(func() { ws.drainRevokedPrincipal(); b.releasePrincipalWorkspace(ws) })
			}
			// Final publication rechecks admitLocked. Provisional workspaces
			// are shut down before their creation count is released.
			lifetime.creations.Wait()
			drains.Wait()
			b.mu.Lock()
			for _, ws := range workspaces {
				lifetime.err = errors.Join(lifetime.err, ws.shutdownErr)
			}
			clear(lifetime.workspaces)
			b.mu.Unlock()
			close(lifetime.drained)
		}()
	}
	b.mu.Unlock()
	select {
	case <-lifetime.drained:
		return lifetime.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (w *Workspace) drainRevokedPrincipal() {
	w.invokeShutdown()
}

// Releasing the draining reservation is also the point at which the daemon
// can become idle. A timer or control request must not exit during cleanup.
func (b *Backend) releasePrincipalWorkspace(ws *Workspace) {
	if ws.shutdownErr != nil {
		return
	}
	b.mu.Lock()
	if b.drainingPaths[ws.resolvedPath] == ws {
		delete(b.drainingPaths, ws.resolvedPath)
	}
	if lifetime := b.principals[ws.principal]; lifetime != nil && lifetime.workspaces[ws.ID] == ws {
		delete(lifetime.workspaces, ws.ID)
	}
	shutdownNow := b.scheduleShutdownIfIdleLocked()
	b.mu.Unlock()
	if shutdownNow && b.shutdownFn != nil {
		b.shutdownFn()
	}
}
