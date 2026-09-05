package chat

import (
	"encoding/json"
	"fmt"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
)

type JQToolRenderContext struct{}

func NewJQToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &JQToolRenderContext{}, canceled)
}

func (r *JQToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "jq", opts.Anim, opts.Compact)
	}

	var params tools.JQParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	filter := params.Filter
	if filter == "" {
		filter = "."
	}
	toolParams := []string{filter}
	if len(params.Files) == 1 {
		toolParams = append(toolParams, "file", params.Files[0])
	} else if len(params.Files) > 1 {
		toolParams = append(toolParams, "files", fmt.Sprintf("%d files", len(params.Files)))
	} else if params.NullInput {
		toolParams = append(toolParams, "input", "null")
	} else if params.RawInput {
		toolParams = append(toolParams, "input", "raw text")
	}

	header := toolHeader(sty, opts.Status, "jq", cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}
	bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
	return joinToolParts(header, body)
}
