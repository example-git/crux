package workspace

import (
	"context"
	"errors"
	"fmt"
	"reflect"

	"github.com/example-git/crux/internal/config"
)

// OverrideModels applies per-run model choices without writing either host's
// model configuration. Owning clients publish all selected dependencies before
// exposing the new model state to the caller.
func (w *ClientWorkspace) OverrideModels(ctx context.Context, requested config.AgentModelState) (config.AgentModelState, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.subCtx, cancel)
	defer stop()
	if w.subCtx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return config.AgentModelState{}, err
	}
	if !w.clientOwned() {
		state, err := w.client.OverrideModels(ctx, w.workspaceID(), requested)
		if err != nil {
			return config.AgentModelState{}, err
		}
		current, err := w.client.GetWorkspace(ctx, w.workspaceID())
		if err != nil {
			return config.AgentModelState{}, fmt.Errorf("model overrides applied; cannot refresh workspace configuration: %w", err)
		}
		if current.Config == nil || !reflect.DeepEqual(current.Config.AgentModelState(), state) {
			return config.AgentModelState{}, errors.New("model selection changed before override acknowledgement")
		}
		w.adoptRuntimeResponse(*current)
		return state, nil
	}
	a := w.authority
	if a == nil {
		return config.AgentModelState{}, errors.New("owning client model configuration is unavailable; reconnect with a collected runtime")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return config.AgentModelState{}, err
	}
	if _, err := a.store.OverrideModelsForOwnersContext(ctx, requested); err != nil {
		return config.AgentModelState{}, err
	}
	if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
		return config.AgentModelState{}, err
	}
	return a.configView().AgentModelState(), nil
}
