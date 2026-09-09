package workspace

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
)

func (w *AppWorkspace) RuntimeControlState(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return w.store.RuntimeControlState(ctx, scope, target)
}

func (w *AppWorkspace) SetRuntimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage) (config.RuntimeControlState, error) {
	return w.store.SetRuntimeControl(ctx, scope, target, value)
}

func (w *AppWorkspace) RemoveRuntimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return w.store.RemoveRuntimeControl(ctx, scope, target)
}

func (w *ClientWorkspace) RuntimeControlState(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return w.runtimeControl(ctx, scope, target, nil, false, false)
}

func (w *ClientWorkspace) SetRuntimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage) (config.RuntimeControlState, error) {
	return w.runtimeControl(ctx, scope, target, value, true, true)
}

func (w *ClientWorkspace) RemoveRuntimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget) (config.RuntimeControlState, error) {
	return w.runtimeControl(ctx, scope, target, nil, true, false)
}

func (w *ClientWorkspace) runtimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage, mutation, set bool) (config.RuntimeControlState, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(w.subCtx, cancel)
	defer stop()
	if w.subCtx.Err() != nil {
		cancel()
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := proto.ValidateRuntimeControlRequest(&scope, target, value, mutation, set); err != nil {
		return config.RuntimeControlState{}, err
	}
	if !w.clientOwned() {
		return w.serverRuntimeControl(ctx, scope, target, value, mutation, set)
	}
	a := w.authority
	if a == nil {
		return config.RuntimeControlState{}, fmt.Errorf("owning client runtime control configuration is unavailable; reconnect with a collected runtime")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := w.reconcileClientAuthority(ctx, a); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	if !mutation {
		accepted, err := a.configView().RuntimeControlEffectiveState(scope, target)
		if err != nil {
			return config.RuntimeControlState{}, err
		}
		local, err := a.store.Config().RuntimeControlEffectiveState(scope, target)
		if err != nil || !proto.RuntimeControlEffectiveStatesEqual(local, accepted) {
			return config.RuntimeControlState{}, fmt.Errorf("local runtime control changes are not acknowledged by the execution host; refresh the client runtime before resolving this control")
		}
		if err := validateAcceptedRuntimeControl(a.accepted, a.configView(), accepted); err != nil {
			return config.RuntimeControlState{}, err
		}
		// Resolving never publishes local changes or mistakes unknown scope
		// provenance for an absent persisted value.
		if err := proto.ValidateRuntimeControlState(accepted); err != nil {
			return config.RuntimeControlState{}, err
		}
		return accepted, ctx.Err()
	}
	if err := a.requireAuthenticationPublication(w.workspaceID()); err != nil {
		return config.RuntimeControlState{}, err
	}
	var stored config.RuntimeControlState
	var err error
	if set {
		stored, err = a.store.SetRuntimeControl(ctx, scope, target, value)
	} else {
		stored, err = a.store.RemoveRuntimeControl(ctx, scope, target)
	}
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := proto.ValidateRuntimeControlAcknowledgement(stored, scope, target, value, true, set); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := w.publishClientAuthorityLocked(ctx, a); err != nil {
		return config.RuntimeControlState{}, err
	}
	accepted, err := a.configView().RuntimeControlEffectiveState(scope, target)
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	if !proto.RuntimeControlEffectiveStatesEqual(stored, accepted) {
		return config.RuntimeControlState{}, fmt.Errorf("accepted runtime control state changed before acknowledgement")
	}
	if err := validateAcceptedRuntimeControl(a.accepted, a.configView(), accepted); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	return stored, nil
}

func (w *ClientWorkspace) serverRuntimeControl(ctx context.Context, scope config.Scope, target config.RuntimeControlTarget, value json.RawMessage, mutation, set bool) (config.RuntimeControlState, error) {
	id := w.workspaceID()
	var state config.RuntimeControlState
	var err error
	switch {
	case !mutation:
		state, err = w.client.RuntimeControlState(ctx, id, scope, target)
	case set:
		state, err = w.client.SetRuntimeControl(ctx, id, scope, target, value)
	default:
		state, err = w.client.RemoveRuntimeControl(ctx, id, scope, target)
	}
	if err != nil {
		return config.RuntimeControlState{}, err
	}
	current, err := w.client.GetWorkspace(ctx, id)
	if err != nil {
		return config.RuntimeControlState{}, fmt.Errorf("cannot verify refreshed runtime control configuration: %w", err)
	}
	if current.ID != id || w.workspaceID() != id || current.Authority != nil && current.Authority.Mode == "client" {
		return config.RuntimeControlState{}, fmt.Errorf("workspace changed before runtime control acknowledgement")
	}
	if err := validateRuntimeControlPublicView(state, current.Config, current.ProviderSurfaces); err != nil {
		return config.RuntimeControlState{}, err
	}
	if err := ctx.Err(); err != nil {
		return config.RuntimeControlState{}, err
	}
	w.adoptRuntimeResponse(*current)
	if w.workspaceID() != id || w.clientOwned() {
		return config.RuntimeControlState{}, fmt.Errorf("workspace changed before runtime control acknowledgement")
	}
	if err := validateRuntimeControlPublicView(state, w.Config(), w.ProviderSurfaces()); err != nil {
		return config.RuntimeControlState{}, err
	}
	return state, ctx.Err()
}

func validateRuntimeControlPublicView(state config.RuntimeControlState, cfg *config.Config, surfaces []providerregistry.Surface) error {
	surface, ok := providerregistry.LookupSurface(surfaces, state.Target.Owner.ProviderID)
	if !ok {
		return fmt.Errorf("runtime control provider surface is unavailable")
	}
	effective, err := config.RuntimeControlEffectiveStateForSurface(cfg, state.Scope, state.Target, surface)
	if err != nil {
		return err
	}
	if !proto.RuntimeControlEffectiveStatesEqual(state, effective) {
		return fmt.Errorf("refreshed runtime control state differs from its acknowledgement")
	}
	return nil
}

func validateAcceptedRuntimeControl(proposal config.RemoteRuntimeProposal, cfg *config.Config, state config.RuntimeControlState) error {
	for kind, owned := range map[config.SelectedModelType]*config.OwnedSelectedModel{config.SelectedModelTypeLarge: state.Models.Large, config.SelectedModelTypeSmall: state.Models.Small} {
		if owned == nil || !runtimeControlValueEqual(proposal.Models[kind], owned.Model) {
			return fmt.Errorf("accepted runtime control model state differs from the acknowledged selection")
		}
		found := false
		for _, credential := range proposal.Credentials {
			if credential.Owner.ProviderID == owned.Owner.ProviderID {
				if credential.Owner != owned.Owner {
					return fmt.Errorf("accepted runtime control model owner changed")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("accepted runtime control model owner is missing")
		}
	}
	provider, ok := cfg.Providers.Get(state.Target.Owner.ProviderID)
	if !ok {
		return fmt.Errorf("accepted runtime control provider is unavailable")
	}
	found := false
	for _, definition := range proposal.Providers {
		if definition.Config.ID != state.Target.Owner.ProviderID {
			continue
		}
		if definition.Config.Disable || !runtimeControlValueEqual(definition.Config.ProviderOptions, provider.ProviderOptions) || !runtimeControlValueEqual(definition.Config.Models, provider.Models) {
			return fmt.Errorf("accepted runtime control provider options changed")
		}
		found = true
	}
	if !found {
		return fmt.Errorf("accepted runtime control provider definition is missing")
	}
	options := cfg.Options
	if options == nil {
		options = &config.Options{}
	}
	if proposal.Controls.AnalysisEffort != options.AnalysisEffort || proposal.Controls.ResponseVerbosity != options.ResponseVerbosity {
		return fmt.Errorf("accepted runtime control host options changed")
	}
	return nil
}

func runtimeControlValueEqual(left, right any) bool {
	a, ae := json.Marshal(left)
	b, be := json.Marshal(right)
	return ae == nil && be == nil && config.RuntimeControlJSONEqual(a, b)
}
