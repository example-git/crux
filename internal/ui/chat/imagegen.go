package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
)

type imagegenToolRenderContext struct{}

func newImagegenToolMessageItem(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &imagegenToolRenderContext{}, canceled)
}

func (r *imagegenToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	var params tools.ImagegenParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	name := "Generate Image"
	if params.Mode == imagegen.ModeEdit {
		name = "Edit Image"
	}
	if opts.IsPending() {
		return pendingTool(sty, name, opts.Anim, opts.Compact)
	}
	output := params.Output
	if output == "" {
		output = params.OutputDirectory
	}
	toolParams := []string{params.Prompt}
	if output != "" {
		toolParams = append(toolParams, "output", output)
	}
	if params.N != nil && *params.N > 1 {
		toolParams = append(toolParams, "variants", fmt.Sprintf("%d", *params.N))
	}
	header := toolHeader(sty, opts.Status, name, cappedWidth, opts, toolParams...)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}
	content := opts.Result.Content
	if formatted, ok := formatQueuedImagegenResult(opts.Result.Metadata); ok {
		content = formatted
	}
	body := sty.Tool.Body.Render(toolOutputPlainContent(sty, content, cappedWidth-toolBodyLeftPaddingTotal, opts.ExpandedContent))
	return joinToolParts(header, body)
}

func formatQueuedImagegenResult(metadata string) (string, bool) {
	var result tools.ImagegenResponseMetadata
	if json.Unmarshal([]byte(metadata), &result) != nil || result.TaskID == "" {
		return "", false
	}
	label := "Image generation queued"
	if result.Mode == imagegen.ModeEdit {
		label = "Image edit queued"
	}
	lines := []string{fmt.Sprintf("%s as %s", label, result.TaskID)}
	lines = appendImagePaths(lines, result.Outputs, "Planned output")
	return strings.Join(lines, "\n"), true
}

func FormatImagegenResult(value string) (string, bool) {
	var result imagegen.JobResult
	if json.Unmarshal([]byte(value), &result) != nil || result.Mode == "" {
		return "", false
	}
	var lines []string
	if result.Success {
		if len(result.Failures) > 0 {
			total := result.Requested
			if total < len(result.Outputs)+len(result.Failures) {
				total = len(result.Outputs) + len(result.Failures)
			}
			if result.Mode == imagegen.ModeEdit {
				lines = append(lines, fmt.Sprintf("Created %d of %d edited image variants", len(result.Outputs), total))
			} else {
				lines = append(lines, fmt.Sprintf("Generated %d of %d image variants", len(result.Outputs), total))
			}
		} else {
			switch {
			case result.Mode == imagegen.ModeEdit && len(result.Outputs) == 1:
				lines = append(lines, "Image edit completed")
			case result.Mode == imagegen.ModeEdit:
				lines = append(lines, fmt.Sprintf("Created %d edited image variants", len(result.Outputs)))
			case len(result.Outputs) == 1:
				lines = append(lines, "Image generated")
			default:
				lines = append(lines, fmt.Sprintf("Generated %d image variants", len(result.Outputs)))
			}
		}
		lines = appendImagePaths(lines, result.Outputs, "Saved to")
		if len(result.Failures) > 0 {
			lines = append(lines, "Failed variants:")
			for _, failure := range result.Failures {
				lines = append(lines, fmt.Sprintf("• Variant %d: %s", failure.Variant, strings.TrimSpace(failure.Error)))
			}
		}
		settings := make([]string, 0, 2)
		if result.Model != "" {
			settings = append(settings, "Model: "+result.Model)
		}
		switch result.AuthMode {
		case "codex":
			settings = append(settings, "Account: Codex")
		case "openai_api_key":
			settings = append(settings, "Account: OpenAI API")
		case "flow":
			settings = append(settings, "Account: Google Flow")
		}
		if len(settings) > 0 {
			lines = append(lines, strings.Join(settings, " · "))
		}
		return strings.Join(lines, "\n"), true
	}
	if result.Mode == imagegen.ModeEdit {
		lines = append(lines, "Image edit failed")
	} else {
		lines = append(lines, "Image generation failed")
	}
	if strings.TrimSpace(result.Error) != "" {
		lines = append(lines, strings.TrimSpace(result.Error))
	}
	lines = appendImagePaths(lines, result.Outputs, "Planned output")
	return strings.Join(lines, "\n"), true
}

func appendImagePaths(lines, paths []string, label string) []string {
	if len(paths) == 0 {
		return lines
	}
	if len(paths) == 1 {
		return append(lines, label+": "+paths[0])
	}
	lines = append(lines, label+"s:")
	for _, path := range paths {
		lines = append(lines, "• "+path)
	}
	return lines
}
