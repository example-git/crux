package providerregistry

import (
	"maps"
	"slices"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

// Brand is display-only provider presentation metadata. It contains no
// endpoint, credential, or implementation information.
type Brand struct {
	Label     string `json:"label,omitempty"`
	ShortName string `json:"short_name,omitempty"`
	Color     string `json:"color,omitempty"`
	GradientA string `json:"gradient_a,omitempty"`
	GradientB string `json:"gradient_b,omitempty"`
}

// Authentication describes one declared interactive authentication surface.
// Available is false when the declaration is valid but its host-owned
// interpreter is not implemented. Declarative bundles never supply behavior.
type Authentication struct {
	Kind       string       `json:"kind"`
	FlowID     string       `json:"flow_id,omitempty"`
	Adapter    LoginAdapter `json:"adapter,omitempty"`
	Available  bool         `json:"available"`
	Diagnostic string       `json:"diagnostic,omitempty"`
}

// InstructionSurface contains immutable validated profile text for rendering
// and preview. Bundle paths are intentionally absent.
type InstructionSurface struct {
	Default  string            `json:"default"`
	Profiles map[string]string `json:"profiles"`
}

// RuntimeControlSurface is a declarative control plus whether the selected
// model can currently apply it through a host-owned adapter.
type RuntimeControlSurface struct {
	manifest.RuntimeControl
	Available bool `json:"available"`
}

// Surface is the redacted, serializable provider presentation contract shared
// by local TUI, remote clients, shell/schema generation, and tests.
type Surface struct {
	ID                string                           `json:"id"`
	Name              string                           `json:"name"`
	Available         bool                             `json:"available"`
	Availability      string                           `json:"availability"`
	Diagnostic        string                           `json:"diagnostic,omitempty"`
	Description       string                           `json:"description,omitempty"`
	Order             int                              `json:"order"`
	FlatRate          bool                             `json:"flat_rate,omitempty"`
	Brand             *Brand                           `json:"brand,omitempty"`
	DefaultLargeModel string                           `json:"default_large_model,omitempty"`
	DefaultSmallModel string                           `json:"default_small_model,omitempty"`
	Models            []catwalk.Model                  `json:"models,omitempty"`
	Authentication    []Authentication                 `json:"authentication,omitempty"`
	Configuration     map[string]any                   `json:"configuration_schema,omitempty"`
	ConfigurationUI   map[string]manifest.FieldDisplay `json:"configuration_fields,omitempty"`
	Images            *manifest.ImagePolicy            `json:"images,omitempty"`
	Instructions      *InstructionSurface              `json:"instructions,omitempty"`
	RuntimeControls   []RuntimeControlSurface          `json:"runtime_controls,omitempty"`
	UsageAvailable    bool                             `json:"usage_available,omitempty"`
}

// Clone returns a deep copy safe for independent consumers and JSON transport.
func (s Surface) Clone() Surface {
	s.Models = cloneJSON(s.Models)
	s.Authentication = slices.Clone(s.Authentication)
	s.Configuration = cloneJSON(s.Configuration)
	s.ConfigurationUI = maps.Clone(s.ConfigurationUI)
	if s.Brand != nil {
		value := *s.Brand
		s.Brand = &value
	}
	if s.Images != nil {
		value := cloneJSON(*s.Images)
		s.Images = &value
	}
	if s.Instructions != nil {
		value := *s.Instructions
		value.Profiles = maps.Clone(s.Instructions.Profiles)
		s.Instructions = &value
	}
	s.RuntimeControls = cloneJSON(s.RuntimeControls)
	return s
}

// Surfaces projects registry registrations and the effective catalog into one
// ordered redacted presentation contract. selectedModels maps provider IDs to
// their currently selected model ID for model-scoped availability checks.
func (r *Registry) Surfaces(catalog []catwalk.Provider, selectedModels map[string]string) []Surface {
	if r == nil {
		return nil
	}
	catalogByID := make(map[string]catwalk.Provider, len(catalog))
	for _, provider := range catalog {
		catalogByID[string(provider.ID)] = provider
	}
	registrations := r.Registrations()
	result := make([]Surface, 0, len(registrations))
	for order, registration := range registrations {
		surface := Surface{ID: registration.ProviderID, Name: registration.Name, Available: true, Availability: "available", Order: order, UsageAvailable: registration.Quota != nil || registration.Usage != nil}
		if registration.Manifest != nil {
			provider := registration.Manifest.Provider
			surface.Description = provider.Description
			surface.FlatRate = provider.FlatRate
			surface.DefaultLargeModel = provider.DefaultLargeModel
			surface.DefaultSmallModel = provider.DefaultSmallModel
			surface.Configuration = cloneJSON(registration.Manifest.Configuration.Schema)
			surface.ConfigurationUI = maps.Clone(registration.Manifest.Configuration.Fields)
		}
		if registration.Brand != nil {
			brand := *registration.Brand
			surface.Brand = &brand
		}
		if provider, ok := catalogByID[registration.ProviderID]; ok {
			surface.Models = cloneJSON(provider.Models)
			if surface.DefaultLargeModel == "" {
				surface.DefaultLargeModel = provider.DefaultLargeModelID
			}
			if surface.DefaultSmallModel == "" {
				surface.DefaultSmallModel = provider.DefaultSmallModelID
			}
		}
		if registration.OAuth != nil {
			available := registration.OAuth.Authorize != nil || (registration.OAuth.RequestDeviceCode != nil && registration.OAuth.PollDeviceCode != nil)
			auth := Authentication{Kind: "oauth2", FlowID: registration.OAuth.FlowID, Adapter: registration.OAuth.Adapter, Available: available}
			if !available {
				auth.Diagnostic = "host OAuth interpreter unavailable"
			}
			surface.Authentication = append(surface.Authentication, auth)
		}
		if registration.Images != nil {
			value := cloneJSON(*registration.Images)
			surface.Images = &value
		}
		if registration.Instructions != nil {
			surface.Instructions = &InstructionSurface{Default: registration.Instructions.Default, Profiles: maps.Clone(registration.Instructions.Profiles)}
		}
		modelID := selectedModels[registration.ProviderID]
		available := registration.Runtime != nil && registration.Runtime.Available != nil && registration.Runtime.Available(modelID)
		for _, control := range registration.RuntimeControls {
			surface.RuntimeControls = append(surface.RuntimeControls, RuntimeControlSurface{RuntimeControl: cloneJSON(control), Available: available})
		}
		result = append(result, surface)
	}
	return result
}

// LookupSurface returns a deep-cloned surface by canonical provider ID.
func LookupSurface(surfaces []Surface, providerID string) (Surface, bool) {
	for _, surface := range surfaces {
		if surface.ID == providerID {
			return surface.Clone(), true
		}
	}
	return Surface{}, false
}
