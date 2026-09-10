package common

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestMarkdownCodeBlockBackgroundIsDistinct(t *testing.T) {
	InvalidateMarkdownRendererCache()
	t.Cleanup(InvalidateMarkdownRendererCache)
	sty := styles.CharmtonePantera()
	background := lipgloss.Color(*sty.Markdown.CodeBlock.BackgroundColor)
	require.NotEqual(t, sty.Background, background)
	r, g, b, a := background.RGBA()
	wr, wg, wb, wa := sty.PanelBackground.RGBA()
	require.Equal(t, []uint32{wr, wg, wb, wa}, []uint32{r, g, b, a})
	wr, wg, wb, wa = sty.Tool.SummaryPanel.GetBackground().RGBA()
	require.Equal(t, []uint32{wr, wg, wb, wa}, []uint32{r, g, b, a})
	for _, language := range []string{"go", "text", ""} {
		renderer := MarkdownRenderer(&sty, 60)
		rendered, err := renderer.Render("Prose before.\n\n```" + language + "\nprintln(42)\n```\n\nProse after.")
		require.NoError(t, err)
		buffer := uv.NewScreenBuffer(60, lipgloss.Height(rendered))
		uv.NewStyledString(rendered).Draw(&buffer, buffer.Bounds())
		found := false
		for y, line := range strings.Split(ansi.Strip(rendered), "\n") {
			start := strings.Index(line, "println(42)")
			if start < 0 {
				continue
			}
			found = true
			for x := start; x < start+len("println(42)"); x++ {
				cell := buffer.CellAt(x, y)
				require.NotNil(t, cell.Style.Bg, "%s at %d,%d", language, x, y)
				r, g, b, _ := cell.Style.Bg.RGBA()
				wr, wg, wb, _ := background.RGBA()
				require.Equal(t, []uint32{wr, wg, wb}, []uint32{r, g, b})
			}
		}
		require.True(t, found, language)
	}
}
