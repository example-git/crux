package chat

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/stretchr/testify/require"
)

func TestTrafficLogsRendererCollapsesAndExpandsStructuredRecords(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "traffic-logs",
		Name:     tools.TrafficLogsToolName,
		Input:    `{"search":"api","protocol":"http","direction":"outbound","limit":4,"include_body":true}`,
		Finished: true,
	}
	records := make([]tools.TrafficLogRecord, 4)
	for index := range records {
		records[index] = tools.TrafficLogRecord{
			ID:            int64(101 + index),
			Timestamp:     time.Date(2026, 8, 24, 3, index, 5, 6, time.UTC),
			ProcessID:     400 + index,
			TraceID:       "trace-" + string(rune('a'+index)),
			Protocol:      "http",
			Direction:     "outbound",
			Phase:         "response",
			Method:        "POST",
			URL:           "https://api.example.test/record/" + string(rune('1'+index)),
			StatusCode:    200 + index,
			Headers:       []tools.TrafficLogHeader{{Name: "Content-Type", Values: []string{"application/json"}}},
			BodyEncoding:  "utf-8",
			ContentLength: int64(1000 + index),
			DurationMS:    int64(30 + index),
			MessageType:   1,
			Error:         "record error",
			Shape:         "{input[1], model}",
			Instructions:  []string{"follow the project instructions"},
			UserMessages:  []string{"inspect the request"},
			AssistantMessages: []string{
				"request inspected",
			},
			Body: "{\"input\":\"first line\nsecond line with enough text to exercise wrapping in the expanded traffic record body\"}",
		}
	}
	metadata, err := json.Marshal(tools.TrafficLogsResponseMetadata{Records: records})
	require.NoError(t, err)
	result := &message.ToolResult{
		ToolCallID: call.ID,
		Content:    "model-facing traffic output",
		Metadata:   string(metadata),
	}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")

	collapsed := ansi.Strip(item.Render(100))
	require.Contains(t, collapsed, "Traffic Logs")
	require.Contains(t, collapsed, "api")
	require.Contains(t, collapsed, "4 traffic records")
	require.Contains(t, collapsed, "Record 101")
	require.Contains(t, collapsed, "Record 103")
	require.NotContains(t, collapsed, "Record 104")
	for _, label := range []string{"Timestamp:", "Protocol:", "Direction:", "Phase:", "Method:", "URL:", "Status:"} {
		require.Contains(t, collapsed, label)
	}
	require.NotContains(t, collapsed, "Trace ID:")
	require.NotContains(t, collapsed, "Header Content-Type:")
	require.NotContains(t, collapsed, "Body:")
	require.Contains(t, collapsed, "… 1 more records; expand for all fields")

	expandable, ok := item.(Expandable)
	require.True(t, ok)
	expandable.ToggleExpanded()
	expanded := ansi.Strip(item.Render(100))
	require.Contains(t, expanded, "Record 104")
	for _, label := range []string{
		"Record ID:",
		"Process ID:",
		"Trace ID:",
		"Message type:",
		"Content length:",
		"Duration (ms):",
		"Error:",
		"Body encoding:",
		"JSON shape:",
		"Header Content-Type:",
		"Instruction:",
		"User message:",
		"Assistant message:",
		"Body:",
	} {
		require.Contains(t, expanded, label)
	}
	require.Contains(t, expanded, "trace-d")
	require.Contains(t, expanded, "follow the project instructions")
	require.Contains(t, expanded, "second line")
	require.NotContains(t, expanded, "… 1 more records")
	require.NotContains(t, expanded, "model-facing traffic output")
}

func TestTrafficLogsRendererShowsBodyExclusionAndLegacyFallback(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{ID: "traffic-logs", Name: tools.TrafficLogsToolName, Input: `{}`, Finished: true}
	metadata, err := json.Marshal(tools.TrafficLogsResponseMetadata{Records: []tools.TrafficLogRecord{{
		ID:        1,
		Timestamp: time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
		Protocol:  "websocket",
		Direction: "inbound",
		Phase:     "frame",
	}}})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: call.ID, Content: "legacy text", Metadata: string(metadata)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")
	item.(Expandable).ToggleExpanded()
	expanded := ansi.Strip(item.Render(80))
	require.Contains(t, expanded, "Body:")
	require.Contains(t, expanded, "Not included; query with include_body=true")
	require.Contains(t, expanded, "Headers:")
	require.Contains(t, expanded, "Instruction:")

	legacyResult := &message.ToolResult{ToolCallID: call.ID, Content: "persisted traffic plaintext"}
	legacy := NewToolMessageItem(&sty, "message", call, legacyResult, false, "")
	collapsedLegacy := ansi.Strip(legacy.Render(80))
	require.Contains(t, collapsedLegacy, "persisted traffic plaintext")
	legacy.(Expandable).ToggleExpanded()
	expandedLegacy := ansi.Strip(legacy.Render(80))
	require.Contains(t, expandedLegacy, "persisted traffic plaintext")
}

func TestTrafficLogsRendererHandlesInvalidInputAndCompactMode(t *testing.T) {
	sty := styles.CharmtonePantera()
	result := &message.ToolResult{ToolCallID: "traffic-logs", Content: "result"}
	invalid := message.ToolCall{ID: "traffic-logs", Name: tools.TrafficLogsToolName, Input: `{malformed`, Finished: true}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", invalid, result, false, "").Render(80))
	require.Contains(t, view, "Invalid parameters")

	valid := message.ToolCall{ID: "traffic-logs", Name: tools.TrafficLogsToolName, Input: `{"search":"needle"}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", valid, result, false, "")
	compactable, ok := item.(Compactable)
	require.True(t, ok)
	compactable.SetCompact(true)
	compact := ansi.Strip(item.Render(80))
	require.Contains(t, compact, "Traffic Logs")
	require.Contains(t, compact, "needle")
	require.NotContains(t, compact, "result")
	require.Equal(t, 1, len(strings.Split(compact, "\n")))
}
