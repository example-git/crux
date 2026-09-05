package providerplugin

import (
	"fmt"
	"maps"
	"slices"

	"github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

// CatalogProviders projects registered declarative manifests into the legacy
// catalog shape consumed by configuration and model-selection surfaces. Only
// trusted, compatible, non-quarantined registrations are included. This catalog
// is presentation and model metadata, not runtime ownership: callers must still
// use the matching registry operation and must never construct a generic
// provider from catalog shape alone.
func (m *Manager) CatalogProviders() ([]catalog.Provider, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := make([]catalog.Provider, 0, len(m.state.Plugins))
	for _, status := range m.state.Plugins {
		if status.State != StateRegistered || status.manifest == nil {
			continue
		}
		provider, err := catalogProvider(*status.manifest)
		if err != nil {
			return nil, fmt.Errorf("project plugin %s catalog: %w", status.ID, err)
		}
		providers = append(providers, provider)
	}
	return providers, nil
}

func (m *Manager) CatalogPresets() []catalog.Provider {
	m.mu.RLock()
	defer m.mu.RUnlock()

	providers := make([]catalog.Provider, 0, len(m.state.Plugins))
	for _, status := range m.state.Plugins {
		if status.State != StateRegistered || status.preset == nil {
			continue
		}
		providers = append(providers, catalogPreset(status.preset.Preset))
	}
	return providers
}

func catalogPreset(preset foundation.ProviderPreset) catalog.Provider {
	models := make([]catalog.Model, len(preset.Models))
	for i, model := range preset.Models {
		models[i] = catalog.Model{
			ID:                     model.ID,
			Name:                   model.Name,
			CostPer1MIn:            model.CostPer1MIn,
			CostPer1MOut:           model.CostPer1MOut,
			CostPer1MInCached:      model.CostPer1MInCached,
			CostPer1MOutCached:     model.CostPer1MOutCached,
			ContextWindow:          model.ContextWindow,
			DefaultMaxTokens:       model.DefaultMaxTokens,
			CanReason:              model.CanReason,
			ReasoningLevels:        slices.Clone(model.ReasoningLevels),
			DefaultReasoningEffort: model.DefaultReasoningEffort,
			SupportsImages:         model.SupportsImages,
			Options: catalog.ModelOptions{
				Temperature:      model.Options.Temperature,
				TopP:             model.Options.TopP,
				TopK:             model.Options.TopK,
				FrequencyPenalty: model.Options.FrequencyPenalty,
				PresencePenalty:  model.Options.PresencePenalty,
				ProviderOptions:  maps.Clone(model.Options.ProviderOptions),
			},
		}
	}
	return catalog.Provider{
		Name:                preset.Name,
		ID:                  catalog.ProviderID(preset.ID),
		APIKey:              preset.APIKey,
		APIEndpoint:         preset.APIEndpoint,
		Type:                catalog.Type(preset.Type),
		DefaultLargeModelID: preset.DefaultLargeModelID,
		DefaultSmallModelID: preset.DefaultSmallModelID,
		Models:              models,
		DefaultHeaders:      maps.Clone(preset.DefaultHeaders),
	}
}

func catalogProvider(value manifest.Manifest) (catalog.Provider, error) {
	var inference *manifest.Operation
	for i := range value.Capabilities.Operations {
		operation := &value.Capabilities.Operations[i]
		if operation.Kind != "inference" {
			continue
		}
		if inference != nil {
			return catalog.Provider{}, fmt.Errorf("multiple inference operations")
		}
		inference = operation
	}
	if inference == nil {
		return catalog.Provider{}, fmt.Errorf("missing inference operation")
	}

	var endpoint *manifest.Endpoint
	for i := range value.Capabilities.Endpoints {
		if value.Capabilities.Endpoints[i].ID == inference.Endpoint {
			endpoint = &value.Capabilities.Endpoints[i]
			break
		}
	}
	if endpoint == nil {
		return catalog.Provider{}, fmt.Errorf("inference endpoint %q is missing", inference.Endpoint)
	}

	models := make([]catalog.Model, len(value.Models))
	for i, model := range value.Models {
		models[i] = catalogModel(model)
	}
	return catalog.Provider{
		Name:                value.Provider.Name,
		ID:                  catalog.ProviderID(value.Provider.ID),
		APIEndpoint:         endpoint.BaseURL,
		Type:                catalogProviderType(inference.Protocol),
		DefaultLargeModelID: value.Provider.DefaultLargeModel,
		DefaultSmallModelID: value.Provider.DefaultSmallModel,
		Models:              models,
	}, nil
}

func catalogProviderType(protocol string) catalog.Type {
	if protocol == "openai-responses" {
		return catalog.TypeOpenAI
	}
	return ""
}

func catalogModel(model manifest.Model) catalog.Model {
	options := maps.Clone(model.DefaultOptions)
	result := catalog.Model{
		ID:                 model.ID,
		Name:               model.Name,
		CostPer1MIn:        model.CostPer1MIn,
		CostPer1MOut:       model.CostPer1MOut,
		CostPer1MInCached:  model.CostPer1MInCached,
		CostPer1MOutCached: model.CostPer1MOutCached,
		ContextWindow:      model.ContextWindow,
		DefaultMaxTokens:   model.DefaultMaxTokens,
		SupportsImages:     slices.Contains(model.Modalities.Input, "image"),
	}
	if model.Reasoning != nil {
		result.CanReason = true
		result.ReasoningLevels = slices.Clone(model.Reasoning.Levels)
		result.DefaultReasoningEffort = model.Reasoning.Default
	}

	result.Options.Temperature = takeFloat(options, "temperature")
	result.Options.TopP = takeFloat(options, "top_p")
	result.Options.TopK = takeInt(options, "top_k")
	result.Options.FrequencyPenalty = takeFloat(options, "frequency_penalty")
	result.Options.PresencePenalty = takeFloat(options, "presence_penalty")
	if len(options) > 0 {
		result.Options.ProviderOptions = options
	}
	return result
}

func takeFloat(values map[string]any, key string) *float64 {
	value, ok := values[key]
	if !ok {
		return nil
	}
	var number float64
	switch value := value.(type) {
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int64:
		number = float64(value)
	default:
		return nil
	}
	delete(values, key)
	return &number
}

func takeInt(values map[string]any, key string) *int64 {
	value, ok := values[key]
	if !ok {
		return nil
	}
	var number int64
	switch value := value.(type) {
	case int:
		number = int64(value)
	case int64:
		number = value
	case float64:
		if value != float64(int64(value)) {
			return nil
		}
		number = int64(value)
	default:
		return nil
	}
	delete(values, key)
	return &number
}
