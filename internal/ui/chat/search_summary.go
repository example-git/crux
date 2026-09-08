package chat

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/fsext"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
)

var searchSummaryLine = regexp.MustCompile(`^  Line (\d+)(?:, Char (\d+))?: (.*)$`)
var directorySummaryDepth = regexp.MustCompile(`^The directory tree is shown up to a depth of (\d+)\.`)

type searchSummaryMatch struct {
	path, line, column, text string
}

func searchSummaryResult(sty *styles.Styles, params tools.SearchParams, result *message.ToolResult, workingDir string, width int, expanded bool) string {
	content := strings.TrimSpace(summaryClean(result.Content))
	if params.Mode == tools.SearchModeFiles {
		return searchFilesSummary(sty, content, workingDir, width, expanded)
	}
	lines := strings.Split(content, "\n")
	if len(lines) < 2 || !strings.HasPrefix(lines[0], "Found ") {
		return summaryTextResult(sty, content, width, expanded)
	}
	var matches []searchSummaryMatch
	var path, note string
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "(Results are truncated.") {
			note = line
		} else if match := searchSummaryLine.FindStringSubmatch(line); match != nil && path != "" {
			matches = append(matches, searchSummaryMatch{path: path, line: match[1], column: match[2], text: match[3]})
		} else if !strings.HasPrefix(line, " ") && strings.HasSuffix(line, ":") {
			path = strings.TrimSuffix(line, ":")
		} else {
			return summaryTextResult(sty, content, width, expanded)
		}
	}
	if len(matches) == 0 {
		return summaryTextResult(sty, content, width, expanded)
	}
	innerWidth := summaryContentWidth(sty, width)
	rows := []string{sty.Tool.SummaryMeta.Render(lines[0])}
	pattern := params.Pattern
	if params.LiteralText {
		pattern = regexp.QuoteMeta(pattern)
	}
	var matcher *regexp.Regexp
	if pattern != "" {
		matcher, _ = regexp.Compile(pattern)
	}
	limit := len(matches)
	if !expanded {
		limit = min(limit, 4)
	}
	lastPath := ""
	for _, match := range matches[:limit] {
		if match.path != lastPath {
			rows = append(rows, "", sty.Tool.SummaryTitle.Render(summaryPath(match.path, workingDir, innerWidth, expanded)))
			lastPath = match.path
		}
		location := match.line
		if expanded && match.column != "" {
			location += ":" + match.column
		}
		prefix := location + "  "
		textWidth := max(1, innerWidth-ansi.StringWidth(prefix))
		text := highlightSummaryMatch(sty, match.text, matcher)
		wrapped := strings.Split(summaryWrap(text, textWidth), "\n")
		if !expanded && len(wrapped) > 2 {
			wrapped = wrapped[:2]
			wrapped[1] = ansi.Truncate(wrapped[1], max(1, textWidth-1), "") + "…"
		}
		for i, row := range wrapped {
			gutter := strings.Repeat(" ", ansi.StringWidth(prefix))
			if i == 0 {
				gutter = prefix
			}
			rows = append(rows, sty.Tool.SummaryMeta.Render(gutter)+sty.Tool.SummaryText.Render(row))
		}
	}
	if note != "" {
		rows = append(rows, "", sty.Tool.SummaryMeta.Render(summaryWrap(note, innerWidth)))
	}
	detail := "Full matches"
	if limit < len(matches) {
		detail = fmt.Sprintf("%d more matches", len(matches)-limit)
	}
	return renderSummaryCard(sty, width, rows, summaryDisclosure(detail, expanded))
}

func highlightSummaryMatch(sty *styles.Styles, text string, matcher *regexp.Regexp) string {
	if matcher == nil {
		return text
	}
	return matcher.ReplaceAllStringFunc(text, func(match string) string {
		return sty.Tool.SummaryMatch.Render(match)
	})
}

func summaryPath(path, workingDir string, width int, expanded bool) string {
	if expanded {
		return summaryWrap(path, width)
	}
	if workingDir != "" {
		if relative, err := filepath.Rel(workingDir, path); err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			path = filepath.ToSlash(relative)
		}
	}
	path = fsext.PrettyPath(path)
	if cells := ansi.StringWidth(path); cells > width {
		return ansi.TruncateLeft(path, cells-max(1, width)+1, "…")
	}
	return path
}

func searchFilesSummary(sty *styles.Styles, content, workingDir string, width int, expanded bool) string {
	if content == "No files found" {
		return summaryTextResult(sty, content, width, expanded)
	}
	paths := strings.Split(content, "\n")
	note := ""
	if index := strings.Index(content, "\n\n(Results are truncated."); index >= 0 {
		paths = strings.Split(content[:index], "\n")
		note = content[index+2:]
	}
	innerWidth := summaryContentWidth(sty, width)
	rows := []string{sty.Tool.SummaryMeta.Render(fmt.Sprintf("%d files", len(paths))), ""}
	limit := len(paths)
	if !expanded {
		limit = min(limit, 8)
	}
	for _, path := range paths[:limit] {
		rows = append(rows, sty.Tool.SummaryText.Render(summaryPath(path, workingDir, innerWidth, expanded)))
	}
	if note != "" {
		rows = append(rows, "", sty.Tool.SummaryMeta.Render(summaryWrap(note, innerWidth)))
	}
	detail := "Full paths"
	if limit < len(paths) {
		detail = fmt.Sprintf("%d more files", len(paths)-limit)
	}
	return renderSummaryCard(sty, width, rows, summaryDisclosure(detail, expanded))
}

func directorySummaryResult(sty *styles.Styles, result *message.ToolResult, width int, expanded bool) string {
	content := strings.TrimSpace(summaryClean(result.Content))
	var entries, notes []string
	var root, depth string
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if match := directorySummaryDepth.FindStringSubmatch(line); match != nil {
			depth = match[1]
		} else if strings.HasPrefix(line, "- ") && root == "" {
			root = strings.TrimPrefix(line, "- ")
		} else if root != "" && strings.HasPrefix(strings.TrimLeft(line, " "), "- ") {
			entries = append(entries, line)
		} else {
			notes = append(notes, line)
		}
	}
	if root == "" {
		return summaryTextResult(sty, content, width, expanded)
	}
	innerWidth := summaryContentWidth(sty, width)
	title := fmt.Sprintf("%d entries", len(entries))
	if depth != "" {
		title += " · depth " + depth
	}
	rows := []string{sty.Tool.SummaryMeta.Render(title), ""}
	if expanded {
		rows = append(rows, sty.Tool.SummaryTitle.Render(summaryWrap(root, innerWidth)))
	}
	limit := len(entries)
	if !expanded {
		limit = min(limit, 8)
	}
	clipped := false
	for i, entry := range entries[:limit] {
		trimmed := strings.TrimLeft(entry, " ")
		indent := len(entry) - len(trimmed)
		branch := "└─ "
		for _, next := range entries[i+1:] {
			nextIndent := len(next) - len(strings.TrimLeft(next, " "))
			if nextIndent == indent {
				branch = "├─ "
			}
			if nextIndent <= indent {
				break
			}
		}
		prefix := strings.Repeat(" ", max(0, indent-2)) + branch
		name := strings.TrimPrefix(trimmed, "- ")
		textWidth := max(1, innerWidth-ansi.StringWidth(prefix))
		if !expanded {
			clipped = clipped || ansi.StringWidth(name) > textWidth
			name = ansi.Truncate(name, textWidth, "…")
		} else {
			name = summaryWrap(name, textWidth)
			name = strings.ReplaceAll(name, "\n", "\n"+strings.Repeat(" ", ansi.StringWidth(prefix)))
		}
		style := sty.Tool.SummaryText
		if strings.HasSuffix(trimmed, "/") {
			style = sty.Tool.SummaryTitle
		}
		rows = append(rows, sty.Tool.SummaryMeta.Render(prefix)+style.Render(name))
	}
	var metadata tools.LSResponseMetadata
	_ = json.Unmarshal([]byte(result.Metadata), &metadata)
	if metadata.Truncated {
		notes = append(notes, "Listing limited by the tool; use a more specific path.")
	}
	for _, note := range notes {
		rows = append(rows, sty.Tool.SummaryMeta.Render(summaryWrap(note, innerWidth)))
	}
	footer := ""
	if expanded || limit < len(entries) || clipped {
		detail := "Full listing"
		if limit < len(entries) {
			detail = fmt.Sprintf("%d more entries", len(entries)-limit)
		}
		footer = summaryDisclosure(detail, expanded)
	}
	return renderSummaryCard(sty, width, rows, footer)
}
