package backend

import (
	"context"

	"github.com/example-git/crux/internal/pathtranslate"
	"github.com/example-git/crux/internal/proto"
)

func (b *Backend) LoadPathTranslation(ctx context.Context, workspaceID string, scope pathtranslate.Scope, attachment *proto.WorkspaceAttachment) (pathtranslate.Policy, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	return ws.App.LoadPathTranslation(ctx, scope, attachment)
}

func (b *Backend) SavePathTranslation(ctx context.Context, workspaceID string, policy pathtranslate.Policy, attachment *proto.WorkspaceAttachment) (pathtranslate.Policy, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return pathtranslate.Policy{}, err
	}
	return ws.App.SavePathTranslation(ctx, policy, attachment)
}
