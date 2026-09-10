package model

import (
	"fmt"
	"reflect"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/providerregistry"
)

func (p *Preview) UseLaunchProviders(cfg *config.Config) error {
	surfaces := config.ProviderSurfaces(cfg)
	selected := cfg.Models[config.SelectedModelTypeLarge]
	for i := len(p.data.Messages) - 1; i >= 0; i-- {
		msg := p.data.Messages[i]
		if msg.Role == message.Assistant && msg.Model != "" && msg.Provider != "" {
			selected.Model = msg.Model
			selected.Provider = msg.Provider
			break
		}
	}
	if selected.Model == "" || selected.Provider == "" {
		return fmt.Errorf("normal launch configuration has no selected provider/model")
	}
	surface, found := providerregistry.LookupSurface(surfaces, selected.Provider)
	if !found {
		surface = providerregistry.Surface{ID: selected.Provider, Name: selected.Provider, Availability: "unavailable", Diagnostic: "Saved session provider is absent from the current launch catalog"}
		surfaces = append(surfaces, surface)
		p.schemaNote += "; saved provider is absent from the current catalog"
	}
	hasModel := false
	for _, model := range surface.Models {
		if model.ID == selected.Model {
			hasModel = true
		}
	}
	if !hasModel {
		surface.Models = append(surface.Models, catalog.Model{ID: selected.Model, Name: selected.Model + " (historical; metadata unavailable)"})
		p.schemaNote += "; saved model metadata is unavailable; identity retained"
		for i := range surfaces {
			if surfaces[i].ID == surface.ID {
				surfaces[i] = surface
			}
		}
	}
	p.runtimeConfig = cfg
	p.runtimeSurfaces = surfaces
	p.initialModel = selected.Provider + "::" + selected.Model
	p.data.Provider = surface.Clone()
	p.data.Models = p.data.Provider.Models
	p.data.Provider.Models = nil
	p.data.Settings = PreviewSettings{Debug: cfg.Options.Debug, InstructionMode: cfg.Options.InstructionMode, DisabledInstructionSections: cfg.Options.DisabledInstructionSections, DisableAutoSummarize: cfg.Options.DisableAutoSummarize, SummarizationContextCap: cfg.Options.SummarizationContextCap, SummarizationMaxTokens: cfg.Options.SummarizationMaxTokens, SummarizationFastMode: cfg.Options.SummarizationFastMode, CodexCompactionV2: cfg.Options.CodexCompactionV2}
	p.base = clonePreviewValue(reflect.ValueOf(p.data)).Interface().(*PreviewData)
	p.ui = nil
	p.itemsKey = ""
	return nil
}

func (p *Preview) InitialModel() string {
	if p.initialModel != "" {
		return p.initialModel
	}
	return "dummy-coder"
}

func (p *Preview) Catalog() map[string]any {
	result := PreviewRegistry()
	if p.runtimeConfig == nil {
		return result
	}
	models := []map[string]any{}
	for _, surface := range p.runtimeSurfaces {
		for _, model := range surface.Models {
			models = append(models, map[string]any{"id": surface.ID + "::" + model.ID, "name": surface.Name + " / " + model.Name, "context_window": model.ContextWindow})
		}
	}
	result["provider"] = "Normal launch providers · read-only session"
	result["providers"] = p.runtimeSurfaces
	result["models"] = models
	return result
}
