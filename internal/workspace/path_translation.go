package workspace

import (
	"context"

	"github.com/example-git/crux/internal/pathtranslate"
)

func (w *AppWorkspace) LoadPathTranslation(ctx context.Context, scope pathtranslate.Scope) (pathtranslate.Policy, error) {
	return w.app.LoadPathTranslation(ctx, scope, nil)
}

func (w *AppWorkspace) SavePathTranslation(ctx context.Context, policy pathtranslate.Policy) (pathtranslate.Policy, error) {
	return w.app.SavePathTranslation(ctx, policy, nil)
}

func (w *ClientWorkspace) LoadPathTranslation(ctx context.Context, scope pathtranslate.Scope) (pathtranslate.Policy, error) {
	return w.client.LoadPathTranslation(ctx, w.workspaceID(), scope)
}

func (w *ClientWorkspace) SavePathTranslation(ctx context.Context, policy pathtranslate.Policy) (pathtranslate.Policy, error) {
	return w.client.SavePathTranslation(ctx, w.workspaceID(), policy)
}
