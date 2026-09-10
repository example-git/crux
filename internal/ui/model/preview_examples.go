package model

import (
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/message"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/example-git/crux/internal/ui/chat"
)

// PreviewExample describes a selectable fixture. All output goes through the
// ordinary message extraction and tool factories; these are data, not views.
type PreviewExample struct {
	expandableOutput bool
	ID               string `json:"id"`
	Label            string `json:"label"`
	Group            string `json:"group"`
	Note             string `json:"note,omitempty"`
	tool             string
	params           any
	result           *message.ToolResult
	status           chat.ToolStatus
	msg              *message.Message
}

func PreviewExamples() []PreviewExample {
	var examples []PreviewExample
	addMessage := func(id, label string, msg *message.Message) {
		msg.ID = "example-" + id
		msg.SessionID = "preview-session"
		examples = append(examples, PreviewExample{ID: id, Label: label, Group: "Messages", msg: msg})
	}
	finished := func(text string) *message.Message {
		return &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: text}, message.Finish{Reason: message.FinishReasonEndTurn}}}
	}
	addMessage("user", "User · text", &message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Show one example of every task, tool, and message type. All content here is local fixture data."}}})
	addMessage("attachments", "User · attachments", &message.Message{Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: "Review these fixture attachments."}, message.BinaryContent{Path: "preview-notes.txt", MIMEType: "text/plain", Data: []byte("Local fixture notes")}, message.BinaryContent{Path: "preview-image.png", MIMEType: "image/png"}, message.ImageURLContent{URL: "https://example.invalid/preview-image.png"}}})
	addMessage("assistant", "Assistant · text and Markdown", finished("## Fixture response\n\nThe preview uses the **real UI renderer**.\n\n- Inspect spacing and hierarchy\n- Compare compact and expanded output\n\n```go\nfunc Preview() string { return \"ready\" }\n```"))
	addMessage("reasoning", "Assistant · completed reasoning", &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.ReasoningContent{Thinking: "Synthetic reasoning fixture: compare the input and output layouts, then check spacing at a narrow width.", StartedAt: 1788690000, FinishedAt: 1788690008}, message.TextContent{Text: "The fixture layout is ready for review."}, message.Finish{Reason: message.FinishReasonEndTurn}}})
	addMessage("thinking", "Assistant · thinking", &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.ReasoningContent{Thinking: "Synthetic active reasoning fixture: inspecting the session layout."}}})
	addMessage("streaming", "Assistant · streaming text", &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "This is a fixture of an unfinished streaming response…"}}})
	addMessage("retrying", "Assistant · retrying", &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.RetryingContent{}}})
	for _, f := range []struct {
		id, label string
		reason    message.FinishReason
	}{
		{"canceled", "Canceled", message.FinishReasonCanceled}, {"error", "Error", message.FinishReasonError}, {"refused", "Refused", message.FinishReasonContentFilter}, {"max-tokens", "Token limit", message.FinishReasonMaxTokens}, {"unknown-finish", "Unknown finish", message.FinishReasonUnknown},
	} {
		addMessage("assistant-"+f.id, "Assistant · "+f.label, &message.Message{Role: message.Assistant, Parts: []message.ContentPart{message.TextContent{Text: "Fixture response: " + f.label}, message.Finish{Reason: f.reason, Message: "Example " + f.label + " state", Details: "Local preview data; no provider request was made."}}})
	}
	summary := finished("The session reviewed renderer spacing, inspected tool results, and prepared a complete local fixture catalog.")
	summary.IsSummaryMessage = true
	addMessage("summary", "Conversation summary", summary)
	addMessage("shell", "User · shell command", &message.Message{Role: message.User, Parts: []message.ContentPart{message.ShellCommand{Command: "git status --short", Output: " M preview.go\n?? preview_examples.go", ExitCode: 0}}})
	addMessage("shell-error", "User · shell error", &message.Message{Role: message.User, Parts: []message.ContentPart{message.ShellCommand{Command: "go test ./fixture", Output: "FAIL: synthetic fixture failure", ExitCode: 1}}})
	addMessage("system", "System · context only", &message.Message{Role: message.System, Parts: []message.ContentPart{message.TextContent{Text: "Local preview system context."}, message.ProviderMetadataContent{}}})
	examples[len(examples)-1].Note = "System context has no standalone transcript row in the production UI."
	addTool := func(name string, params any, content string) {
		examples = append(examples, PreviewExample{ID: "tool-" + name, Label: name, Group: "Tools", tool: name, params: params, result: &message.ToolResult{Content: content}, status: chat.ToolStatusSuccess})
	}
	meta := func(v any) { examples[len(examples)-1].result.Metadata = previewJSON(v) }
	path := "/preview/crush/example.go"
	file := map[string]any{"file_path": path, "line": 3, "character": 5, "symbol": "Preview"}
	before := "package fixture\n\nfunc Preview() string { return \"before\" }\n"
	after := "package fixture\n\nfunc Preview() string { return \"after\" }\n"
	addTool(tools.BashToolName, map[string]any{"command": "go test ./fixture"}, "ok fixture 0.024s")
	addTool(tools.JQToolName, map[string]any{"filter": ".items[].name", "input": "{\"items\":[{\"name\":\"preview\"}]}"}, "\"preview\"")
	addTool(tools.JobOutputToolName, map[string]any{"job_id": "s00000001"}, "Fixture build completed.")
	addTool(tools.JobKillToolName, map[string]any{"job_id": "s00000001"}, "Stopped fixture job.")
	addTool(tools.ViewToolName, file, after)
	addTool(tools.WriteToolName, map[string]any{"file_path": path, "content": after}, "Wrote fixture file.")
	addTool(tools.EditToolName, map[string]any{"file_path": path, "old_string": "before", "new_string": "after"}, "Applied fixture edit.")
	meta(tools.EditResponseMetadata{OldContent: before, NewContent: after})
	addTool(tools.MultiEditToolName, map[string]any{"file_path": path, "edits": []any{map[string]string{"old_string": "before", "new_string": "after"}}}, "Applied fixture edits.")
	meta(tools.MultiEditResponseMetadata{OldContent: before, NewContent: after, EditsApplied: 1})
	addTool(tools.SearchToolName, map[string]any{"mode": "content", "pattern": "Preview", "path": "/preview/crush"}, "/preview/crush/example.go:3:func Preview() string { return \"ready\" }")
	addTool(tools.LSToolName, map[string]any{"path": "/preview/crush"}, "example.go\nREADME.md\ninternal/")
	addTool(tools.SkillLoadToolName, map[string]any{"name": "crux-config"}, "# Fixture skill\nInspect configuration and report changes.")
	meta(map[string]any{"description": "Configure Crux providers, models, permissions, and tools.", "builtin": true})
	addTool(tools.DownloadToolName, map[string]any{"url": "https://example.invalid/fixture.txt", "file_path": "/preview/crush/fixture.txt"}, "Downloaded fixture.txt (128 bytes).")
	addTool(tools.ImagegenToolName, map[string]any{"mode": "generate", "prompt": "A small blue terminal icon", "output": "/preview/crush/icon.png"}, "Image generation queued.")
	meta(map[string]any{"task_id": "i00000001", "mode": "generate", "outputs": []string{"/preview/crush/icon.png"}})
	addTool(tools.FetchToolName, map[string]any{"url": "https://example.invalid/docs"}, "# Fixture documentation\nThis page describes the preview layout.")
	addTool(tools.SourcegraphToolName, map[string]any{"query": "repo:fixture Preview"}, "example.go:3: func Preview() string")
	addTool(tools.CodebaseSearchToolName, map[string]any{"query": "preview rendering entry point", "count": 1}, "1. example.go:1-3 (score 0.98, symbol Preview)\npackage fixture\n\nfunc Preview() string { return \"ready\" }")
	addTool(tools.DiagnosticsToolName, file, "example.go:3:5: warning: fixture diagnostic")
	record := map[string]any{"record_id": "preview-traffic-1", "timestamp": "2026-09-06T15:00:00Z", "protocol": "http", "direction": "outbound", "phase": "response", "method": "POST", "url": "https://example.invalid/v1/messages", "status_code": 200}
	addTool(tools.TrafficLogsToolName, map[string]any{"limit": 1}, "1 fixture traffic record")
	meta(map[string]any{"records": []any{record}})
	addTool(tools.TrafficLogDetailToolName, map[string]any{"record_id": "preview-traffic-1"}, "Fixture response body")
	meta(map[string]any{"record": record})
	addTool(tools.TrafficLogSearchToolName, map[string]any{"record_id": "preview-traffic-1", "query": "preview"}, "preview-traffic-1: fixture match")
	addTool(tools.TrafficCaptureToolName, map[string]any{"executable": "fixture-client", "arguments": []string{"--preview"}}, "Fixture capture started")
	addTool(agent.AgentToolName, map[string]any{"prompt": "Review the fixture layout", "subagent_type": "explore"}, "The fixture review is complete.")
	addTool(tools.AgenticFetchToolName, map[string]any{"prompt": "Summarize the fixture documentation", "url": "https://example.invalid/docs"}, "Fixture documentation summary.")
	addTool(tools.WebFetchToolName, map[string]any{"url": "https://example.invalid/docs", "prompt": "Read the overview"}, "# Overview\nA local fixture of web content.")
	addTool(tools.WebSearchToolName, map[string]any{"query": "terminal layout examples"}, "1. Fixture terminal guide\nhttps://example.invalid/guide\nExample search result.")
	todos := []map[string]string{{"content": "Inspect renderer", "status": "completed", "active_form": "Inspecting renderer"}, {"content": "Compare layouts", "status": "in_progress", "active_form": "Comparing layouts"}, {"content": "Review results", "status": "pending", "active_form": "Reviewing results"}}
	addTool(tools.TodosToolName, map[string]any{"todos": todos}, "Updated fixture To-Do list.")
	meta(map[string]any{"is_new": true, "todos": todos, "total": 3, "completed": 1})
	addTool(tools.QuestionToolName, map[string]any{"questions": []any{map[string]any{"header": "Layout", "question": "Which layout should the fixture use?", "options": []any{map[string]string{"label": "Compact", "description": "Condensed layout"}, map[string]string{"label": "Full", "description": "Sidebar visible"}}}}}, "Question: Which layout should the fixture use?\nAnswer: Full")
	addTool(tools.ReferencesToolName, file, "example.go:3:6\nmain.go:12:2")
	addTool(tools.DefinitionToolName, file, "example.go:3:6\nfunc Preview() string")
	addTool(tools.RenameToolName, map[string]any{"file_path": path, "line": 3, "character": 5, "symbol": "Preview", "new_name": "RenderPreview"}, "Renamed Preview to RenderPreview in 2 files.")
	addTool(tools.ReplaceSymbolToolName, map[string]any{"file_path": path, "line": 3, "character": 5, "symbol": "Preview", "replacement": after, "action": "replace"}, "Replaced fixture symbol.")
	meta(map[string]any{"old_content": before, "new_content": after})
	addTool(tools.CallHierarchyToolName, map[string]any{"symbol": "Preview", "direction": "incoming", "path": "/preview/crush"}, "Preview\n  called by main.go:12:2")
	addTool(tools.SymbolsToolName, map[string]any{"file_path": path, "query": "Preview"}, "Preview · function · example.go:3")
	addTool(tools.LSPRestartToolName, map[string]any{"name": "dummy-gopls"}, "Restarted dummy-gopls.")
	addTool(tools.MemoryListToolName, map[string]any{"scope": "project"}, `[{"name":"Preview conventions","description":"Use the real renderer","content":"Fixture memory content"}]`)
	addTool(tools.MemoryUpsertToolName, map[string]any{"scope": "project", "name": "Preview conventions", "content": "Use the real renderer"}, "Saved fixture memory.")
	addTool(tools.MemoryRemoveToolName, map[string]any{"scope": "project", "topic": "old-fixture"}, "Removed fixture memory.")
	addTool(tools.ProjectCreateToolName, map[string]any{"name": "TUI preview"}, "Created fixture project.")
	addTool(tools.ProjectStatusToolName, map[string]any{}, `{"name":"TUI preview","status":"active","tasks":[{"id":"layout","content":"Review layout","completed":false}]}`)
	addTool(tools.ProjectUpdateToolName, map[string]any{"id": "layout", "completed": true}, "Updated fixture project.")
	addTool(tools.ProjectNotesToolName, map[string]any{"note": "Reviewed preview spacing."}, "Added fixture note.")
	addTool(tools.ProjectCompleteToolName, map[string]any{}, "Completed fixture project.")
	taskRows := []map[string]any{}
	for i, s := range []managedtask.Status{managedtask.StatusPending, managedtask.StatusRunning, managedtask.StatusCompleted, managedtask.StatusFailed, managedtask.StatusKilled, managedtask.StatusLost} {
		taskRows = append(taskRows, map[string]any{"id": fmt.Sprintf("a0000000%d", i), "type": "agent", "status": s, "description": "Fixture task " + string(s), "summary": "Fixture task " + string(s)})
	}
	addTool(tools.TaskListToolName, map[string]any{}, previewJSON(taskRows))
	addTool(tools.TaskOutputToolName, map[string]any{"task_id": "a00000002"}, `{"task":{"id":"a00000002","type":"agent","state":{"status":"completed"}},"retrieval_status":"success","output":"Fixture review completed."}`)
	addTool(tools.TaskRestartToolName, map[string]any{"task_id": "b00000001"}, `{"id":"b00000001","type":"shell","state":{"status":"running"}}`)
	addTool(tools.TaskStopToolName, map[string]any{"task_id": "a00000001"}, `{"id":"a00000001","type":"agent","state":{"status":"killed"}}`)
	addTool(tools.TaskContinueToolName, map[string]any{"task_id": "a00000002", "prompt": "Review another layout"}, `{"id":"a00000002","type":"agent","state":{"status":"running"},"child_session_id":"fixture-child"}`)
	addTool("mcp_dummy_lookup", map[string]any{"query": "fixture"}, "Dummy MCP result.")
	addTool("mcp_"+config.DockerMCPName+"_mcp-find", map[string]any{"query": "fixture"}, "Fixture Docker MCP discovery result.")
	addTool("fixture_custom_tool", map[string]any{"example": "generic renderer"}, "Custom tool fixture output.")
	// Distinct modes share a factory but have visibly different render paths.
	addTool(tools.SearchToolName, map[string]any{"mode": "files", "pattern": "*.go", "path": "/preview/crush"}, "example.go\nmain.go")
	examples[len(examples)-1].ID += "-files"
	examples[len(examples)-1].Label += " · files"
	addTool(tools.ImagegenToolName, map[string]any{"mode": "edit", "prompt": "Recolor the fixture icon", "output": "/preview/crush/icon-edit.png"}, "Image edit queued.")
	examples[len(examples)-1].ID += "-edit"
	examples[len(examples)-1].Label += " · edit"
	for _, s := range []struct {
		id     string
		status chat.ToolStatus
	}{{"awaiting-permission", chat.ToolStatusAwaitingPermission}, {"running", chat.ToolStatusRunning}, {"success", chat.ToolStatusSuccess}, {"error", chat.ToolStatusError}, {"canceled", chat.ToolStatusCanceled}} {
		result := &message.ToolResult{Content: "Fixture command finished."}
		if s.status == chat.ToolStatusRunning || s.status == chat.ToolStatusAwaitingPermission || s.status == chat.ToolStatusCanceled {
			result = nil
		}
		if s.status == chat.ToolStatusError {
			result.IsError = true
			result.Content = "Synthetic command failure: exit status 1"
		}
		examples = append(examples, PreviewExample{ID: "status-" + s.id, Label: "Tool · " + s.id, Group: "Tool states", tool: tools.BashToolName, params: map[string]any{"command": "echo fixture-" + s.id}, result: result, status: s.status})
	}
	for _, typ := range []managedtask.Type{managedtask.TypeAgent, managedtask.TypeShell, managedtask.TypeImage} {
		id := "task-" + string(typ)
		content := fmt.Sprintf("<task-notification><task-id>%s</task-id><task-type>%s</task-type><status>completed</status><summary>Fixture %s complete</summary><result>Local fixture %s task result.</result></task-notification>", id, typ, typ, typ)
		examples = append(examples, PreviewExample{ID: id, Label: "Background " + string(typ) + " · completed", Group: "Tasks", msg: &message.Message{ID: id, Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: content}}}})
	}
	for _, s := range []managedtask.Status{managedtask.StatusFailed, managedtask.StatusKilled, managedtask.StatusLost} {
		id := "task-" + string(s)
		content := fmt.Sprintf("<task-notification><task-id>%s</task-id><task-type>shell</task-type><status>%s</status><summary>Fixture task %s</summary><result>Local fixture result.</result><error-message>Synthetic %s state</error-message><exit-code>1</exit-code></task-notification>", id, s, s, s)
		examples = append(examples, PreviewExample{ID: id, Label: "Background shell · " + string(s), Group: "Tasks", msg: &message.Message{ID: id, Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: content}}}})
	}
	for _, typ := range []string{"agent", "shell", "image"} {
		e := PreviewExample{ID: "task-running-" + typ, Label: "Background " + typ + " · running", Group: "Tasks", status: chat.ToolStatusRunning}
		switch typ {
		case "agent":
			e.tool = agent.AgentToolName
			e.params = map[string]any{"prompt": "Inspect fixture spacing", "run_in_background": true}
		case "shell":
			e.tool = tools.BashToolName
			e.params = map[string]any{"command": "go test ./fixture", "run_in_background": true}
		case "image":
			e.tool = tools.ImagegenToolName
			e.params = map[string]any{"mode": "generate", "prompt": "Fixture icon"}
		}
		examples = append(examples, e)
	}
	extendPreviewOutputs(examples)
	return examples
}

func (p *Preview) exampleItems(o PreviewOptions) []chat.MessageItem {
	if p.imported {
		return p.importedItems(o)
	}
	var items []chat.MessageItem
	for _, e := range PreviewExamples() {
		if d, ok := p.data.Examples[e.ID]; ok {
			e.tool = d.Tool
			e.params = d.Params
			e.result = d.Result
			e.status = d.Status
			e.msg = d.Message
		}
		if o.Example != "all" && o.Example != e.ID {
			continue
		}
		var next []chat.MessageItem
		if e.msg != nil {
			next = chat.ExtractMessageItems(p.ui.com.Styles, e.msg, nil, "/preview/crush")
		} else {
			tc := message.ToolCall{ID: "example-" + e.ID, Name: e.tool, Input: previewJSON(e.params), Finished: e.status != chat.ToolStatusRunning}
			msg := &message.Message{ID: "message-" + e.ID, Role: message.Assistant, Parts: []message.ContentPart{tc, message.Finish{Reason: message.FinishReasonToolUse}}}
			if e.status == chat.ToolStatusCanceled {
				msg.Parts[1] = message.Finish{Reason: message.FinishReasonCanceled}
			}
			results := map[string]message.ToolResult{}
			if e.result != nil {
				result := *e.result
				result.ToolCallID = tc.ID
				result.Name = tc.Name
				results = chat.BuildToolResultMap([]*message.Message{{Role: message.Tool, Parts: []message.ContentPart{result}}})
			}
			next = chat.ExtractMessageItems(p.ui.com.Styles, msg, results, "/preview/crush")
			for _, item := range next {
				if tool, ok := item.(chat.ToolMessageItem); ok {
					tool.SetStatus(e.status)
				}
			}
		}
		for _, item := range next {
			if c, ok := item.(chat.Compactable); ok {
				c.SetCompact(o.ToolsCompact)
			}
			if o.ToolsExpanded {
				if c, ok := item.(chat.Expandable); ok {
					c.ToggleExpanded()
				}
			}
		}
		items = append(items, next...)
	}
	for i, text := range o.Submitted {
		msg := &message.Message{ID: fmt.Sprintf("gallery-input-%d", i), Role: message.User, Parts: []message.ContentPart{message.TextContent{Text: text}}}
		items = append(items, chat.ExtractMessageItems(p.ui.com.Styles, msg, nil, "")...)
	}
	return items
}

func previewExampleNote(id string) string {
	if id == "all" {
		return fmt.Sprintf("%d fixture examples · system context has no visible row · running states are snapshots", len(PreviewExamples()))
	}
	for _, e := range PreviewExamples() {
		if e.ID == id {
			return strings.TrimSuffix(strings.TrimSpace(e.Group+" · "+e.Label+" · "+e.Note), " ·")
		}
	}
	return ""
}
