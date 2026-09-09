package workspace

import (
	"context"
	"errors"
	"sync/atomic"
)

type sessionWorkspaceContextKey struct{}
type sessionSelectionContextKey struct{}
type sessionSelectionGuard struct {
	current  *atomic.Uint64
	expected uint64
}

// ContextWithSessionSelection binds a scheduled UI command to its immutable
// selection generation. The atomic source is independent of the UI model.
func ContextWithSessionSelection(ctx context.Context, id string, current *atomic.Uint64, expected uint64) context.Context {
	ctx = ContextWithSessionWorkspace(ctx, id)
	return context.WithValue(ctx, sessionSelectionContextKey{}, sessionSelectionGuard{current, expected})
}

// ContextWithSessionWorkspace pins a UI session read/presence command to the
// incarnation for which it was prepared. It grants no workspace authority.
func ContextWithSessionWorkspace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionWorkspaceContextKey{}, id)
}
func checkSessionWorkspace(ctx context.Context, id string) error {
	if expected, ok := ctx.Value(sessionWorkspaceContextKey{}).(string); ok && expected != id {
		return errors.New("workspace changed while loading the session")
	}
	if selection, ok := ctx.Value(sessionSelectionContextKey{}).(sessionSelectionGuard); ok && (selection.current == nil || selection.current.Load() != selection.expected) {
		return errors.New("current-session selection was superseded")
	}
	return ctx.Err()
}
func (w *ClientWorkspace) sessionReadContext(ctx context.Context) (context.Context, string, func(), error) {
	ctx, done := providerAuthContext(ctx, w.subCtx)
	id := w.workspaceID()
	return ctx, id, done, checkSessionWorkspace(ctx, id)
}
