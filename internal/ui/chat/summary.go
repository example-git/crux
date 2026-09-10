package chat

import (
	"image/color"
	"strings"

	"github.com/charmbracelet/x/ansi"
	uv "github.com/example-git/crux/foundation/ultraviolet"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

func toolBodyWidth(sty *styles.Styles, width int) int {
	return max(1, width-sty.Tool.Body.GetHorizontalFrameSize())
}

func summaryContentWidth(sty *styles.Styles, width int) int {
	return max(1, width-sty.Tool.SummaryPanel.GetHorizontalFrameSize())
}

func summaryClean(content string) string {
	return strings.ReplaceAll(ansi.Strip(common.StripCursorControl(content)), "\t", "    ")
}

func summaryPreview(content string, width int) string {
	content = strings.Join(strings.Fields(content), " ")
	if ansi.StringWidth(content) <= width {
		return content
	}
	preview := ansi.Truncate(content, max(1, width), "…")
	if cut := strings.LastIndex(preview, " "); cut > len(preview)/2 {
		preview = strings.TrimSpace(preview[:cut]) + "…"
	}
	return preview
}

func summaryWrap(content string, width int) string {
	return ansi.Wrap(content, max(1, width), "")
}

func summaryDisclosure(detail string, expanded bool) string {
	action := "click or Space to expand"
	if expanded {
		action = "click or Space to collapse"
	}
	if detail != "" {
		return detail + " · " + action
	}
	return action
}

func renderOutputFooter(sty *styles.Styles, width int, footer string, background color.Color) string {
	width = max(1, width)
	footerBackground := sty.Tool.SummaryHint.GetForeground()
	width = min(width, ansi.StringWidth(footer)+2)
	padding := min(1, (width-1)/2)
	return sty.Tool.SummaryHint.
		Foreground(styles.ReadableText(background, footerBackground)).
		Background(footerBackground).
		Padding(0, padding).
		Width(width).
		Render(summaryWrap(footer, max(1, width-2*padding)))
}

func renderSummaryCard(sty *styles.Styles, width int, rows []string, footer string) string {
	return sty.Tool.Body.Render(renderSummaryPanel(sty, width, rows, footer))
}

func renderSummaryPanel(sty *styles.Styles, width int, rows []string, footer string) string {
	width = max(1, width)
	innerWidth := summaryContentWidth(sty, width)
	panel := sty.Tool.SummaryPanel
	if width <= panel.GetHorizontalFrameSize() {
		panel = panel.Padding(0)
		innerWidth = width
	}
	lines := strings.Split(summaryWrap(strings.Join(rows, "\n"), innerWidth), "\n")
	body := panel.Width(width).Render(strings.Join(lines, "\n"))
	buffer := uv.NewScreenBuffer(width, len(lines))
	uv.NewStyledString(body).Draw(&buffer, buffer.Bounds())
	for y := 0; y < buffer.Height(); y++ {
		for x := 0; x < buffer.Width(); {
			cell := uv.Cell{Content: " ", Width: 1}
			if existing := buffer.CellAt(x, y); existing != nil && !existing.IsZero() {
				cell = *existing
			}
			if cell.Style.Bg == nil {
				cell.Style.Bg = panel.GetBackground()
				buffer.SetCell(x, y, &cell)
			}
			x += max(1, cell.Width)
		}
	}
	body = buffer.Render()
	if footer != "" {
		body += "\n" + renderOutputFooter(sty, width, footer, panel.GetBackground())
	}
	return body
}

func summaryTextResult(sty *styles.Styles, content string, width int, expanded bool) string {
	lines := strings.Split(strings.TrimSpace(summaryClean(content)), "\n")
	limit := len(lines)
	if !expanded {
		limit = min(limit, responseContextHeight)
	}
	innerWidth := summaryContentWidth(sty, width)
	rows := make([]string, 0, limit)
	clipped := false
	for _, line := range lines[:limit] {
		if expanded {
			line = summaryWrap(line, innerWidth)
		} else {
			clipped = clipped || ansi.StringWidth(line) > innerWidth
			line = ansi.Truncate(line, innerWidth, "…")
		}
		rows = append(rows, sty.Tool.SummaryText.Render(line))
	}
	footer := ""
	if expanded || clipped || limit < len(lines) {
		footer = summaryDisclosure("Full output", expanded)
	}
	return renderSummaryCard(sty, width, rows, footer)
}
