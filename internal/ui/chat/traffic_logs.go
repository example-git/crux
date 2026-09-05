package chat

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
)

const collapsedTrafficRecordLimit = 3

type TrafficLogsToolRenderContext struct{}

func NewTrafficLogsToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &TrafficLogsToolRenderContext{}, canceled)
}

func (r *TrafficLogsToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Traffic Logs", opts.Anim, opts.Compact)
	}

	var params tools.TrafficLogsParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}

	header := toolHeader(sty, opts.Status, "Traffic Logs", cappedWidth, opts, trafficLogToolParams(params)...)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}

	var metadata tools.TrafficLogsResponseMetadata
	if opts.Result.Metadata == "" || json.Unmarshal([]byte(opts.Result.Metadata), &metadata) != nil || len(metadata.Records) == 0 {
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}

	bodyWidth := max(1, cappedWidth-toolBodyLeftPaddingTotal)
	parts := []string{sty.Tool.ParamKey.Render(fmt.Sprintf("%d traffic records", len(metadata.Records)))}
	limit := len(metadata.Records)
	if !opts.ExpandedContent {
		limit = min(limit, collapsedTrafficRecordLimit)
	}
	for index, record := range metadata.Records[:limit] {
		parts = append(parts, renderTrafficLogRecord(sty, record, bodyWidth, false, false, index+1))
	}
	if !opts.ExpandedContent {
		hidden := len(metadata.Records) - limit
		if hidden > 0 {
			parts = append(parts, sty.Tool.ContentTruncation.Render(fmt.Sprintf("… %d more records; expand to show all", hidden)))
		}
	}
	parts = append(parts, sty.Tool.ContentTruncation.Render("Use traffic_log_detail or traffic_log_search with a record ID"))
	return joinToolParts(header, sty.Tool.Body.Render(strings.Join(parts, "\n")))
}

type TrafficLogDetailToolRenderContext struct{}

type TrafficLogSearchToolRenderContext struct{}

func NewTrafficLogDetailToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &TrafficLogDetailToolRenderContext{}, canceled)
}

func NewTrafficLogSearchToolMessageItem(
	sty *styles.Styles,
	toolCall message.ToolCall,
	result *message.ToolResult,
	canceled bool,
) ToolMessageItem {
	return newBaseToolMessageItem(sty, toolCall, result, &TrafficLogSearchToolRenderContext{}, canceled)
}

func (r *TrafficLogDetailToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Traffic Log Detail", opts.Anim, opts.Compact)
	}
	var params tools.TrafficLogDetailParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	header := toolHeader(sty, opts.Status, "Traffic Log Detail", cappedWidth, opts, params.RecordID)
	if opts.Compact {
		return header
	}
	if earlyState, ok := toolEarlyStateContent(sty, opts, cappedWidth); ok {
		return joinToolParts(header, earlyState)
	}
	if opts.HasEmptyResult() {
		return header
	}
	var metadata tools.TrafficLogDetailResponseMetadata
	if opts.Result.Metadata == "" || json.Unmarshal([]byte(opts.Result.Metadata), &metadata) != nil || metadata.Record.RecordID == "" {
		bodyWidth := cappedWidth - toolBodyLeftPaddingTotal
		body := sty.Tool.Body.Render(toolOutputPlainContent(sty, opts.Result.Content, bodyWidth, opts.ExpandedContent))
		return joinToolParts(header, body)
	}
	bodyWidth := max(1, cappedWidth-toolBodyLeftPaddingTotal)
	body := renderTrafficLogRecord(sty, metadata.Record, bodyWidth, opts.ExpandedContent, params.IncludeBody, 1)
	if !opts.ExpandedContent {
		body += "\n" + sty.Tool.ContentTruncation.Render("Expand for bounded headers, summaries, and body details")
	}
	return joinToolParts(header, sty.Tool.Body.Render(body))
}

func (r *TrafficLogSearchToolRenderContext) RenderTool(sty *styles.Styles, width int, opts *ToolRenderOpts) string {
	cappedWidth := cappedMessageWidth(width)
	if opts.IsPending() {
		return pendingTool(sty, "Traffic Log Search", opts.Anim, opts.Compact)
	}
	var params tools.TrafficLogSearchParams
	if err := json.Unmarshal([]byte(opts.ToolCall.Input), &params); err != nil {
		return toolErrorContent(sty, &message.ToolResult{Content: "Invalid parameters"}, cappedWidth)
	}
	header := toolHeader(sty, opts.Status, "Traffic Log Search", cappedWidth, opts, params.RecordID, params.Query)
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

func trafficLogToolParams(params tools.TrafficLogsParams) []string {
	main := params.Search
	if main == "" {
		main = "recent traffic"
	}
	values := []string{main}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "protocol", value: params.Protocol},
		{name: "direction", value: params.Direction},
		{name: "phase", value: params.Phase},
		{name: "sort", value: params.Sort},
		{name: "limit", value: formatNonZero(params.Limit)},
	} {
		if field.value != "" {
			values = append(values, field.name, field.value)
		}
	}
	return values
}

func renderTrafficLogRecord(sty *styles.Styles, record tools.TrafficLogRecord, width int, expanded, includeBody bool, position int) string {
	identifier := strconv.Itoa(position)
	if record.RecordID != "" {
		identifier = record.RecordID
	} else if record.ID != 0 {
		identifier = strconv.FormatInt(record.ID, 10)
	}
	parts := []string{sty.Tool.NameNested.Render("Record " + identifier)}
	parts = append(parts,
		trafficLogField(sty, "Timestamp", record.Timestamp.Format(time.RFC3339Nano), width, expanded),
		trafficLogField(sty, "Protocol", record.Protocol, width, expanded),
		trafficLogField(sty, "Direction", record.Direction, width, expanded),
		trafficLogField(sty, "Phase", record.Phase, width, expanded),
		trafficLogField(sty, "Method", record.Method, width, expanded),
		trafficLogField(sty, "URL", record.URL, width, expanded),
		trafficLogField(sty, "Status", formatOptionalInt(record.StatusCode), width, expanded),
	)
	if !expanded {
		return strings.Join(parts, "\n")
	}

	parts = append(parts,
		trafficLogField(sty, "Record ID", identifier, width, true),
		trafficLogField(sty, "Process ID", strconv.Itoa(record.ProcessID), width, true),
		trafficLogField(sty, "Trace ID", record.TraceID, width, true),
		trafficLogField(sty, "Message type", formatOptionalInt(record.MessageType), width, true),
		trafficLogField(sty, "Content length", formatOptionalInt64(record.ContentLength), width, true),
		trafficLogField(sty, "Duration (ms)", formatOptionalInt64(record.DurationMS), width, true),
		trafficLogField(sty, "Error", record.Error, width, true),
		trafficLogField(sty, "Body encoding", record.BodyEncoding, width, true),
		trafficLogField(sty, "JSON shape", record.Shape, width, true),
	)
	if len(record.Headers) == 0 {
		parts = append(parts, trafficLogField(sty, "Headers", "—", width, true))
	} else {
		for _, header := range record.Headers {
			parts = append(parts, trafficLogField(sty, "Header "+header.Name, strings.Join(header.Values, ", "), width, true))
		}
	}
	parts = appendTrafficLogValues(parts, sty, "Instruction", record.Instructions, width)
	parts = appendTrafficLogValues(parts, sty, "User message", record.UserMessages, width)
	parts = appendTrafficLogValues(parts, sty, "Assistant message", record.AssistantMessages, width)
	body := record.Body
	if body == "" {
		if includeBody {
			body = "—"
		} else {
			body = "Not included; use traffic_log_detail with include_body=true"
		}
	}
	parts = append(parts, trafficLogField(sty, "Body", body, width, true))
	return strings.Join(parts, "\n")
}

func appendTrafficLogValues(parts []string, sty *styles.Styles, label string, values []string, width int) []string {
	if len(values) == 0 {
		return append(parts, trafficLogField(sty, label, "—", width, true))
	}
	for index, value := range values {
		fieldLabel := label
		if len(values) > 1 {
			fieldLabel += " " + strconv.Itoa(index+1)
		}
		parts = append(parts, trafficLogField(sty, fieldLabel, value, width, true))
	}
	return parts
}

func trafficLogField(sty *styles.Styles, label, value string, width int, wrap bool) string {
	labelWidth := min(20, max(8, width/2))
	if value == "" {
		value = "—"
	}
	value = common.StripCursorControl(value)
	labelText := fmt.Sprintf("%-*s", labelWidth, ansi.Truncate(label+":", labelWidth, "…"))
	prefix := sty.Tool.ParamKey.Render(labelText)
	valueWidth := max(1, width-lipgloss.Width(labelText))
	if !wrap {
		return prefix + sty.Tool.ParamMain.Render(ansi.Truncate(strings.Join(strings.Fields(value), " "), valueWidth, "…"))
	}

	var lines []string
	for sourceLine := range strings.SplitSeq(value, "\n") {
		wrapped := ansi.Hardwrap(sourceLine, valueWidth, false)
		lines = append(lines, strings.Split(wrapped, "\n")...)
	}
	if len(lines) == 0 {
		lines = []string{"—"}
	}
	result := prefix + sty.Tool.ParamMain.Render(lines[0])
	indent := strings.Repeat(" ", labelWidth)
	for _, line := range lines[1:] {
		result += "\n" + indent + sty.Tool.ParamMain.Render(line)
	}
	return result
}

func formatOptionalInt(value int) string {
	if value == 0 {
		return "—"
	}
	return strconv.Itoa(value)
}

func formatOptionalInt64(value int64) string {
	if value == 0 {
		return "—"
	}
	return strconv.FormatInt(value, 10)
}
