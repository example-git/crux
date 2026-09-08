package brand

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/providerregistry"
)

type Provider struct {
	Title  string
	GradA  color.Color
	GradB  color.Color
	Accent color.Color
}

func FromSurface(surface providerregistry.Surface) *Provider {
	if surface.Brand == nil {
		return ForProvider(surface.ID)
	}
	brand := surface.Brand
	title := brand.ShortName
	if title == "" {
		title = brand.Label
	}
	if title == "" {
		title = "CRUX"
	}
	gradientA := brand.GradientA
	if gradientA == "" {
		gradientA = brand.Color
	}
	if gradientA == "" {
		gradientA = "#39FF14"
	}
	gradientB := brand.GradientB
	if gradientB == "" {
		gradientB = brand.Color
	}
	if gradientB == "" {
		gradientB = "#FF3B1F"
	}
	accent := brand.Color
	if accent == "" {
		accent = gradientA
	}
	return &Provider{
		Title:  title,
		GradA:  lipgloss.Color(gradientA),
		GradB:  lipgloss.Color(gradientB),
		Accent: lipgloss.Color(accent),
	}
}

func ForProvider(providerID string) *Provider {
	return nil
}
