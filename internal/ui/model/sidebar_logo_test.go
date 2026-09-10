package model

import (
	"image"
	"testing"

	"charm.land/lipgloss/v2"
	tea "github.com/example-git/crux/foundation/bubbletea"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/stretchr/testify/require"
)

func TestSidebarLogoClickTogglesProviderAndCruxBranding(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.layout.sidebar = image.Rect(10, 5, 42, 35)
	ui.brand = &providerBrand{
		Title:  "CODEX",
		GradA:  lipgloss.Color("#111111"),
		GradB:  lipgloss.Color("#222222"),
		Accent: lipgloss.Color("#333333"),
	}
	ui.sidebarBrandLogoHeight = 6
	ui.cacheSidebarLogo(ui.layout.sidebar.Dx())
	providerLogo := ui.sidebarLogo

	require.Same(t, ui.brand, ui.sidebarBrand())
	require.True(t, ui.handleSidebarLogoClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      7,
		Button: uv.MouseLeft,
	})))
	require.True(t, ui.sidebarShowCruxLogo)
	require.Nil(t, ui.sidebarBrand())
	require.NotEqual(t, providerLogo, ui.sidebarLogo)

	require.True(t, ui.handleSidebarLogoClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      7,
		Button: uv.MouseLeft,
	})))
	require.False(t, ui.sidebarShowCruxLogo)
	require.Same(t, ui.brand, ui.sidebarBrand())
	require.Equal(t, providerLogo, ui.sidebarLogo)
}

func TestSidebarLogoClickIgnoresUnavailableAndNonLogoClicks(t *testing.T) {
	t.Parallel()

	ui := newTestUI()
	ui.layout.sidebar = image.Rect(10, 5, 42, 35)
	ui.sidebarBrandLogoHeight = 6
	leftClick := tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      7,
		Button: uv.MouseLeft,
	})

	require.False(t, ui.handleSidebarLogoClick(leftClick))

	ui.brand = &providerBrand{Title: "CODEX"}
	require.False(t, ui.handleSidebarLogoClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      12,
		Button: uv.MouseLeft,
	})))
	require.False(t, ui.handleSidebarLogoClick(tea.MouseClickMsg(tea.Mouse{
		X:      12,
		Y:      7,
		Button: uv.MouseRight,
	})))
	require.False(t, ui.sidebarShowCruxLogo)
}
