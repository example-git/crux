package config

import (
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/providerregistry"
	kjsonschema "github.com/kaptinlin/jsonschema"
)

// ProviderSurfaces returns redacted presentation state without changing runtime
// ownership. Effective selected models and temporarily unavailable plugin
// providers remain visible so users can diagnose and restore them. A generic
// surface here is display-only: it must never reclassify a plugin-owned provider
// as a generic runtime or authorize fallback to another provider.
func ProviderSurfaces(cfg *Config) []providerregistry.Surface {
	if cfg == nil {
		return nil
	}
	providers, _ := Providers(cfg)
	byID := make(map[string]int, len(providers))
	for i, provider := range providers {
		byID[string(provider.ID)] = i
	}
	if cfg.Providers != nil {
		for id, provider := range cfg.Providers.Seq2() {
			if index, ok := byID[id]; ok {
				if provider.Name != "" {
					providers[index].Name = provider.Name
				}
				providers[index].Models = append([]catalog.Model(nil), provider.Models...)
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

	registered := cfg.providerCapabilities().Surfaces(providers, selected)
	registeredByID := make(map[string]providerregistry.Surface, len(registered))
	for _, surface := range registered {
		registeredByID[surface.ID] = surface
	}

	result := make([]providerregistry.Surface, 0, len(providers)+len(registered))
	seen := make(map[string]struct{}, len(providers)+len(registered))
	for _, provider := range providers {
		id := string(provider.ID)
		surface, ok := registeredByID[id]
		if !ok {
			surface = genericProviderSurface(id, provider.Name, provider.Models)
			surface.DefaultLargeModel = provider.DefaultLargeModelID
			surface.DefaultSmallModel = provider.DefaultSmallModelID
		}
		if cfg.Providers != nil {
			if configured, exists := cfg.Providers.Get(id); exists {
				if cfg.isUnavailableRegisteredProvider(id) {
					surface = retainedProviderSurface(id, configured)
				}
				applyProviderSurfaceAvailability(cfg, id, configured, &surface)
			}
		}
		applyProviderSurfaceOwner(cfg, id, &surface)
		surface.Order = len(result)
		result = append(result, surface)
		seen[id] = struct{}{}
	}
	for _, surface := range registered {
		if _, ok := seen[surface.ID]; ok {
			continue
		}
		if cfg.Providers != nil {
			if configured, exists := cfg.Providers.Get(surface.ID); exists {
				if cfg.isUnavailableRegisteredProvider(surface.ID) {
					surface = retainedProviderSurface(surface.ID, configured)
				}
				applyProviderSurfaceAvailability(cfg, surface.ID, configured, &surface)
			}
		}
		applyProviderSurfaceOwner(cfg, surface.ID, &surface)
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
			applyProviderSurfaceAvailability(cfg, id, provider, &surface)
			applyProviderSurfaceOwner(cfg, id, &surface)
			surface.Order = len(result)
			result = append(result, surface)
			seen[id] = struct{}{}
		}
	}
	return result
}

func applyProviderSurfaceOwner(cfg *Config, providerID string, surface *providerregistry.Surface) {
	owner, active := cfg.ProviderOwner(providerID)
	if !active {
		surface.Owner = nil
		return
	}
	surface.Owner = &owner
}

func applyProviderSurfaceAvailability(cfg *Config, id string, provider ProviderConfig, surface *providerregistry.Surface) {
	if cfg.isUnavailableRegisteredProvider(id) {
		surface.Available = false
		surface.Availability = "missing"
		switch {
		case provider.Preset != nil:
			surface.Diagnostic = fmt.Sprintf("provider preset %s is not installed; configuration, credentials, and model selections were preserved", provider.Preset.ID)
			if status, _, found := cfg.providerPluginAvailability(provider.Preset.ID); found {
				surface.Availability = string(status.State)
				if len(status.Diagnostics) > 0 {
					surface.Diagnostic = status.Diagnostics[0].Message
				} else {
					surface.Diagnostic = fmt.Sprintf("provider preset %s is %s; configuration, credentials, and model selections were preserved", provider.Preset.ID, status.State)
				}
			}
		case provider.Plugin != nil:
			surface.Diagnostic = fmt.Sprintf("provider plugin %s is not installed; configuration, credentials, and model selections were preserved", provider.Plugin.ID)
			if status, mode, found := cfg.providerPluginAvailability(provider.Plugin.ID); found {
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
			if err := cfg.ProviderRegistrationError(id); err != nil {
				surface.Availability = "invalid"
				surface.Diagnostic = err.Error()
			}
		default:
			surface.Diagnostic = "OAuth provider integration is not active; credentials, configuration, and model selections were preserved"
		}
		return
	}
	if provider.Disable {
		surface.Available = false
		surface.Availability = "disabled"
		surface.Diagnostic = "provider is disabled in configuration"
	}
}

// genericProviderSurface creates detached presentation metadata for custom or
// retained unavailable providers. It does not create a provider registration,
// validate credentials, or make the provider constructible.
func genericProviderSurface(id, name string, models []catalog.Model) providerregistry.Surface {
	if name == "" {
		name = id
	}
	return providerregistry.Surface{
		ID:           id,
		Name:         name,
		Available:    true,
		Availability: "available",
		Models:       append([]catalog.Model(nil), models...),
	}
}

func retainedProviderSurface(id string, provider ProviderConfig) providerregistry.Surface {
	surface := genericProviderSurface(id, provider.Name, provider.Models)
	surface.FlatRate = provider.FlatRate
	return surface
}

// ValidateProviderConfiguration validates namespaced values only when the exact
// provider manifest is active. Unknown or missing-plugin providers deliberately
// bypass validation so private fields survive absence losslessly. Do not apply
// another provider's schema or delete values that the current host cannot
// interpret.
func (c *Config) ValidateProviderConfiguration(providerID string, values map[string]any) error {
	registration, ok := c.ProviderRegistration(providerID)
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
