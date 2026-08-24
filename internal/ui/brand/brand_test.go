package brand

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForProviderUsesLightAccent(t *testing.T) {
	tests := map[string]color.RGBA{
		"openai":  {R: 127, G: 196, B: 255, A: 255},
		"gemini":  {R: 140, G: 233, B: 154, A: 255},
		"copilot": {R: 201, G: 168, B: 255, A: 255},
	}

	for providerID, expected := range tests {
		t.Run(providerID, func(t *testing.T) {
			provider := ForProvider(providerID)
			require.NotNil(t, provider)
			assert.Equal(t, expected, provider.Accent)
		})
	}
}

func TestForProviderUnknown(t *testing.T) {
	assert.Nil(t, ForProvider("custom"))
}

func TestFromSurfaceUsesDeclarativeBrand(t *testing.T) {
	provider := FromSurface(providerregistry.Surface{
		ID:   "synthetic",
		Name: "Synthetic Provider",
		Brand: &providerregistry.Brand{
			ShortName: "SYNTH",
			Color:     "#123456",
		},
	})

	require.NotNil(t, provider)
	assert.Equal(t, "SYNTH", provider.Title)
	assert.Equal(t, lipgloss.Color("#123456"), provider.GradA)
	assert.Equal(t, lipgloss.Color("#123456"), provider.GradB)
	assert.Equal(t, lipgloss.Color("#123456"), provider.Accent)
}
