package backend

import (
	"context"
	"time"

	"github.com/example-git/crux/internal/config"
)

func (b *Backend) ProviderUsage(ctx context.Context, workspaceID string, request config.ProviderUsageRequest) (*config.ProviderUsageResult, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return nil, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return nil, ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()
	defer ws.runWG.Done()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stop := context.AfterFunc(ws.ctx, cancel)
	defer stop()
	if ws.ctx.Err() != nil {
		cancel()
	}
	return ws.Cfg.ProviderUsage(ctx, request)
}
