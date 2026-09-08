package brand

import (
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestForProviderUsesDefaultWithoutLoadedBrand(t *testing.T) {
	for _, providerID := range []string{"", "openai", "gemini", "copilot", "custom"} {
		t.Run(providerID, func(t *testing.T) {
			assert.Nil(t, ForProvider(providerID))
			assert.Nil(t, FromSurface(providerregistry.Surface{ID: providerID}))
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
