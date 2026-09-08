package chat

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/skills"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

type skillLoadRenderContext struct{}

func (r *skillLoadRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	if opts.IsPending() {
		return pendingTool(sty, "Load Skill", opts.Anim, opts.Compact)
	}
	var params tools.SkillLoadParams
	if json.Unmarshal([]byte(opts.ToolCall.Input), &params) != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, width)
	}
	header := toolHeader(sty, opts.Status, "Load Skill", width, opts, params.Name)
	if opts.Compact {
		return header
	}
	if early, ok := toolEarlyStateContent(sty, opts, width); ok {
		return joinToolParts(header, early)
	}
	if opts.HasEmptyResult() {
		return header
	}
	width = toolBodyWidth(sty, width)
	innerWidth := summaryContentWidth(sty, width)
	var metadata skills.SkillReadResult
	_ = json.Unmarshal([]byte(opts.Result.Metadata), &metadata)
	instructions := opts.Result.Content
	skill, err := skills.ParseContent([]byte(instructions))
	if err == nil {
		instructions = skill.Instructions
		if metadata.Description == "" {
			metadata.Description = skill.Description
		}
	}
	source := "Loaded"
	if metadata.Builtin {
		source += " · built-in skill"
	} else if metadata.Source != "" {
		source += " · " + string(metadata.Source) + " skill"
	}
	rows := []string{sty.Tool.SummaryTitle.Render(source)}
	if metadata.Description != "" {
		description := summaryWrap(summaryClean(metadata.Description), innerWidth)
		if !opts.ExpandedContent {
			lines := strings.Split(description, "\n")
			if len(lines) > 3 {
				lines = lines[:3]
				lines[2] = summaryPreview(lines[2]+" …", innerWidth)
			}
			description = strings.Join(lines, "\n")
		}
		rows = append(rows, "", sty.Tool.SummaryText.Render(description))
	}
	if opts.ExpandedContent {
		if skill != nil {
			var details []string
			if skill.License != "" {
				details = append(details, "License: "+skill.License)
			}
			if skill.Compatibility != "" {
				details = append(details, "Compatibility: "+skill.Compatibility)
			}
			if skill.UserInvocable {
				details = append(details, "User invocable")
			}
			if skill.DisableModelInvocation {
				details = append(details, "Model invocation disabled")
			}
			keys := make([]string, 0, len(skill.Metadata))
			for key := range skill.Metadata {
				keys = append(keys, key)
			}
			slices.Sort(keys)
			for _, key := range keys {
				details = append(details, key+": "+skill.Metadata[key])
			}
			for _, detail := range details {
				rows = append(rows, sty.Tool.SummaryMeta.Render(summaryWrap(summaryClean(detail), innerWidth)))
			}
		}
		renderer := common.MarkdownRenderer(sty, innerWidth)
		mu := common.LockMarkdownRenderer(renderer)
		mu.Lock()
		rendered, renderErr := renderer.Render(summaryClean(instructions))
		mu.Unlock()
		if renderErr != nil {
			rendered = summaryWrap(summaryClean(instructions), innerWidth)
		}
		rows = append(rows, "", strings.TrimSpace(rendered))
	} else if metadata.Description == "" {
		rows = append(rows, "", sty.Tool.SummaryText.Render(summaryPreview(summaryClean(instructions), innerWidth)))
	}
	footer := summaryDisclosure(fmt.Sprintf("%d instruction lines", countLines(strings.TrimSpace(instructions))), opts.ExpandedContent)
	return joinToolParts(header, renderSummaryCard(sty, width, rows, footer))
}
