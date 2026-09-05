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

func TestTrafficLogsRendererKeepsListCompactWhenExpanded(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "traffic-logs",
		Name:     tools.TrafficLogsToolName,
		Input:    `{"search":"api","protocol":"http","direction":"outbound","limit":4}`,
		Finished: true,
	}
	records := make([]tools.TrafficLogRecord, 4)
	for index := range records {
		records[index] = tools.TrafficLogRecord{
			RecordID:   "http/response/" + string(rune('1'+index)),
			Timestamp:  time.Date(2026, 8, 24, 3, index, 5, 6, time.UTC),
			ProcessID:  400 + index,
			TraceID:    "trace-" + string(rune('a'+index)),
			Protocol:   "http",
			Direction:  "outbound",
			Phase:      "response",
			Method:     "POST",
			URL:        "https://api.example.test/record/" + string(rune('1'+index)),
			StatusCode: 200 + index,
		}
	}
	metadata, err := json.Marshal(tools.TrafficLogsResponseMetadata{Records: records})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: call.ID, Content: "model-facing traffic output", Metadata: string(metadata)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")

	collapsed := ansi.Strip(item.Render(100))
	require.Contains(t, collapsed, "Traffic Logs")
	require.Contains(t, collapsed, "Record http/response/1")
	require.NotContains(t, collapsed, "Record http/response/4")
	require.Contains(t, collapsed, "… 1 more records; expand to show all")
	require.Contains(t, collapsed, "traffic_log_detail or traffic_log_search")

	item.(Expandable).ToggleExpanded()
	expanded := ansi.Strip(item.Render(100))
	require.Contains(t, expanded, "Record http/response/4")
	require.NotContains(t, expanded, "Header")
	require.NotContains(t, expanded, "Instruction")
	require.NotContains(t, expanded, "Body:")
	require.NotContains(t, expanded, "model-facing traffic output")
}

func TestTrafficLogDetailRendererShowsBoundedRecordOnExpand(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "traffic-detail",
		Name:     tools.TrafficLogDetailToolName,
		Input:    `{"record_id":"http/request/7","include_body":true}`,
		Finished: true,
	}
	record := tools.TrafficLogRecord{
		RecordID:      "http/request/7",
		Timestamp:     time.Date(2026, 8, 24, 4, 0, 0, 0, time.UTC),
		ProcessID:     42,
		TraceID:       "trace-7",
		Protocol:      "http",
		Direction:     "outbound",
		Phase:         "request",
		Method:        "POST",
		URL:           "https://example.test/v1/messages",
		Headers:       []tools.TrafficLogHeader{{Name: "Content-Type", Values: []string{"application/json"}}},
		BodyEncoding:  "utf-8",
		ContentLength: 100,
		Shape:         "{messages[1]}",
		UserMessages:  []string{"hello"},
		Body:          `{"messages":[{"role":"user","content":"hello"}]}`,
	}
	metadata, err := json.Marshal(tools.TrafficLogDetailResponseMetadata{Record: record})
	require.NoError(t, err)
	result := &message.ToolResult{ToolCallID: call.ID, Content: "fallback", Metadata: string(metadata)}
	item := NewToolMessageItem(&sty, "message", call, result, false, "")

	collapsed := ansi.Strip(item.Render(100))
	require.Contains(t, collapsed, "Traffic Log Detail")
	require.Contains(t, collapsed, "http/request/7")
	require.NotContains(t, collapsed, "hello")

	item.(Expandable).ToggleExpanded()
	expanded := ansi.Strip(item.Render(100))
	require.Contains(t, expanded, "Header Content-Type:")
	require.Contains(t, expanded, "User message:")
	require.Contains(t, expanded, "Body:")
	require.Contains(t, expanded, "hello")
}

func TestTrafficLogSearchRendererShowsOnlyBoundedToolResult(t *testing.T) {
	sty := styles.CharmtonePantera()
	call := message.ToolCall{
		ID:       "traffic-search",
		Name:     tools.TrafficLogSearchToolName,
		Input:    `{"record_id":"http/request/7","query":"needle"}`,
		Finished: true,
	}
	result := &message.ToolResult{ToolCallID: call.ID, Content: "1 body matches\nmatch 1 offset=12: before [[needle]] after"}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", call, result, false, "").Render(100))
	require.Contains(t, view, "Traffic Log Search")
	require.Contains(t, view, "http/request/7")
	require.Contains(t, view, "[[needle]]")
}

func TestTrafficLogRenderersHandleInvalidInputAndCompactMode(t *testing.T) {
	sty := styles.CharmtonePantera()
	result := &message.ToolResult{ToolCallID: "traffic-logs", Content: "result"}
	invalid := message.ToolCall{ID: "traffic-logs", Name: tools.TrafficLogsToolName, Input: `{malformed`, Finished: true}
	view := ansi.Strip(NewToolMessageItem(&sty, "message", invalid, result, false, "").Render(80))
	require.Contains(t, view, "Invalid parameters")

	valid := message.ToolCall{ID: "traffic-logs", Name: tools.TrafficLogsToolName, Input: `{"search":"needle"}`, Finished: true}
	item := NewToolMessageItem(&sty, "message", valid, result, false, "")
	item.(Compactable).SetCompact(true)
	compact := ansi.Strip(item.Render(80))
	require.Contains(t, compact, "Traffic Logs")
	require.Contains(t, compact, "needle")
	require.NotContains(t, compact, "result")
	require.Equal(t, 1, len(strings.Split(compact, "\n")))
}
