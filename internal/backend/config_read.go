package backend

import (
	"context"
	"errors"

	"github.com/example-git/crux/internal/config"
)

func (b *Backend) ReadConfigView(ctx context.Context, workspaceID string) (config.ConfigReadView, error) {
	if workspaceID == "" {
		if b.cfg == nil {
			return config.ConfigReadView{}, errors.New("global configuration authority is unavailable")
		}
		return b.cfg.ReadConfigView(ctx, config.ScopeGlobal)
	}
	ws, ctx, done, err := b.beginProviderAuthRead(ctx, workspaceID)
	if err != nil {
		return config.ConfigReadView{}, err
	}
	defer done()
	return ws.Cfg.ReadConfigView(ctx, config.ScopeWorkspace)
}
