package chat

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
)

var codebaseSearchMatchHeader = regexp.MustCompile(`(?m)^\d+\. (.+):(\d+)-(\d+) \(score ([0-9.+-]+)(?:, symbol ([^)]+))?\)\n`)

type codebaseSearchToolRenderContext struct{}

type codebaseSearchMatch struct {
	role        string
	path        string
	startLine   int
	endLine     int
	score       string
	symbol      string
	explanation string
	content     string
}

func newCodebaseSearchToolMessageItem(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &codebaseSearchToolRenderContext{}, canceled)
}

func (r *codebaseSearchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Codebase Search", opts.Anim, opts.Compact)
	}

	var params tools.CodebaseSearchParams
	if json.Unmarshal([]byte(opts.ToolCall.Input), &params) != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	toolParams := []string{params.Query}
	if params.PathPrefix != "" {
		toolParams = append(toolParams, "path", params.PathPrefix)
	}
	if params.Count != 0 {
		toolParams = append(toolParams, "count", formatNonZero(params.Count))
	}

	header := toolHeader(sty, opts.Status, "Codebase Search", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}
	if opts.Result.Content == "No semantic matches found." {
		return joinToolParts(header, sty.Tool.StateWaiting.Render(opts.Result.Content))
	}

	matches := parseCodebaseSearchMatches(opts.Result.Content)
	if len(matches) == 0 {
		body := toolOutputPlainContent(sty, opts.Result.Content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent)
		return joinToolParts(header, body)
	}

	limit := len(matches)
	if !opts.ExpandedContent {
		limit = min(limit, 3)
	}
	bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
	parts := []string{sty.Tool.ParamKey.Render(fmt.Sprintf("%d semantic matches", len(matches)))}
	lastRole := ""
	for _, match := range matches[:limit] {
		if match.role != "" && match.role != lastRole {
			parts = append(parts, sty.Tool.NameNested.Render(match.role))
			lastRole = match.role
		}
		location := sty.Tool.ParamMain.Render(fmt.Sprintf("%s:%d-%d", match.path, match.startLine, match.endLine))
		score := sty.Tool.ParamKey.Render("  score " + match.score)
		if match.symbol != "" {
			score += sty.Tool.ParamKey.Render("  " + match.symbol)
		}
		parts = append(parts, location+score)
		if match.explanation != "" {
			parts = append(parts, sty.Tool.ParamKey.Render(match.explanation))
		}
		code := toolOutputCodeContent(sty, match.path, match.content, match.startLine, bodyWidth, opts.ExpandedContent)
		parts = append(parts, code)
	}
	if limit < len(matches) {
		parts = append(parts, sty.Tool.ContentTruncation.Render(fmt.Sprintf("… %d more matches", len(matches)-limit)))
	}
	return joinToolParts(header, sty.Tool.Body.Render(strings.Join(parts, "\n")))
}

func parseCodebaseSearchMatches(content string) []codebaseSearchMatch {
	indexes := codebaseSearchMatchHeader.FindAllStringSubmatchIndex(content, -1)
	matches := make([]codebaseSearchMatch, 0, len(indexes))
	for index, positions := range indexes {
		contentEnd := len(content)
		if index+1 < len(indexes) {
			contentEnd = indexes[index+1][0]
		}
		startLine, _ := strconv.Atoi(content[positions[4]:positions[5]])
		endLine, _ := strconv.Atoi(content[positions[6]:positions[7]])
		role := codebaseSearchRoleBefore(content[:positions[0]])
		symbol := ""
		if positions[10] >= 0 {
			symbol = content[positions[10]:positions[11]]
		}
		body := strings.TrimSpace(content[positions[1]:contentEnd])
		explanation := ""
		if line, rest, found := strings.Cut(body, "\n"); found && codebaseSearchExplanation(line) {
			explanation = line
			body = strings.TrimSpace(rest)
		}
		matches = append(matches, codebaseSearchMatch{
			role:        role,
			path:        content[positions[2]:positions[3]],
			startLine:   startLine,
			endLine:     endLine,
			score:       content[positions[8]:positions[9]],
			symbol:      symbol,
			explanation: explanation,
			content:     body,
		})
	}
	return matches
}

func codebaseSearchRoleBefore(content string) string {
	lines := strings.Split(strings.TrimSpace(content), "\n")
	if len(lines) == 0 {
		return ""
	}
	last := strings.TrimSpace(lines[len(lines)-1])
	switch last {
	case "Direct implementation", "Delivery/call path", "Persistence", "Recovery/retry", "Payload/construction", "Startup/call path", "Validation", "Contract", "Comparison", "Parallel implementation", "Related behavior":
		return last
	default:
		return ""
	}
}

func codebaseSearchExplanation(line string) bool {
	return strings.HasPrefix(line, "Matches ") ||
		strings.Contains(line, " evidence. Matches ") ||
		strings.HasPrefix(line, "Enclosing symbol: ") ||
		strings.Contains(line, " evidence. Enclosing symbol: ") ||
		line == "Semantically related supporting result." ||
		strings.HasSuffix(line, " Semantically related supporting result.")
}
