package backend

import (
	"context"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
)

// OverrideModels atomically applies transient selections to a server-owned
// workspace. ConfigStore rejects this operation for client-owned runtimes.
func (b *Backend) OverrideModels(ctx context.Context, workspaceID string, requested config.AgentModelState) (config.AgentModelState, error) {
	ws, err := b.GetWorkspace(workspaceID)
	if err != nil {
		return config.AgentModelState{}, err
	}
	ws.runMu.Lock()
	if ws.closing {
		ws.runMu.Unlock()
		return config.AgentModelState{}, ErrWorkspaceClosing
	}
	ws.runWG.Add(1)
	ws.runMu.Unlock()
	defer ws.runWG.Done()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ws.ctx, cancel)
	defer stop()
	if ws.ctx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return config.AgentModelState{}, err
	}
	state, err := ws.Cfg.OverrideModelsForOwnersContext(ctx, requested)
	if err != nil {
		return config.AgentModelState{}, err
	}
	// Model selections do not change process-wide MCP configuration.
	ws.SendEvent(pubsub.Event[proto.ConfigChanged]{
		Type:    pubsub.UpdatedEvent,
		Payload: proto.ConfigChanged{WorkspaceID: ws.ID},
	})
	return state, nil
}
