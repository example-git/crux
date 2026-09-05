package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
)

type TrafficCaptureToolRenderContext struct{}

func NewTrafficCaptureToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &TrafficCaptureToolRenderContext{}, canceled)
}

func (r *TrafficCaptureToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Traffic Capture", opts.Anim, opts.Compact)
	}
	var params tools.TrafficCaptureParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	target := params.Executable
	if target == "" && params.PID > 0 {
		target = fmt.Sprintf("PID %d", params.PID)
	}
	header := toolHeader(sty, opts.Status, "Traffic Capture", cappedWidth, opts, target)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}
	var metadata tools.TrafficCaptureResponseMetadata
	if opts.Result.Metadata == "" || json.Unmarshal([]byte(opts.Result.Metadata), &metadata) != nil || metadata.Session == "" {
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}
	parts := []string{
		sty.Tool.ParamKey.Render("Session: ") + metadata.Session,
		sty.Tool.ParamKey.Render("Capture: ") + metadata.CapturePath,
		sty.Tool.ParamKey.Render("Status: ") + metadata.StatusPath,
		sty.Tool.ParamKey.Render("Pane log: ") + metadata.PaneLogPath,
	}
	if metadata.ViewerURL != "" {
		parts = append(parts, sty.Tool.ParamKey.Render("Viewer: ")+metadata.ViewerURL)
	}
	return joinToolParts(header, sty.Tool.Body.Render(strings.Join(parts, "\n")))
}
