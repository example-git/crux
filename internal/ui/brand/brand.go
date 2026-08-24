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
		title = surface.Name
	}
	gradientA := brand.GradientA
	if gradientA == "" {
		gradientA = brand.Color
	}
	gradientB := brand.GradientB
	if gradientB == "" {
		gradientB = brand.Color
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
	switch providerID {
	case "openai":
		return &Provider{
			Title:  "CODEX",
			GradA:  lipgloss.Color("#1B3B8B"),
			GradB:  lipgloss.Color("#7FC4FF"),
			Accent: lipgloss.Color("#7FC4FF"),
		}
	case "gemini":
		return &Provider{
			Title:  "GEMINI",
			GradA:  lipgloss.Color("#1E8E3E"),
			GradB:  lipgloss.Color("#8CE99A"),
			Accent: lipgloss.Color("#8CE99A"),
		}
	case "copilot":
		return &Provider{
			Title:  "COPILOT",
			GradA:  lipgloss.Color("#C9A8FF"),
			GradB:  lipgloss.Color("#C9A8FF"),
			Accent: lipgloss.Color("#C9A8FF"),
		}
	default:
		return nil
	}
}
