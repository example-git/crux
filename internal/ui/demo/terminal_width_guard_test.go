package demo

import (
	"bytes"
	"fmt"
	"image/color"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/stretchr/testify/require"
)

func TestTerminalWidthGuardPreservesAdjacentCells(t *testing.T) {
	var terminal headlessRenderer
	t.Cleanup(func() {
		if terminal.ctx != nil {
			terminal.ctx.Close()
		}
	})
	for _, fullscreen := range []bool{false, true} {
		for _, grapheme := range []bool{false, true} {
			t.Run(fmt.Sprintf("fullscreen=%v/grapheme=%v", fullscreen, grapheme), func(t *testing.T) {
				const cols, rows = 45, 15
				capture := retainedTerminalCapture(t, &terminal, cols, rows)
				height, offset := rows, 0
				if !fullscreen {
					height, offset = 5, 5
					capture("\x1b[HHEADER\x1b[6;1H")
				}
				var output bytes.Buffer
				renderer := uv.NewTerminalRenderer(&output, []string{"TERM=xterm-256color"})
				renderer.SetColorProfile(colorprofile.TrueColor)
				renderer.SetFullscreen(fullscreen)
				renderer.SetRelativeCursor(!fullscreen)
				renderer.SetScrollOptim(true)
				renderer.SetGraphemeWidth(grapheme)
				output.WriteString("\x1b[?7h")
				for stage := 0; stage < 6; stage++ {
					buf := uv.NewScreenBuffer(cols, height)
					for y := 0; y < height; y++ {
						for x := 0; x < cols; x++ {
							buf.SetCell(x, y, &uv.Cell{Content: string(rune('A' + y)), Width: 1})
						}
					}
					glyphX, glyph := -1, ""
					switch stage {
					case 1:
						glyphX, glyph = 22, "👩‍💻" // The virtual terminal consumes four columns.
					case 2:
						glyphX, glyph = cols-2, "👩‍💻" // Must not wrap or scroll at bottom-right.
					case 3:
						glyphX, glyph = 22, "⚙️" // The virtual terminal consumes only one column.
					}
					if glyphX >= 0 {
						buf.SetCell(glyphX, height-1, &uv.Cell{Content: glyph, Width: 2, Style: uv.Style{Bg: color.RGBA{17, 34, 51, 255}}})
					}
					renderer.Render(buf.RenderBuffer)
					require.NoError(t, renderer.Flush())
					if stage == 5 {
						require.Empty(t, output.String(), "identical screen must not repaint")
					}
					got := capture(output.String())
					output.Reset()
					actual := map[[2]int]terminalCell{}
					for _, cell := range got.Cells {
						actual[[2]int{cell.X, cell.Y}] = cell
					}
					for y := 0; y < height; y++ {
						for x := 0; x < cols; x++ {
							if y == height-1 && glyphX >= 0 && x >= glyphX && x < glyphX+2 {
								continue // Glyph shaping differs; cells outside its allocation must not.
							}
							cell := actual[[2]int{x, y + offset}]
							require.Equal(t, string(rune('A'+y)), cell.Text, "stage=%d cell=(%d,%d)", stage, x, y)
							require.Equal(t, 1, cell.Width, "stage=%d cell=(%d,%d)", stage, x, y)
						}
					}
					if stage == 3 {
						padding := actual[[2]int{glyphX + 1, height - 1 + offset}]
						require.Empty(t, strings.TrimSpace(padding.Text), "narrow glyph retained stale text in its second column")
						require.True(t, padding.BGRGB)
						require.Equal(t, 0x112233, padding.BG)
					}
					if !fullscreen {
						require.Equal(t, "HEADER", got.Lines[0], "inline rendering moved into preceding output")
					}
				}
			})
		}
	}
}
