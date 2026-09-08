package chat

import (
	"encoding/json"
	"strings"

	"github.com/example-git/crux/internal/diffdetect"
	"github.com/example-git/crux/internal/stringext"
	"github.com/example-git/crux/internal/ui/styles"
)

func humanizedToolName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "-", " ")
	return stringext.Capitalize(name)
}

func looksLikeMarkdown(content string) bool {
	patterns := []string{
		"# ",
		"## ",
		"**",
		"```",
		"- ",
		"1. ",
		"> ",
		"---",
		"***",
	}
	for _, p := range patterns {
		if strings.Contains(content, p) {
			return true
		}
	}
	return false
}

func renderToolResultTextContent(sty *styles.Styles, content string, width int, expanded bool) string {
	bodyWidth := toolBodyWidth(sty, width)
	var result json.RawMessage
	if err := json.Unmarshal([]byte(content), &result); err == nil {
		prettyResult, err := json.MarshalIndent(result, "", "  ")
		if err == nil {
			return toolOutputCodeContent(sty, "result.json", string(prettyResult), 0, width, expanded)
		}
		return sty.Tool.Body.Render(toolOutputPlainContent(sty, content, bodyWidth, expanded))
	}
	if diffdetect.IsUnifiedDiff(content) {
		return toolOutputDiffContentFromUnified(sty, content, width, expanded)
	}
	if looksLikeMarkdown(content) {
		return sty.Tool.Body.Render(toolOutputMarkdownPanel(sty, content, bodyWidth, expanded))
	}
	return sty.Tool.Body.Render(toolOutputPlainContent(sty, content, bodyWidth, expanded))
}
