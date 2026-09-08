package diffview

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/aymanbagabas/go-udiff"
	"github.com/charmbracelet/x/ansi"
)

var patchHunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func (dv *DiffView) Wrap(wrap bool) *DiffView {
	dv.wrap = wrap
	return dv
}

func (dv *DiffView) Patch(content string) *DiffView {
	dv.patch = content
	return dv
}

func (dv *DiffView) readableText(content string, width int) []string {
	if width <= 0 {
		width = max(1, ansi.StringWidth(content))
	}
	if dv.wrap {
		return strings.Split(ansi.Hardwrap(content, width, true), "\n")
	}
	return []string{ansi.Truncate(content, width, "…")}
}

func (dv *DiffView) readableHeading(content string) []string {
	rows := dv.readableText(content, dv.width)
	for index := range rows {
		rows[index] = dv.style.DividerLine.Code.Render(rows[index])
	}
	return rows
}

func (dv *DiffView) readableViewport(rows []string) string {
	dv.totalLines = len(rows)
	dv.preventInfiniteYScroll()
	start := min(len(rows), max(0, dv.yOffset))
	end := len(rows)
	if dv.height > 0 {
		end = min(end, start+dv.height)
	}
	return strings.Join(rows[start:end], "\n")
}

func (dv *DiffView) readableCell(line *udiff.Line, before, after string, beforeDigits, afterDigits, width int, continuation bool) []string {
	ls := dv.style.MissingLine
	content, symbol := "", "  "
	if line != nil {
		ls = dv.lineStyleForType(line.Kind)
		if !continuation {
			content = strings.ReplaceAll(strings.TrimSuffix(line.Content, "\n"), "\t", strings.Repeat(" ", dv.tabWidth))
			content = dv.hightlightCode(content, ls.Code.GetBackground())
			switch line.Kind {
			case udiff.Insert:
				symbol = "+ "
			case udiff.Delete:
				symbol = "- "
			}
		}
	}
	gutterWidth := 0
	if dv.lineNumbers {
		if beforeDigits > 0 {
			gutterWidth += beforeDigits + 2*lineNumPadding
		}
		if afterDigits > 0 {
			gutterWidth += afterDigits + 2*lineNumPadding
		}
	}
	if width <= 0 {
		width = gutterWidth + 2 + max(1, ansi.StringWidth(content))
	}
	if gutterWidth+4 > width {
		gutterWidth, beforeDigits, afterDigits = 0, 0, 0
	}
	if width < 3 {
		symbol = strings.TrimSpace(symbol)
	}
	textWidth := max(1, width-gutterWidth-ansi.StringWidth(symbol))
	rows := dv.readableText(content, textWidth)
	for index, text := range rows {
		prefix := ""
		if dv.lineNumbers {
			if beforeDigits > 0 {
				prefix += ls.LineNumber.Render(pad(before, beforeDigits))
			}
			if afterDigits > 0 {
				prefix += ls.LineNumber.Render(pad(after, afterDigits))
			}
		}
		rows[index] = prefix + ls.Symbol.Background(ls.Code.GetBackground()).Render(symbol) + ls.Code.Width(textWidth).Render(text)
		before, after = "", ""
		symbol = strings.Repeat(" ", ansi.StringWidth(symbol))
	}
	return rows
}

func (dv *DiffView) readableHunk(hunk *udiff.Hunk) []string {
	beforeDigits := len(strconv.Itoa(hunk.FromLine + len(hunk.Lines)))
	afterDigits := len(strconv.Itoa(hunk.ToLine + len(hunk.Lines)))
	before, after := hunk.FromLine, hunk.ToLine
	var rows []string
	if dv.layout == layoutSplit && (dv.width <= 0 || dv.width >= beforeDigits+afterDigits+12) {
		leftWidth, rightWidth := dv.width/2, dv.width-dv.width/2
		if dv.width <= 0 {
			for _, line := range hunk.Lines {
				leftWidth = max(leftWidth, ansi.StringWidth(line.Content)+beforeDigits+4)
			}
			rightWidth = leftWidth
		}
		for _, pair := range hunkToSplit(hunk).lines {
			leftNumber, rightNumber := "", ""
			if pair.before != nil {
				leftNumber = strconv.Itoa(before)
				before++
			}
			if pair.after != nil {
				rightNumber = strconv.Itoa(after)
				after++
			}
			left := dv.readableCell(pair.before, leftNumber, "", beforeDigits, 0, leftWidth, false)
			right := dv.readableCell(pair.after, "", rightNumber, 0, afterDigits, rightWidth, false)
			leftBlank := dv.readableCell(pair.before, "", "", beforeDigits, 0, leftWidth, true)[0]
			rightBlank := dv.readableCell(pair.after, "", "", 0, afterDigits, rightWidth, true)[0]
			for index := range max(len(left), len(right)) {
				leftRow, rightRow := leftBlank, rightBlank
				if index < len(left) {
					leftRow = left[index]
				}
				if index < len(right) {
					rightRow = right[index]
				}
				rows = append(rows, leftRow+rightRow)
			}
		}
		return rows
	}
	for _, line := range hunk.Lines {
		leftNumber, rightNumber := "", ""
		if line.Kind != udiff.Insert {
			leftNumber = strconv.Itoa(before)
			before++
		}
		if line.Kind != udiff.Delete {
			rightNumber = strconv.Itoa(after)
			after++
		}
		rows = append(rows, dv.readableCell(&line, leftNumber, rightNumber, beforeDigits, afterDigits, dv.width, false)...)
	}
	return rows
}

func (dv *DiffView) renderPatch() []string {
	var rows []string
	var hunk *udiff.Hunk
	beforeRemaining, afterRemaining := 0, 0
	flush := func() {
		if hunk == nil {
			return
		}
		rows = append(rows, dv.readableHunk(hunk)...)
		before, after := dv.hunkShownLines(hunk)
		hunk = &udiff.Hunk{FromLine: hunk.FromLine + before, ToLine: hunk.ToLine + after}
	}
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(dv.patch, "\r\n", "\n"), "\n"), "\n")
	for index, line := range lines {
		if match := patchHunkHeader.FindStringSubmatch(line); match != nil {
			flush()
			before, _ := strconv.Atoi(match[1])
			after, _ := strconv.Atoi(match[3])
			beforeRemaining, afterRemaining = 1, 1
			if match[2] != "" {
				beforeRemaining, _ = strconv.Atoi(match[2])
			}
			if match[4] != "" {
				afterRemaining, _ = strconv.Atoi(match[4])
			}
			hunk = &udiff.Hunk{FromLine: before, ToLine: after}
			rows = append(rows, dv.readableHeading(line)...)
			continue
		}
		fileHeader := strings.HasPrefix(line, "diff --git ") || strings.HasPrefix(line, "--- ") && (hunk == nil || beforeRemaining <= 0 && afterRemaining <= 0 && index+1 < len(lines) && strings.HasPrefix(lines[index+1], "+++ "))
		if fileHeader {
			flush()
			hunk = nil
		}
		if hunk != nil && len(line) > 0 {
			kind, code := udiff.Equal, true
			switch line[0] {
			case '-':
				kind = udiff.Delete
			case '+':
				kind = udiff.Insert
			case ' ':
			default:
				code = false
			}
			if code {
				if kind != udiff.Insert {
					beforeRemaining--
				}
				if kind != udiff.Delete {
					afterRemaining--
				}
				hunk.Lines = append(hunk.Lines, udiff.Line{Kind: kind, Content: line[1:]})
				continue
			}
		}
		flush()
		if hunk == nil && (strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ")) {
			path, _, _ := strings.Cut(line[4:], "\t")
			if path != "/dev/null" {
				dv.before.path = strings.TrimPrefix(strings.TrimPrefix(path, "a/"), "b/")
				dv.cachedLexer = nil
				dv.clearSyntaxCache()
			}
		}
		rows = append(rows, dv.readableHeading(line)...)
	}
	flush()
	return rows
}
