package chat

import (
	"fmt"
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func alignmentColorEqual(a, b color.Color) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	ar, ag, ab, aa := a.RGBA()
	br, bg, bb, ba := b.RGBA()
	return ar == br && ag == bg && ab == bb && aa == ba
}

func TestToolPanelAlignment(t *testing.T) {
	content := strings.Repeat("Alignment marker with enough content to wrap consistently.\n", 30)
	for _, inset := range []int{2, 3} {
		sty := styles.CharmtonePantera()
		sty.Tool.Body = sty.Tool.Body.PaddingLeft(inset)
		for _, width := range []int{40, 80, 160} {
			for _, expanded := range []bool{false, true} {
				for _, fixture := range []struct{ name, tool, input, content string }{
					{"fetch-markdown", "fetch", `{"url":"https://example.com","format":"markdown"}`, content},
					{"fetch-text", "fetch", `{"url":"https://example.com","format":"text"}`, content},
					{"generic-plain", "custom_tool", `{}`, content},
					{"generic-markdown", "custom_tool", `{}`, "# Heading\n\n" + content},
					{"generic-json", "custom_tool", `{}`, `{"values":[1,2,3,4,5,6,7,8,9,10,11,12]}`},
					{"bash", "bash", `{"command":"printf output"}`, content},
					{"view", "view", `{"file_path":"/workspace/output.txt"}`, content},
					{"job-output", "job_output", `{"shell_id":"123","wait":false}`, content},
					{"task-output", "task_output", `{"task_id":"a123","wait":false}`, content},
				} {
					t.Run(fmt.Sprintf("%s/%d/%d/%t", fixture.name, inset, width, expanded), func(t *testing.T) {
						item := summaryTestItem(&sty, fixture.tool, fixture.input, fixture.content, "")
						if expanded {
							item.(Expandable).ToggleExpanded()
						}
						output := item.Render(width)
						requireSummaryWidth(t, output, width)
						buffer := uv.NewScreenBuffer(width, lipgloss.Height(output))
						uv.NewStyledString(output).Draw(&buffer, buffer.Bounds())
						wantLeft := MessageLeftPaddingTotal + inset
						wantRight := MessageLeftPaddingTotal + min(width-MessageLeftPaddingTotal, maxTextWidth)
						panelRows, footerRows := 0, 0
						for y := 0; y < buffer.Height(); y++ {
							left, right, footerLeft := -1, -1, -1
							for x := 0; x < width; x++ {
								cell := buffer.CellAt(x, y)
								if cell == nil {
									continue
								}
								if alignmentColorEqual(cell.Style.Bg, sty.PanelBackground) {
									if left < 0 {
										left = x
									}
									right = x + 1
								}
								if footerLeft < 0 && alignmentColorEqual(cell.Style.Bg, sty.Tool.SummaryHint.GetForeground()) {
									footerLeft = x
								}
							}
							if left >= 0 {
								require.Equal(t, wantLeft, left, "panel left at row %d", y)
								require.Equal(t, wantRight, right, "panel right at row %d", y)
								panelRows++
							}
							if footerLeft >= 0 {
								require.Equal(t, wantLeft, footerLeft, "footer left at row %d", y)
								footerRows++
							}
						}
						require.Positive(t, panelRows)
						require.Positive(t, footerRows)
					})
				}
			}
		}
	}
}

func TestThinkingSurfaceAlignsWithOuterMessageLayer(t *testing.T) {
	for _, inset := range []int{2, 3} {
		sty := styles.CharmtonePantera()
		sty.Tool.Body = sty.Tool.Body.PaddingLeft(inset)
		for _, width := range []int{40, 80, 160} {
			item := NewAssistantMessageItem(&sty, thinkingMessage("thinking", "First\nSecond\nThird", "Answer")).(*AssistantMessageItem)
			for _, mode := range []thinkingViewMode{thinkingCollapsed, thinkingFullExpanded} {
				for _, focused := range []bool{false, true} {
					item.thinkingViewMode = mode
					item.SetFocused(focused)
					item.clearCache()
					output := item.Render(width)
					requireSummaryWidth(t, output, width)
					buffer := uv.NewScreenBuffer(width, lipgloss.Height(output))
					uv.NewStyledString(output).Draw(&buffer, buffer.Bounds())
					wantFrameLeft := MessageLeftPaddingTotal + sty.Messages.ThinkingBox.GetPaddingLeft()
					frameRows := 0
					footerRows := 0
					for y, line := range strings.Split(ansi.Strip(output), "\n") {
						if index := strings.IndexAny(line, "╭│•╰"); index >= 0 {
							frameLeft := ansi.StringWidth(line[:index])
							require.Equal(t, wantFrameLeft, frameLeft, "inset=%d width=%d mode=%d focused=%t line=%q", inset, width, mode, focused, line)
							frameRows++
						}
						if index := strings.Index(line, "Thought for"); index >= 0 {
							footerLeft := ansi.StringWidth(line[:index])
							require.Equal(t, wantFrameLeft, footerLeft, "inset=%d width=%d mode=%d focused=%t line=%q", inset, width, mode, focused, line)
							footerRows++
						}
						for x := 0; x < width; x++ {
							cell := buffer.CellAt(x, y)
							if cell != nil {
								require.Nil(t, cell.Style.Bg, "thinking background at %d,%d", x, y)
							}
						}
					}
					require.Positive(t, frameRows)
					if mode == thinkingFullExpanded {
						require.Positive(t, footerRows)
					} else {
						require.Zero(t, footerRows)
					}
				}
			}
		}
	}
}
