package backend

import (
	"context"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
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

func (ws *Workspace) startProviderUsage() {
	ws.runMu.Lock()
	defer ws.runMu.Unlock()
	if ws.closing {
		return
	}
	if ws.providerUsageWake == nil {
		ws.providerUsageWake = make(chan struct{}, 1)
		ws.providerUsageWG.Add(1)
		go ws.publishProviderUsage()
	}
	select {
	case ws.providerUsageWake <- struct{}{}:
	default:
	}
}

func (ws *Workspace) publishProviderUsage() {
	defer ws.providerUsageWG.Done()
	events := ws.Events(ws.ctx)
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ws.ctx.Done():
			return
		case <-ws.providerUsageWake:
		case <-ticker.C:
		case event, ok := <-events:
			if !ok {
				return
			}
			if _, changed := event.Payload.(pubsub.Event[proto.ConfigChanged]); !changed {
				continue
			}
		}
		snapshot := ws.Cfg.RuntimeSnapshot()
		cfg := snapshot.Config()
		for _, surface := range config.ProviderSurfaces(cfg) {
			if !surface.Available || !surface.UsageAvailable || surface.Owner == nil {
				continue
			}
			request := config.ProviderUsageRequest{Owner: *surface.Owner}
			if authority := snapshot.RemoteAuthority(); authority != nil {
				request.Revision, request.Digest = authority.Revision, authority.Digest
			}
			result := &config.ProviderUsageResult{Revision: request.Revision, Digest: request.Digest}
			ctx, cancel := context.WithTimeout(ws.ctx, 15*time.Second)
			usage, err := ws.Cfg.ProviderUsage(ctx, request)
			cancel()
			if ws.ctx.Err() != nil {
				return
			}
			if !snapshot.SamePublication(ws.Cfg.RuntimeSnapshot()) {
				break
			}
			if err == nil {
				result = usage
			}
			ws.SendEvent(pubsub.Event[proto.AgentEvent]{Type: pubsub.UpdatedEvent, Payload: proto.AgentEvent{Type: proto.AgentEventTypeUsage, ProviderID: surface.ID, Owner: surface.Owner, Usage: result}})
		}
	}
}
