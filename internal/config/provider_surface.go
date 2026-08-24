package config

import (
	"encoding/json"
	"fmt"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerregistry"
	kjsonschema "github.com/kaptinlin/jsonschema"
)

// ProviderSurfaces returns the redacted registry-generated presentation state
// for this configuration. Effective configured model overrides are preserved,
// and custom or temporarily unavailable providers remain visible as generic
// surfaces so their configuration is not tied to plugin presence.
func ProviderSurfaces(cfg *Config) []providerregistry.Surface {
	if cfg == nil {
		return nil
	}
	catalog, _ := Providers(cfg)
	byID := make(map[string]int, len(catalog))
	for i, provider := range catalog {
		byID[string(provider.ID)] = i
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			if index, ok := byID[id]; ok {
				if provider.Name != "" {
					catalog[index].Name = provider.Name
				}
				catalog[index].Models = append([]catwalk.Model(nil), provider.Models...)
			}
		}
	}

	selected := make(map[string]string, 2)
	if model, ok := cfg.Models[SelectedModelTypeLarge]; ok {
		selected[model.Provider] = model.Model
	}
	if model, ok := cfg.Models[SelectedModelTypeSmall]; ok {
		if _, exists := selected[model.Provider]; !exists {
			selected[model.Provider] = model.Model
		}
	}

	registered := ProviderCapabilities().Surfaces(catalog, selected)
	registeredByID := make(map[string]providerregistry.Surface, len(registered))
	for _, surface := range registered {
		registeredByID[surface.ID] = surface
	}

	result := make([]providerregistry.Surface, 0, len(catalog)+len(registered))
	seen := make(map[string]struct{}, len(catalog)+len(registered))
	for _, provider := range catalog {
		id := string(provider.ID)
		surface, ok := registeredByID[id]
		if !ok {
			surface = genericProviderSurface(id, provider.Name, provider.Models)
			surface.DefaultLargeModel = provider.DefaultLargeModelID
			surface.DefaultSmallModel = provider.DefaultSmallModelID
		}
		if cfg.Providers != nil {
			if configured, exists := cfg.Providers.Get(id); exists && configured.Disable {
				surface.Available = false
				surface.Availability = "disabled"
				surface.Diagnostic = "provider is disabled in configuration"
			}
		}
		surface.Order = len(result)
		result = append(result, surface)
		seen[id] = struct{}{}
	}
	for _, surface := range registered {
		if _, ok := seen[surface.ID]; ok {
			continue
		}
		if cfg.Providers != nil {
			if configured, exists := cfg.Providers.Get(surface.ID); exists && configured.Disable {
				surface.Available = false
				surface.Availability = "disabled"
				surface.Diagnostic = "provider is disabled in configuration"
			}
		}
		surface.Order = len(result)
		result = append(result, surface)
		seen[surface.ID] = struct{}{}
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			if _, ok := seen[id]; ok {
				continue
			}
			surface := genericProviderSurface(id, provider.Name, provider.Models)
			if provider.Plugin != nil {
				surface.Available = false
				surface.Availability = "missing"
				surface.Diagnostic = fmt.Sprintf("provider plugin %s is not installed; configuration, credentials, and model selections were preserved", provider.Plugin.ID)
				if status, mode, found := ProviderPluginAvailability(provider.Plugin.ID); found {
					surface.Availability = string(status.State)
					if mode == providerregistry.OwnerDisabled {
						surface.Availability = "disabled"
						surface.Diagnostic = fmt.Sprintf("provider plugin %s is disabled by the host rollout profile", provider.Plugin.ID)
					} else if len(status.Diagnostics) > 0 {
						surface.Diagnostic = status.Diagnostics[0].Message
					} else {
						surface.Diagnostic = fmt.Sprintf("provider plugin %s is %s; configuration, credentials, and model selections were preserved", provider.Plugin.ID, status.State)
					}
				}
			} else if cfg.isUnavailableRegisteredProvider(id) {
				surface.Available = false
				surface.Availability = "missing"
				surface.Diagnostic = "OAuth provider integration is not active; credentials, configuration, and model selections were preserved"
			} else if provider.Disable {
				surface.Available = false
				surface.Availability = "disabled"
				surface.Diagnostic = "provider is disabled in configuration"
			}
			surface.Order = len(result)
			result = append(result, surface)
			seen[id] = struct{}{}
		}
	}
	return result
}

func genericProviderSurface(id, name string, models []catwalk.Model) providerregistry.Surface {
	if name == "" {
		name = id
	}
	return providerregistry.Surface{
		ID:           id,
		Name:         name,
		Available:    true,
		Availability: "available",
		Models:       append([]catwalk.Model(nil), models...),
	}
}

// ValidateProviderConfiguration validates one installed provider's namespaced
// declarative values. Unknown or missing-plugin providers deliberately bypass
// validation so their values survive plugin absence losslessly.
func ValidateProviderConfiguration(providerID string, values map[string]any) error {
	registration, ok := ProviderCapabilities().Lookup(providerID)
	if !ok || registration.ProviderID != providerID || registration.Manifest == nil {
		return nil
	}
	schemaBytes, err := json.Marshal(registration.Manifest.Configuration.Schema)
	if err != nil {
		return fmt.Errorf("encoding provider %s configuration schema: %w", providerID, err)
	}
	schema, err := kjsonschema.NewCompiler().Compile(schemaBytes)
	if err != nil {
		return fmt.Errorf("compiling provider %s configuration schema: %w", providerID, err)
	}
	if values == nil {
		values = map[string]any{}
	}
	result := schema.ValidateMap(values)
	if result.IsValid() {
		return nil
	}
	validation, _ := json.Marshal(result)
	return fmt.Errorf("provider %s configuration is invalid: %s", providerID, validation)
}
