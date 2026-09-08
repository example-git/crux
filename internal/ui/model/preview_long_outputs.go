package model

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/chat"
)

const previewOutputLines = 24

// Long fixtures exercise the real disclosure limits; no renderer is changed.
func previewReport(topic string) string {
	phases := []string{"Load fixture configuration", "Inspect input boundaries", "Read session metadata", "Resolve dummy model", "Measure header spacing", "Measure sidebar width", "Check text wrapping", "Compare compact layout", "Inspect code gutter", "Check diff alignment", "Review added lines", "Review removed lines", "Inspect task progress", "Check result metadata", "Review warning styles", "Check error details", "Inspect disclosure label", "Expand hidden content", "Review scroll position", "Check narrow viewport", "Restore full layout", "Verify local-only state", "Record preview observations", "Complete fixture review"}
	rows := make([]string, len(phases))
	for i, phase := range phases {
		rows[i] = fmt.Sprintf("%d. %s: %s.", i+1, topic, phase)
	}
	return strings.Join(rows, "\n")
}

func previewSource(variant string) string {
	rows := []string{"package fixture", "", "// Synthetic source for long output and diff previews."}
	for i := 1; i <= previewOutputLines; i++ {
		rows = append(rows, fmt.Sprintf("func PreviewStep%02d() string { return %q }", i, fmt.Sprintf("%s step %02d", variant, i)))
	}
	return strings.Join(rows, "\n") + "\n"
}

func previewObject(value string) map[string]any {
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		panic(fmt.Sprintf("invalid structured preview fixture: %v", err))
	}
	return object
}

func extendPreviewOutputs(examples []PreviewExample) {
	for index := range examples {
		e := &examples[index]
		report := previewReport(e.Label)
		if e.msg != nil {
			for i, part := range e.msg.Parts {
				switch value := part.(type) {
				case message.ShellCommand:
					value.Output += "\n" + report
					e.msg.Parts[i] = value
					e.expandableOutput = true
				case message.ReasoningContent:
					value.Thinking = report
					e.msg.Parts[i] = value
					e.expandableOutput = true
				case message.TextContent:
					if e.msg.IsSummaryMessage {
						value.Text = "## Session review\n\n" + report
						e.msg.Parts[i] = value
						e.expandableOutput = true
					}
					if strings.HasPrefix(value.Text, "<task-notification>") {
						var escaped bytes.Buffer
						_ = xml.EscapeText(&escaped, []byte("\n"+report))
						value.Text = strings.Replace(value.Text, "</result>", escaped.String()+"</result>", 1)
						e.msg.Parts[i] = value
						e.expandableOutput = true
					}
				}
			}
			continue
		}
		if e.result == nil || e.status == chat.ToolStatusCanceled {
			continue
		}
		switch e.tool {
		// These successful operations intentionally render just an acknowledgment,
		// fixed status summary, or an always-visible answer in the production UI.
		case tools.TodosToolName, tools.MemoryUpsertToolName, tools.MemoryRemoveToolName, tools.ProjectCreateToolName, tools.ProjectStatusToolName, tools.ProjectUpdateToolName, tools.ProjectNotesToolName, tools.ProjectCompleteToolName, tools.TaskStopToolName, tools.TaskRestartToolName, tools.TaskContinueToolName, tools.QuestionToolName:
			e.Note = "Production renderer has no collapsible output body."
			continue
		}
		e.expandableOutput = true
		switch e.tool {
		case tools.ViewToolName:
			e.result.Content = previewSource("view")
		case tools.WriteToolName:
			e.params.(map[string]any)["content"] = previewSource("written")
		case tools.EditToolName, tools.MultiEditToolName, tools.ReplaceSymbolToolName:
			meta := previewObject(e.result.Metadata)
			meta["old_content"] = previewSource("before")
			meta["new_content"] = previewSource("after")
			e.result.Metadata = previewJSON(meta)
			params := e.params.(map[string]any)
			switch e.tool {
			case tools.EditToolName:
				params["old_string"] = previewSource("before")
				params["new_string"] = previewSource("after")
			case tools.MultiEditToolName:
				params["edits"] = []any{map[string]any{"old_string": previewSource("before"), "new_string": previewSource("after")}}
			case tools.ReplaceSymbolToolName:
				params["replacement"] = previewSource("after")
			}
		case tools.DefinitionToolName:
			e.result.Metadata = previewJSON(map[string]any{"file_path": "/preview/crush/example.go", "content": previewSource("definition")})
		case tools.SearchToolName:
			params := e.params.(map[string]any)
			rows := []string{}
			if params["mode"] == "files" {
				for i := 1; i <= 16; i++ {
					rows = append(rows, fmt.Sprintf("/preview/crush/internal/preview/step_%02d.go", i))
				}
			} else {
				rows = append(rows, "Found 16 matches")
				for i := 1; i <= 16; i++ {
					rows = append(rows, fmt.Sprintf("/preview/crush/internal/preview/step_%02d.go:", i), fmt.Sprintf("  Line %d, Char 6: func PreviewStep%02d() string { return \"ready\" }", i+2, i))
				}
			}
			e.result.Content = strings.Join(rows, "\n")
		case tools.LSToolName:
			rows := []string{"/preview/crush/"}
			for i := 1; i <= 16; i++ {
				rows = append(rows, fmt.Sprintf("  - preview_step_%02d.go", i))
			}
			e.result.Content = strings.Join(rows, "\n")
		case tools.CodebaseSearchToolName:
			rows := []string{}
			for i := 1; i <= 8; i++ {
				rows = append(rows, fmt.Sprintf("%d. internal/preview/step_%02d.go:1-27 (score 0.98, symbol PreviewStep%02d)\n%s", i, i, i, previewSource("semantic match")))
			}
			e.result.Content = strings.Join(rows, "\n")
			e.params.(map[string]any)["count"] = 8
		case tools.MemoryListToolName:
			entries := []any{}
			for i := 1; i <= 12; i++ {
				entries = append(entries, map[string]any{"name": fmt.Sprintf("Preview convention %02d", i), "description": fmt.Sprintf("Review layout stage %02d and retain spacing observations", i), "content": report})
			}
			e.result.Content = previewJSON(entries)
		case tools.TaskListToolName:
			var rows []map[string]any
			if err := json.Unmarshal([]byte(e.result.Content), &rows); err != nil {
				panic(err)
			}
			for i := 0; i < 6; i++ {
				row := map[string]any{}
				for k, v := range rows[i] {
					row[k] = v
				}
				row["id"] = fmt.Sprintf("a000000%02d", i+6)
				rows = append(rows, row)
			}
			for i := range rows {
				rows[i]["description"] = fmt.Sprintf("Fixture task %02d\n%s", i+1, previewReport("Task review"))
			}
			e.result.Content = previewJSON(rows)
		case tools.TaskOutputToolName:
			result := previewObject(e.result.Content)
			result["output"] = report
			e.result.Content = previewJSON(result)
		case tools.TrafficLogsToolName:
			meta := previewObject(e.result.Metadata)
			base := meta["records"].([]any)[0].(map[string]any)
			records := []any{}
			for i := 1; i <= 8; i++ {
				record := map[string]any{}
				for k, v := range base {
					record[k] = v
				}
				record["record_id"] = fmt.Sprintf("preview-traffic-%d", i)
				records = append(records, record)
			}
			meta["records"] = records
			e.result.Metadata = previewJSON(meta)
			e.params.(map[string]any)["limit"] = 8
		case tools.TrafficLogDetailToolName:
			meta := previewObject(e.result.Metadata)
			record := meta["record"].(map[string]any)
			record["body"] = report
			record["body_encoding"] = "utf-8"
			record["headers"] = []any{map[string]any{"name": "content-type", "values": []string{"text/plain"}}}
			e.result.Metadata = previewJSON(meta)
			e.params.(map[string]any)["include_body"] = true
		case tools.ImagegenToolName:
			if e.result.Metadata != "" {
				meta := previewObject(e.result.Metadata)
				outputs := []string{}
				for i := 1; i <= 16; i++ {
					outputs = append(outputs, fmt.Sprintf("/preview/crush/generated/variant_%02d.png", i))
				}
				meta["outputs"] = outputs
				e.result.Metadata = previewJSON(meta)
				e.params.(map[string]any)["n"] = 16
			} else {
				e.result.Content += "\n" + report
			}
		case tools.JQToolName:
			values := []map[string]string{}
			rows := []string{}
			for i := 1; i <= 24; i++ {
				name := fmt.Sprintf("preview_step_%02d", i)
				values = append(values, map[string]string{"name": name})
				rows = append(rows, previewJSON(name))
			}
			e.params.(map[string]any)["input"] = previewJSON(map[string]any{"items": values})
			e.result.Content = strings.Join(rows, "\n")
		default:
			e.result.Content += "\n" + report
		}
		if e.tool == tools.JobOutputToolName || e.tool == tools.JobKillToolName {
			e.params = map[string]any{"shell_id": "s00000001"}
		}
	}
}
