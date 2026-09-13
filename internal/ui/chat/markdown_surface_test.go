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
	require.Equal(t, lipgloss.NoColor{}, sty.Messages.ThinkingBox.GetBackground())
	require.Nil(t, sty.ThinkingMarkdown.Document.BackgroundColor)
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

func TestThinkingDisclosureAndTransparentSurface(t *testing.T) {
	sty := styles.CharmtonePantera()
	for _, active := range []bool{false, true} {
		for _, width := range []int{40, 80} {
			thinking := "**SURFACE_MARKER**\n\nSecond *internal* thought\n\n**unfinished\n\nprefix **internal** suffix"
			msg := thinkingMessage("surface-thinking", thinking, "Done")
			if active {
				msg.Parts = []message.ContentPart{message.ReasoningContent{Thinking: thinking}}
			}
			item := NewAssistantMessageItem(&sty, msg).(*AssistantMessageItem)
			collapsed := item.cachedThinking(width)
			collapsedLines := strings.Split(ansi.Strip(collapsed), "\n")
			require.Len(t, collapsedLines, 3)
			require.Contains(t, collapsedLines[1], map[bool]string{false: "THOUGHTS ▾", true: "Thinking..."}[active])
			frameWidth := ansi.StringWidth(strings.TrimSpace(collapsedLines[0]))
			for _, line := range collapsedLines {
				trimmed := strings.TrimSpace(line)
				require.Equal(t, frameWidth, ansi.StringWidth(trimmed), line)
			}
			require.True(t, strings.HasPrefix(strings.TrimSpace(collapsedLines[1]), "│"))
			require.True(t, strings.HasSuffix(strings.TrimSpace(collapsedLines[1]), "│"))
			require.Equal(t, 3, item.thinkingBoxHeight)

			require.True(t, item.ToggleExpanded())
			expanded := item.cachedThinking(width)
			plainExpanded := ansi.Strip(expanded)
			expandedLines := strings.Split(plainExpanded, "\n")
			require.Contains(t, plainExpanded, "• SURFACE_MARKER")
			require.NotContains(t, plainExpanded, "**SURFACE_MARKER**")
			require.Contains(t, plainExpanded, "• Second *internal* thought")
			require.Contains(t, plainExpanded, "• **unfinished")
			require.Contains(t, plainExpanded, "• prefix **internal** suffix")
			require.Equal(t, frameWidth, ansi.StringWidth(strings.TrimSpace(expandedLines[0])))
			bottomRail := -1
			for index, line := range expandedLines {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "• ") {
					require.NotContains(t, trimmed, "│")
					require.NotContains(t, trimmed, "├")
					require.NotContains(t, trimmed, "┤")
				}
				if strings.HasPrefix(trimmed, "╰") {
					bottomRail = index
					require.Equal(t, frameWidth, ansi.StringWidth(trimmed))
				}
			}
			require.Positive(t, bottomRail)
			require.Equal(t, bottomRail+1, item.thinkingBoxHeight)
			require.True(t, item.HandleMouseClick(ansi.MouseLeft, 4, 0))
			require.False(t, item.HandleMouseClick(ansi.MouseLeft, 4, item.thinkingBoxHeight))

			buffer := uv.NewScreenBuffer(width, lipgloss.Height(expanded))
			uv.NewStyledString(expanded).Draw(&buffer, buffer.Bounds())
			for y := range buffer.Height() {
				for x := range width {
					cell := buffer.CellAt(x, y)
					require.Nil(t, cell.Style.Bg, "cell %d,%d", x, y)
				}
			}
		}
	}
}
