package backend

import (
	"context"
	"time"

	"github.com/example-git/crux/internal/providerregistry"
)

func (b *Backend) ImportCopilot(ctx context.Context, workspaceID string, owner providerregistry.RegistrationOwner) (bool, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return false, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return false, ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()
	defer ws.runWG.Done()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	stop := context.AfterFunc(ws.ctx, cancel)
	defer stop()
	if ws.ctx.Err() != nil {
		cancel()
	}
	_, found, err := ws.Cfg.ImportCopilotForOwner(ctx, owner)
	if err != nil {
		return false, err
	}
	if found {
		publishConfigChanged(ws)
	}
	return found, nil
}
