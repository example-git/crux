package providerregistry

import (
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestSurfacesProjectDeclarativeMetadataWithoutPluginBehavior(t *testing.T) {
	manifestValue := manifest.Manifest{
		Provider: manifest.Provider{
			ID: "synthetic", Name: "Synthetic", Description: "Synthetic provider", FlatRate: true,
			DefaultLargeModel: "large", DefaultSmallModel: "small",
		},
		Configuration: manifest.Configuration{
			Schema: map[string]any{"type": "object", "properties": map[string]any{"region": map[string]any{"type": "string"}}},
			Fields: map[string]manifest.FieldDisplay{"region": {Label: "Region", Order: 10}},
		},
	}
	registry, err := New(Registration{
		ProviderID: "synthetic",
		Name:       "Synthetic",
		Manifest:   &manifestValue,
		Brand:      &Brand{ShortName: "SYNTH", Color: "#123456"},
		OAuth:      &OAuthCapability{FlowID: "login", Adapter: LoginBrowser},
		RuntimeControls: []manifest.RuntimeControl{{
			ID: "effort", Label: "Effort", Type: "enum", Values: []string{"low", "high"}, RequestPath: "/reasoning/effort",
		}},
	})
	require.NoError(t, err)

	surfaces := registry.Surfaces([]catwalk.Provider{{
		ID: "synthetic", Name: "Synthetic", DefaultLargeModelID: "large", DefaultSmallModelID: "small",
		Models: []catwalk.Model{{ID: "large"}, {ID: "small"}},
	}}, map[string]string{"synthetic": "large"})
	require.Len(t, surfaces, 1)
	surface := surfaces[0]
	require.Equal(t, "Synthetic provider", surface.Description)
	require.True(t, surface.FlatRate)
	require.Equal(t, "SYNTH", surface.Brand.ShortName)
	require.Equal(t, "Region", surface.ConfigurationUI["region"].Label)
	require.Equal(t, []string{"large", "small"}, []string{surface.Models[0].ID, surface.Models[1].ID})
	require.Len(t, surface.Authentication, 1)
	require.False(t, surface.Authentication[0].Available)
	require.Equal(t, "host OAuth interpreter unavailable", surface.Authentication[0].Diagnostic)
	require.Len(t, surface.RuntimeControls, 1)
	require.False(t, surface.RuntimeControls[0].Available)

	surface.Configuration["type"] = "mutated"
	surface.RuntimeControls[0].Values[0] = "mutated"
	again := registry.Surfaces(nil, nil)[0]
	require.Equal(t, "object", again.Configuration["type"])
	require.Equal(t, "low", again.RuntimeControls[0].Values[0])
}

func TestSurfacesMarkHostOwnedRuntimeControlsAvailable(t *testing.T) {
	registry, err := New(Registration{
		ProviderID: "integrated",
		Name:       "Integrated",
		RuntimeControls: []manifest.RuntimeControl{{
			ID: "verbosity", Type: "enum", Values: []string{"low", "high"},
		}},
		Runtime: &RuntimeCapability{Available: func(modelID string) bool { return modelID == "supported" }},
	})
	require.NoError(t, err)

	surface := registry.Surfaces(nil, map[string]string{"integrated": "supported"})[0]
	require.True(t, surface.RuntimeControls[0].Available)
}
