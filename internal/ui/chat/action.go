package chat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/automemory"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/projects"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/styles"
)

type actionToolRenderContext struct{}

func newActionToolMessageItem(sty *styles.Styles, toolCall message.ToolCall, result *message.ToolResult, canceled bool) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &actionToolRenderContext{}, canceled)
}

func (r *actionToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := width
	name, params := actionToolHeader(opts.ToolCall)
	if opts.IsPending() {
		return pendingTool(sty, name, opts.Anim, opts.Compact)
	}

	header := toolHeader(sty, opts.Status, name, cappedWidth, opts, params...)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if !opts.HasResult() || opts.Result.Content == "" {
		return header
	}

	body := actionToolResult(sty, opts, toolBodyWidth(sty, cappedWidth))
	if body == "" {
		return header
	}
	return joinToolParts(header, body)
}

func actionToolHeader(toolCall message.ToolCall) (string, []string) {
	var params map[string]json.RawMessage
	_ = json.Unmarshal([]byte(toolCall.Input), &params)
	value := func(key string) string {
		var text string
		_ = json.Unmarshal(params[key], &text)
		return strings.TrimSpace(text)
	}

	switch toolCall.Name {
	case tools.MemoryListToolName:
		if topic := value("topic"); topic != "" {
			return "Read Memory", []string{topic, "scope", value("scope")}
		}
		return "List Memories", []string{value("scope")}
	case tools.MemoryUpsertToolName:
		return "Save Memory", []string{value("name"), "scope", value("scope")}
	case tools.MemoryRemoveToolName:
		return "Remove Memory", []string{value("topic"), "scope", value("scope")}
	case tools.ProjectCreateToolName:
		return "Create Project", []string{value("name")}
	case tools.ProjectStatusToolName:
		return "Project Status", nil
	case tools.ProjectUpdateToolName:
		state := "reopen"
		var completed bool
		if json.Unmarshal(params["completed"], &completed) == nil && completed {
			state = "complete"
		}
		return "Update Project", []string{value("id"), "state", state}
	case tools.ProjectNotesToolName:
		switch value("action") {
		case "list":
			return "List Project Notes", nil
		case "read":
			return "Read Project Note", []string{value("topic")}
		}
		return "Add Project Note", nil
	case tools.ProjectCompleteToolName:
		return "Complete Project", nil
	case tools.TaskListToolName:
		return "List Tasks", nil
	case tools.TaskOutputToolName:
		return "Task Output", []string{value("task_id")}
	case tools.TaskRestartToolName:
		return "Restart Command", []string{value("task_id")}
	case tools.TaskStopToolName:
		return "Stop Task", []string{value("task_id")}
	case tools.TaskContinueToolName:
		return "Continue Task", []string{value("task_id")}
	default:
		return humanizedToolName(toolCall.Name), nil
	}
}

func actionToolResult(sty *styles.Styles, opts *ToolRenderOpts, width int) string {
	switch opts.ToolCall.Name {
	case tools.MemoryListToolName:
		return memoryActionResult(sty, opts.Result.Content, width, opts.ExpandedContent)
	case tools.ProjectStatusToolName:
		return projectStatusActionResult(sty, opts.Result.Content, width, opts.ExpandedContent)
	case tools.TaskListToolName:
		return taskListActionResult(sty, opts.Result.Content, width, opts.ExpandedContent)
	case tools.TaskOutputToolName:
		return taskOutputActionResult(sty, opts.Result.Content, width, opts.ExpandedContent)
	case tools.TaskContinueToolName, tools.TaskStopToolName, tools.TaskRestartToolName:
		return taskMutationActionResult(sty, opts.Result.Content, width)
	default:
		return ""
	}
}

func memoryActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	var entries []automemory.Entry
	if json.Unmarshal([]byte(content), &entries) == nil {
		if len(entries) == 0 {
			return sty.Tool.Body.Render(sty.Tool.StateWaiting.Render("No memories found."))
		}
		innerWidth := summaryContentWidth(sty, width)
		rows := []string{sty.Tool.SummaryMeta.Render(fmt.Sprintf("%d memories", len(entries)))}
		limit := len(entries)
		if !expanded {
			limit = min(limit, 5)
		}
		for _, entry := range entries[:limit] {
			name := entry.Name
			if name == "" {
				name = entry.File
			}
			text := summaryClean(name)
			if entry.Description != "" {
				text += " · " + summaryClean(entry.Description)
			}
			text = summaryWrap(text, max(1, innerWidth-2))
			rows = append(rows, sty.Tool.SummaryText.Render("• "+strings.ReplaceAll(text, "\n", "\n  ")))
		}
		footer := ""
		if expanded || limit < len(entries) {
			footer = summaryDisclosure("All memories", expanded)
		}
		return renderSummaryCard(sty, width, rows, footer)
	}
	var entry automemory.Entry
	if json.Unmarshal([]byte(content), &entry) == nil && (entry.Name != "" || entry.File != "") {
		text := entry.Name
		if text == "" {
			text = entry.File
		}
		if entry.Description != "" {
			text += "\n" + entry.Description
		}
		if entry.Content != "" {
			text += "\n\n" + entry.Content
		}
		return sty.Tool.Body.Render(toolOutputMarkdownPanel(sty, text, width, expanded))
	}
	return sty.Tool.Body.Render(toolOutputPlainContent(sty, content, width, expanded))
}

type projectStatusView struct {
	Name        string          `json:"name"`
	Status      projects.Status `json:"status"`
	Tasks       []projects.Task `json:"tasks"`
	CurrentGoal *projects.Task  `json:"current_goal"`
}

func projectStatusActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	if content == "No project is active for this workspace." {
		return sty.Tool.StateWaiting.Render(content)
	}
	var project projectStatusView
	if json.Unmarshal([]byte(content), &project) != nil || project.Name == "" {
		return sty.Tool.Body.Render(toolOutputPlainContent(sty, content, width, expanded))
	}
	completed := 0
	for _, task := range project.Tasks {
		if task.Completed {
			completed++
		}
	}
	lines := []string{fmt.Sprintf("%s · %s · %d/%d complete", project.Name, project.Status, completed, len(project.Tasks))}
	if project.CurrentGoal != nil {
		lines = append(lines, "Current: "+project.CurrentGoal.ID+" "+project.CurrentGoal.Content)
	}
	return sty.Tool.Body.Render(toolOutputPlainContent(sty, strings.Join(lines, "\n"), width, expanded))
}

func taskListActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	if content == "No background tasks are currently tracked." {
		return summaryTextResult(sty, content, width, expanded)
	}
	var tasks []struct {
		managedtask.View
		Status managedtask.Status `json:"status"`
	}
	if json.Unmarshal([]byte(content), &tasks) != nil {
		return summaryTextResult(sty, content, width, expanded)
	}
	innerWidth := summaryContentWidth(sty, width)
	rows := []string{sty.Tool.SummaryMeta.Render(fmt.Sprintf("%d tasks", len(tasks)))}
	limit := len(tasks)
	if !expanded {
		limit = min(limit, 6)
	}
	clipped := false
	for _, task := range tasks[:limit] {
		status := task.Status
		if status == "" {
			status = task.State.Status
		}
		statusStyle := sty.Tool.SummaryTitle
		switch status {
		case managedtask.StatusFailed, managedtask.StatusLost:
			statusStyle = statusStyle.Foreground(sty.Tool.IconError.GetForeground())
		case managedtask.StatusCompleted:
			statusStyle = statusStyle.Foreground(sty.Tool.IconSuccess.GetForeground())
		}
		identity := sty.Tool.SummaryTitle.Render(summaryClean(task.ID))
		if status != "" {
			identity += "  " + statusStyle.Render(string(status))
		}
		if task.Type != "" {
			identity += "  " + sty.Tool.SummaryMeta.Render(string(task.Type))
		}
		rows = append(rows, "", summaryWrap(identity, innerWidth))
		description := summaryClean(task.Description)
		if description != "" {
			description = summaryWrap(description, innerWidth)
			lines := strings.Split(description, "\n")
			if !expanded && len(lines) > 3 {
				clipped = true
				description = strings.Join(lines[:2], "\n") + "\n" + summaryPreview(lines[2]+" …", innerWidth)
			}
			rows = append(rows, sty.Tool.SummaryText.Render(description))
		}
	}
	footer := ""
	if expanded || limit < len(tasks) || clipped {
		detail := "Full descriptions"
		if limit < len(tasks) {
			detail = fmt.Sprintf("%d more tasks", len(tasks)-limit)
		}
		footer = summaryDisclosure(detail, expanded)
	}
	return renderSummaryCard(sty, width, rows, footer)
}

func taskOutputActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	var result managedtask.OutputResult
	if json.Unmarshal([]byte(content), &result) != nil || result.Task.ID == "" {
		return sty.Tool.Body.Render(toolOutputPlainContent(sty, content, width, expanded))
	}
	status := fmt.Sprintf("%s · %s", result.Task.State.Status, result.RetrievalStatus)
	if result.OutputTruncated {
		status += " · truncated"
	}
	output := result.Output
	if result.Task.Type == managedtask.TypeImage {
		if formatted, ok := FormatImagegenResult(output); ok {
			output = formatted
		}
	}
	diagnostics := taskResultDiagnostics(result.Task.State.ErrorMessage, result.Task.State.LostReason, result.Task.State.ExitCode)
	if strings.TrimSpace(output) == "" && diagnostics == "" {
		return sty.Tool.Body.Render(sty.Tool.StateWaiting.Render(status + " · no output"))
	}
	return sty.Tool.Body.Render(toolOutputPlainContent(sty, strings.Join([]string{status, diagnostics, output}, "\n"), width, expanded))
}

func taskMutationActionResult(sty *styles.Styles, content string, width int) string {
	var task managedtask.View
	if json.Unmarshal([]byte(content), &task) != nil || task.ID == "" {
		return ""
	}
	text := fmt.Sprintf("%s · %s", task.ID, task.State.Status)
	if task.ChildSessionID != "" {
		text += " · child session ready"
	}
	return sty.Tool.Body.Render(toolOutputPlainContent(sty, text, width, false))
}
