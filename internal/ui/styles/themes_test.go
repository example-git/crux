package styles

import (
	"image/color"
	"math"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/require"
)

func TestBrandHeadingContrastPreservesDecorativeColors(t *testing.T) {
	for _, hex := range []string{"#1B3B8B", "#1E8E3E", "#000000", "#202026", "#7FC4FF", "#FF8A3D"} {
		t.Run(hex, func(t *testing.T) {
			theme := CharmtonePantera()
			gradientA := lipgloss.Color(hex)
			gradientB := lipgloss.Color("#8CE99A")
			ApplyBrandAccents(&theme, gradientA, gradientB, gradientB)
			for _, style := range []lipgloss.Style{theme.Dialog.Title, theme.Dialog.TitleText, theme.Dialog.PrimaryText} {
				foreground := style.GetForeground()
				require.GreaterOrEqual(t, testContrast(foreground, theme.Background), 4.5)
				if testContrast(gradientA, theme.Background) >= 4.5 {
					assertColorEqual(t, "readable color unchanged", foreground, gradientA)
				} else {
					require.Greater(t, testLuminance(foreground), testLuminance(gradientA))
				}
			}
			assertColorEqual(t, "gradient A", theme.Dialog.TitleGradFromColor, gradientA)
			assertColorEqual(t, "gradient B", theme.Dialog.TitleGradToColor, gradientB)
			assertColorEqual(t, "selection", theme.Dialog.SelectedItem.GetBackground(), gradientB)
			assertColorEqual(t, "working gradient", theme.WorkingGradFromColor, gradientA)
		})
	}
}

func TestSecondaryAndSelectedTextContrast(t *testing.T) {
	theme := CharmtonePantera()
	for _, style := range []lipgloss.Style{theme.Dialog.SecondaryText, theme.Tool.SummaryMeta, theme.Tool.StateWaiting, theme.Tool.ParamKey} {
		require.GreaterOrEqual(t, testContrast(style.GetForeground(), theme.Tool.SummaryPanel.GetBackground()), 4.5)
	}
	ApplyBrandAccents(&theme, lipgloss.Color("#1B3B8B"), lipgloss.Color("#7FC4FF"), lipgloss.Color("#7FC4FF"))
	for _, style := range []lipgloss.Style{theme.Dialog.ListItem.InfoFocused, theme.Dialog.Sessions.InfoFocused} {
		require.GreaterOrEqual(t, testContrast(style.GetForeground(), theme.Dialog.SelectedItem.GetBackground()), 4.5)
	}
}

func TestMissingProviderBrandUsesCruxTheme(t *testing.T) {
	base := ThemeForProvider("")
	for _, id := range []string{"openai", "gemini", "copilot", "custom"} {
		theme := ThemeForProvider(id)
		assertColorEqual(t, id, theme.Dialog.TitleGradFromColor, base.Dialog.TitleGradFromColor)
		assertColorEqual(t, id, theme.Dialog.TitleGradToColor, base.Dialog.TitleGradToColor)
	}
}

func testContrast(foreground, background color.Color) float64 {
	first, second := testLuminance(foreground), testLuminance(background)
	return (max(first, second) + 0.05) / (min(first, second) + 0.05)
}

func testLuminance(value color.Color) float64 {
	r, g, b, _ := value.RGBA()
	linear := func(channel uint32) float64 {
		srgb := float64(channel) / 65535
		if srgb <= 0.04045 {
			return srgb / 12.92
		}
		return math.Pow((srgb+0.055)/1.055, 2.4)
	}
	return 0.2126*linear(r) + 0.7152*linear(g) + 0.0722*linear(b)
}
