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
	cappedWidth := cappedMessageWidth(width)
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

	body := actionToolResult(sty, opts, cappedWidth-toolBodyLeftPaddingTotal)
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
		return "Add Project Note", nil
	case tools.ProjectCompleteToolName:
		return "Complete Project", nil
	case tools.TaskListToolName:
		return "List Tasks", nil
	case tools.TaskOutputToolName:
		return "Task Output", []string{value("task_id")}
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
	case tools.TaskContinueToolName, tools.TaskStopToolName:
		return taskMutationActionResult(sty, opts.Result.Content, width)
	default:
		return ""
	}
}

func memoryActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	var entries []automemory.Entry
	if json.Unmarshal([]byte(content), &entries) == nil {
		if len(entries) == 0 {
			return sty.Tool.StateWaiting.Render("No memories found.")
		}
		lines := []string{fmt.Sprintf("%d memories", len(entries))}
		limit := len(entries)
		if !expanded {
			limit = min(limit, 5)
		}
		for _, entry := range entries[:limit] {
			line := entry.Name
			if line == "" {
				line = entry.File
			}
			if entry.Description != "" {
				line += " · " + entry.Description
			}
			lines = append(lines, "• "+line)
		}
		if limit < len(entries) {
			lines = append(lines, fmt.Sprintf("… %d more", len(entries)-limit))
		}
		return toolOutputPlainContent(sty, strings.Join(lines, "\n"), width, true)
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
		return toolOutputPlainContent(sty, text, width, expanded)
	}
	return toolOutputPlainContent(sty, content, width, expanded)
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
		return toolOutputPlainContent(sty, content, width, expanded)
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
	return toolOutputPlainContent(sty, strings.Join(lines, "\n"), width, true)
}

func taskListActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	if content == "No background tasks are currently tracked." {
		return sty.Tool.StateWaiting.Render(content)
	}
	var tasks []managedtask.View
	if json.Unmarshal([]byte(content), &tasks) != nil {
		return toolOutputPlainContent(sty, content, width, expanded)
	}
	lines := []string{fmt.Sprintf("%d tasks", len(tasks))}
	limit := len(tasks)
	if !expanded {
		limit = min(limit, 6)
	}
	for _, task := range tasks[:limit] {
		description := strings.ReplaceAll(strings.TrimSpace(task.Description), "\n", " ")
		lines = append(lines, fmt.Sprintf("• %s · %s · %s", task.ID, task.State.Status, description))
	}
	if limit < len(tasks) {
		lines = append(lines, fmt.Sprintf("… %d more", len(tasks)-limit))
	}
	return toolOutputPlainContent(sty, strings.Join(lines, "\n"), width, true)
}

func taskOutputActionResult(sty *styles.Styles, content string, width int, expanded bool) string {
	var result managedtask.OutputResult
	if json.Unmarshal([]byte(content), &result) != nil || result.Task.ID == "" {
		return toolOutputPlainContent(sty, content, width, expanded)
	}
	status := fmt.Sprintf("%s · %s", result.Task.State.Status, result.RetrievalStatus)
	if result.OutputTruncated {
		status += " · truncated"
	}
	if strings.TrimSpace(result.Output) == "" {
		return sty.Tool.StateWaiting.Render(status + " · no output")
	}
	return toolOutputPlainContent(sty, status+"\n"+result.Output, width, expanded)
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
	return toolOutputPlainContent(sty, text, width, true)
}
