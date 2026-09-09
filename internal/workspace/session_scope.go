package workspace

import (
	"context"
	"errors"
)

type sessionWorkspaceContextKey struct{}

// ContextWithSessionWorkspace pins a UI session read/presence command to the
// incarnation for which it was prepared. It grants no workspace authority.
func ContextWithSessionWorkspace(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sessionWorkspaceContextKey{}, id)
}
func checkSessionWorkspace(ctx context.Context, id string) error {
	if expected, ok := ctx.Value(sessionWorkspaceContextKey{}).(string); ok && expected != id {
		return errors.New("workspace changed while loading the session")
	}
	return ctx.Err()
}
func (w *ClientWorkspace) sessionReadContext(ctx context.Context) (context.Context, string, func(), error) {
	ctx, done := providerAuthContext(ctx, w.subCtx)
	id := w.workspaceID()
	return ctx, id, done, checkSessionWorkspace(ctx, id)
}
