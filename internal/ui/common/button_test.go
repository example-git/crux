package common

import (
	"image/color"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestButtonUsesHoveredStyleForSelectedButtons(t *testing.T) {
	t.Parallel()

	var sty styles.Styles
	sty.Button.Blurred = lipgloss.NewStyle().Background(color.NRGBA{R: 0x11, G: 0x11, B: 0x11, A: 0xff})
	sty.Button.Focused = lipgloss.NewStyle().Background(color.NRGBA{R: 0x22, G: 0x22, B: 0x22, A: 0xff})
	sty.Button.Hovered = lipgloss.NewStyle().Background(color.NRGBA{R: 0x33, G: 0x33, B: 0x33, A: 0xff})

	tests := []struct {
		name     string
		selected bool
		hovered  bool
		style    lipgloss.Style
	}{
		{name: "unselected", style: sty.Button.Blurred},
		{name: "selected", selected: true, style: sty.Button.Focused},
		{name: "hovered", hovered: true, style: sty.Button.Hovered.Bold(true)},
		{name: "selected and hovered", selected: true, hovered: true, style: sty.Button.Hovered.Bold(true)},
	}

	rendered := make(map[string]string, len(tests))
	for _, test := range tests {
		got := Button(&sty, ButtonOpts{
			Text:           "Run",
			UnderlineIndex: -1,
			Selected:       test.selected,
			Hovered:        test.hovered,
		})
		want := test.style.Padding(0, 2).Render("Run")
		require.Equal(t, want, got, test.name)
		rendered[test.name] = got
	}

	require.NotEqual(t, rendered["selected"], rendered["selected and hovered"])
	require.Equal(t, rendered["hovered"], rendered["selected and hovered"])
}
