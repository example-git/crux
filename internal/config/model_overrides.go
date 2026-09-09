package config

import (
	"context"
	"fmt"

	"github.com/example-git/crux/internal/providerregistry"
)

// OverrideModelsForOwners atomically applies per-run agent model selections in
// memory. Every supplied model must still belong to its initiating active owner.
// Omitted selections stay unchanged; callers must supply a default small model
// explicitly when that is the desired behavior. The complete resulting main and
// small selection is pinned for this run so reload cannot recompute an omitted
// implicit model or import a sibling process's selections. Nothing is persisted
// to disk.
func (s *ConfigStore) OverrideModelsForOwners(requested AgentModelState) (AgentModelState, error) {
	return s.OverrideModelsForOwnersContext(context.Background(), requested)
}

// OverrideModelsForOwnersContext also rejects cancellation while waiting for the
// configuration lock, before the atomic selection and reload-pin publication.
func (s *ConfigStore) OverrideModelsForOwnersContext(ctx context.Context, requested AgentModelState) (AgentModelState, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := ctx.Err(); err != nil {
		return AgentModelState{}, err
	}
	if s.RemoteAuthority() != nil {
		return AgentModelState{}, ErrClientRuntimeManaged
	}
	if err := requested.Validate(); err != nil {
		return AgentModelState{}, err
	}
	current := s.Config()
	selections := []struct {
		modelType SelectedModelType
		selected  *OwnedSelectedModel
	}{
		{SelectedModelTypeLarge, requested.Large},
		{SelectedModelTypeSmall, requested.Small},
	}
	for _, selection := range selections {
		if selection.selected == nil {
			continue
		}
		model := selection.selected.Model
		if model.Provider == "" || model.Model == "" {
			return AgentModelState{}, fmt.Errorf("%s model provider and model ID are required", selection.modelType)
		}
		if err := s.validateActiveProviderOwnerLocked(current, selection.selected.Owner); err != nil {
			return AgentModelState{}, err
		}
		if !current.IsModelAvailable(model.Provider, model.Model) {
			return AgentModelState{}, fmt.Errorf("model %q for provider %q is not available", model.Model, model.Provider)
		}
	}

	// Validation finishes before either the live config or reload pins change.
	next := current.cloneForWrite()
	if next.Models == nil {
		next.Models = make(map[SelectedModelType]SelectedModel)
	}
	for modelType, model := range next.Models {
		next.Models[modelType] = cloneSelectedModel(model)
	}
	for _, selection := range selections {
		if selection.selected == nil {
			continue
		}
		model := cloneSelectedModel(selection.selected.Model)
		next.Models[selection.modelType] = model
		next.markModelExplicit(selection.modelType)
	}
	if err := ctx.Err(); err != nil {
		return AgentModelState{}, err
	}
	for _, selection := range selections {
		if model, ok := next.Models[selection.modelType]; ok {
			s.pinPreferredModelLocked(selection.modelType, model)
		}
	}
	s.setConfig(next)
	return captureAgentModelState(next.Models, func(providerID string) (providerregistry.RegistrationOwner, bool) {
		return providerOwnerForConfig(next, s.providerRegistry, providerID)
	}), nil
}
