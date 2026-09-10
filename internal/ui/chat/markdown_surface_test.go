package chat

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestExpandableMarkdownUsesPanelSurface(t *testing.T) {
	common.InvalidateMarkdownRendererCache()
	t.Cleanup(common.InvalidateMarkdownRendererCache)
	sty := styles.CharmtonePantera()
	for _, expanded := range []bool{false, true} {
		output := toolOutputMarkdownContent(&sty, "**SURFACE_MARKER**\n\n"+strings.Repeat("Paragraph\n\n", 20), 60, expanded)
		buffer := uv.NewScreenBuffer(60, lipgloss.Height(output))
		uv.NewStyledString(output).Draw(&buffer, buffer.Bounds())
		found := false
		for y, line := range strings.Split(ansi.Strip(output), "\n") {
			x := strings.Index(line, "SURFACE_MARKER")
			if x < 0 {
				continue
			}
			found = true
			for offset := range len("SURFACE_MARKER") {
				cell := buffer.CellAt(x+offset, y)
				require.NotNil(t, cell)
				require.NotNil(t, cell.Style.Bg)
				r, g, b, _ := cell.Style.Bg.RGBA()
				wr, wg, wb, _ := sty.PanelBackground.RGBA()
				require.Equal(t, []uint32{wr, wg, wb}, []uint32{r, g, b})
			}
		}
		require.True(t, found)
	}
	require.Equal(t, sty.Background, sty.Messages.ThinkingBox.GetBackground())
}

func TestOutputFootersConnectToBody(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, width := range []int{32, 80} {
		for _, expanded := range []bool{false, true} {
			for _, markdown := range []bool{false, true} {
				content := strings.Repeat("- Body line\n", 30)
				output := summaryTextResult(&sty, content, toolBodyWidth(&sty, width), expanded)
				if markdown {
					output = toolOutputMarkdownContent(&sty, content, width, expanded)
				}
				lines := strings.Split(ansi.Strip(output), "\n")
				buffer := uv.NewScreenBuffer(width, len(lines))
				uv.NewStyledString(output).Draw(&buffer, buffer.Bounds())
				footerRow := -1
				for y, line := range lines {
					if strings.Contains(line, "Full output") {
						footerRow = y
						break
					}
				}
				require.Positive(t, footerRow)
				require.NotEmpty(t, strings.TrimSpace(lines[footerRow-1]))
				tabEnd := min(width, sty.Tool.Body.GetPaddingLeft()+ansi.StringWidth(summaryDisclosure("Full output", expanded))+2)
				for x := sty.Tool.Body.GetPaddingLeft(); x < tabEnd; x++ {
					body := buffer.CellAt(x, footerRow-1)
					footer := buffer.CellAt(x, footerRow)
					require.NotNil(t, body.Style.Bg)
					require.NotNil(t, footer.Style.Bg)
					br, bg, bb, _ := body.Style.Bg.RGBA()
					pr, pg, pb, _ := sty.PanelBackground.RGBA()
					require.Equal(t, []uint32{pr, pg, pb}, []uint32{br, bg, bb})
					fr, fg, fb, _ := footer.Style.Bg.RGBA()
					wr, wg, wb, _ := sty.Tool.SummaryHint.GetForeground().RGBA()
					require.Equal(t, []uint32{wr, wg, wb}, []uint32{fr, fg, fb})
				}
			}
		}
	}
}

func TestThinkingDisclosureAndNormalSurface(t *testing.T) {
	common.InvalidateMarkdownRendererCache()
	t.Cleanup(common.InvalidateMarkdownRendererCache)
	sty := styles.CharmtonePantera()
	for _, active := range []bool{false, true} {
		for _, width := range []int{40, 80} {
			msg := thinkingMessage("surface-thinking", "**SURFACE_MARKER**\n\nSecond thought", "Done")
			if active {
				msg.Parts = []message.ContentPart{message.ReasoningContent{Thinking: "**SURFACE_MARKER**\n\nSecond thought"}}
			}
			item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
			item.RawRender(width)
			collapsed := ansi.Strip(item.thinkingSec.out)
			require.Equal(t, "╰── Expand Thoughts ▾", strings.TrimSpace(collapsed))
			require.Equal(t, 1, item.thinkingBoxHeight)
			require.True(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))
			require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, 1))
			require.True(t, item.ToggleExpanded())
			item.RawRender(width)
			output := item.thinkingSec.out
			require.Contains(t, ansi.Strip(output), "SURFACE_MARKER")
			require.Contains(t, ansi.Strip(output), "├──")
			require.Contains(t, ansi.Strip(output), "╰──")
			buffer := uv.NewScreenBuffer(width, lipgloss.Height(output))
			uv.NewStyledString(output).Draw(&buffer, buffer.Bounds())
			for y := range buffer.Height() {
				for x := range width {
					cell := buffer.CellAt(x, y)
					if cell.Style.Bg == nil {
						continue
					}
					r, g, b, _ := cell.Style.Bg.RGBA()
					wr, wg, wb, _ := sty.Background.RGBA()
					require.Equal(t, []uint32{wr, wg, wb}, []uint32{r, g, b}, "cell %d,%d", x, y)
				}
			}
			require.False(t, item.ToggleExpanded())
			item.RawRender(width)
			require.Equal(t, collapsed, ansi.Strip(item.thinkingSec.out))
		}
	}
}
