package tools

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	cruxlog "github.com/example-git/crux/internal/log"
)

const (
	TrafficLogsToolName      = "traffic_logs"
	defaultTrafficLogLimit   = 20
	maxTrafficLogLimit       = 100
	trafficOutputSnippetSize = 1200
	maxTrafficBodyOutput     = 20 * 1024
)

//go:embed traffic_logs.md
var trafficLogsDescription string

type TrafficLogsParams struct {
	Search      string `json:"search,omitempty" description:"Case-insensitive substring search across method, URL, headers, bodies, and errors"`
	Protocol    string `json:"protocol,omitempty" description:"Filter by protocol: http or websocket"`
	Direction   string `json:"direction,omitempty" description:"Filter by direction: inbound or outbound"`
	Phase       string `json:"phase,omitempty" description:"Filter by phase: request, response, handshake, or frame"`
	Since       string `json:"since,omitempty" description:"Only records at or after this RFC3339 timestamp"`
	Until       string `json:"until,omitempty" description:"Only records at or before this RFC3339 timestamp"`
	Sort        string `json:"sort,omitempty" description:"Timestamp sort order: asc or desc (default desc)"`
	Limit       int    `json:"limit,omitempty" description:"Maximum records to return (default 20, max 100)"`
	IncludeBody bool   `json:"include_body,omitempty" description:"Include truncated raw bodies in addition to compact summaries"`
}

type TrafficLogsResponseMetadata struct {
	Records []TrafficLogRecord `json:"records"`
}

type TrafficLogRecord struct {
	ID                int64              `json:"id"`
	Timestamp         time.Time          `json:"timestamp"`
	ProcessID         int                `json:"process_id"`
	TraceID           string             `json:"trace_id"`
	Protocol          string             `json:"protocol"`
	Direction         string             `json:"direction"`
	Phase             string             `json:"phase"`
	Method            string             `json:"method"`
	URL               string             `json:"url"`
	StatusCode        int                `json:"status_code"`
	Headers           []TrafficLogHeader `json:"headers"`
	BodyEncoding      string             `json:"body_encoding"`
	ContentLength     int64              `json:"content_length"`
	DurationMS        int64              `json:"duration_ms"`
	MessageType       int                `json:"message_type"`
	Error             string             `json:"error"`
	Shape             string             `json:"shape"`
	Instructions      []string           `json:"instructions"`
	UserMessages      []string           `json:"user_messages"`
	AssistantMessages []string           `json:"assistant_messages"`
	Body              string             `json:"body"`
}

type TrafficLogHeader struct {
	Name   string   `json:"name"`
	Values []string `json:"values"`
}

func NewTrafficLogsTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		TrafficLogsToolName,
		trafficLogsDescription,
		func(ctx context.Context, params TrafficLogsParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, metadata, err := runTrafficLogs(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}

func runTrafficLogs(ctx context.Context, params TrafficLogsParams) (string, TrafficLogsResponseMetadata, error) {
	query, err := trafficQuery(params)
	if err != nil {
		return "", TrafficLogsResponseMetadata{}, err
	}
	path, err := cruxlog.TrafficDatabasePath()
	if err != nil {
		return "", TrafficLogsResponseMetadata{}, err
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "No traffic database found", TrafficLogsResponseMetadata{}, nil
		}
		return "", TrafficLogsResponseMetadata{}, fmt.Errorf("access traffic database: %w", err)
	}
	database, err := cruxlog.OpenTrafficDatabaseReadOnly()
	if err != nil {
		return "", TrafficLogsResponseMetadata{}, err
	}
	defer database.Close()
	events, err := cruxlog.QueryTraffic(ctx, database, query)
	if err != nil {
		return "", TrafficLogsResponseMetadata{}, fmt.Errorf("query traffic database: %w", err)
	}
	if len(events) == 0 {
		return "No matching traffic records", TrafficLogsResponseMetadata{}, nil
	}
	entries := make([]string, 0, len(events))
	metadata := TrafficLogsResponseMetadata{Records: make([]TrafficLogRecord, 0, len(events))}
	for _, event := range events {
		entries = append(entries, formatTrafficEvent(event, params.IncludeBody))
		metadata.Records = append(metadata.Records, trafficLogRecord(event, params.IncludeBody))
	}
	return strings.Join(entries, "\n"), metadata, nil
}

func trafficQuery(params TrafficLogsParams) (cruxlog.TrafficQuery, error) {
	if params.Protocol != "" && params.Protocol != "http" && params.Protocol != "websocket" {
		return cruxlog.TrafficQuery{}, fmt.Errorf("protocol must be one of: http, websocket")
	}
	if params.Direction != "" && params.Direction != "inbound" && params.Direction != "outbound" {
		return cruxlog.TrafficQuery{}, fmt.Errorf("direction must be one of: inbound, outbound")
	}
	validPhases := map[string]bool{"": true, "request": true, "response": true, "handshake": true, "frame": true}
	if !validPhases[params.Phase] {
		return cruxlog.TrafficQuery{}, fmt.Errorf("phase must be one of: request, response, handshake, frame")
	}
	if params.Protocol == "http" && (params.Phase == "handshake" || params.Phase == "frame") {
		return cruxlog.TrafficQuery{}, fmt.Errorf("phase %q is not valid for protocol http", params.Phase)
	}
	if params.Protocol == "websocket" && (params.Phase == "request" || params.Phase == "response") {
		return cruxlog.TrafficQuery{}, fmt.Errorf("phase %q is not valid for protocol websocket", params.Phase)
	}
	if params.Sort != "" && params.Sort != "asc" && params.Sort != "desc" {
		return cruxlog.TrafficQuery{}, fmt.Errorf("sort must be one of: asc, desc")
	}
	limit := params.Limit
	if limit == 0 {
		limit = defaultTrafficLogLimit
	}
	if limit < 1 || limit > maxTrafficLogLimit {
		return cruxlog.TrafficQuery{}, fmt.Errorf("limit must be between 1 and %d", maxTrafficLogLimit)
	}
	parseTime := func(name, value string) (time.Time, error) {
		if value == "" {
			return time.Time{}, nil
		}
		parsed, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return time.Time{}, fmt.Errorf("%s must be an RFC3339 timestamp: %w", name, err)
		}
		return parsed, nil
	}
	since, err := parseTime("since", params.Since)
	if err != nil {
		return cruxlog.TrafficQuery{}, err
	}
	until, err := parseTime("until", params.Until)
	if err != nil {
		return cruxlog.TrafficQuery{}, err
	}
	if !since.IsZero() && !until.IsZero() && since.After(until) {
		return cruxlog.TrafficQuery{}, fmt.Errorf("since must not be after until")
	}
	return cruxlog.TrafficQuery{
		Search:      params.Search,
		Protocol:    params.Protocol,
		Direction:   params.Direction,
		Phase:       params.Phase,
		Since:       since,
		Until:       until,
		Sort:        params.Sort,
		Limit:       limit,
		IncludeBody: true,
	}, nil
}

func formatTrafficEvent(event cruxlog.TrafficEvent, includeBody bool) string {
	var line strings.Builder
	fmt.Fprintf(&line, "%s id=%d pid=%d trace=%s %s %s %s", event.Timestamp.Format(time.RFC3339Nano), event.ID, event.ProcessID, event.TraceID, event.Protocol, event.Direction, event.Phase)
	if event.Method != "" {
		fmt.Fprintf(&line, " %s", event.Method)
	}
	if event.URL != "" {
		fmt.Fprintf(&line, " %s", event.URL)
	}
	if event.StatusCode != 0 {
		fmt.Fprintf(&line, " status=%d", event.StatusCode)
	}
	if event.MessageType != 0 {
		fmt.Fprintf(&line, " message_type=%d", event.MessageType)
	}
	if event.ContentLength != 0 {
		fmt.Fprintf(&line, " bytes=%d", event.ContentLength)
	}
	if event.DurationMS != 0 {
		fmt.Fprintf(&line, " duration_ms=%d", event.DurationMS)
	}
	if event.Error != "" {
		fmt.Fprintf(&line, " error=%q", compactTrafficText(event.Error))
	}
	if event.Body == "" {
		return line.String()
	}
	shape, instructions, userMessages, assistantMessages := summarizeTrafficBody(event.Body, event.BodyEncoding)
	if shape != "" {
		fmt.Fprintf(&line, "\n  shape: %s", shape)
	}
	appendTrafficValues(&line, "instructions", instructions)
	appendTrafficValues(&line, "user", userMessages)
	appendTrafficValues(&line, "assistant", assistantMessages)
	if includeBody {
		body := event.Body
		if len(body) > maxTrafficBodyOutput {
			body = body[:maxTrafficBodyOutput] + fmt.Sprintf("\n[truncated %d bytes]", len(event.Body)-maxTrafficBodyOutput)
		}
		fmt.Fprintf(&line, "\n  body(%s): %s", event.BodyEncoding, body)
	}
	return line.String()
}

func trafficLogRecord(event cruxlog.TrafficEvent, includeBody bool) TrafficLogRecord {
	record := TrafficLogRecord{
		ID:            event.ID,
		Timestamp:     event.Timestamp,
		ProcessID:     event.ProcessID,
		TraceID:       event.TraceID,
		Protocol:      event.Protocol,
		Direction:     event.Direction,
		Phase:         event.Phase,
		Method:        event.Method,
		URL:           event.URL,
		StatusCode:    event.StatusCode,
		BodyEncoding:  event.BodyEncoding,
		ContentLength: event.ContentLength,
		DurationMS:    event.DurationMS,
		MessageType:   event.MessageType,
		Error:         compactTrafficText(event.Error),
	}
	headerNames := make([]string, 0, len(event.Headers))
	for name := range event.Headers {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	for _, name := range headerNames {
		record.Headers = append(record.Headers, TrafficLogHeader{Name: name, Values: event.Headers[name]})
	}
	if event.Body == "" {
		return record
	}
	record.Shape, record.Instructions, record.UserMessages, record.AssistantMessages = summarizeTrafficBody(event.Body, event.BodyEncoding)
	if includeBody {
		record.Body = event.Body
		if len(record.Body) > maxTrafficBodyOutput {
			record.Body = record.Body[:maxTrafficBodyOutput] + fmt.Sprintf("\n[truncated %d bytes]", len(event.Body)-maxTrafficBodyOutput)
		}
	}
	return record
}

func appendTrafficValues(builder *strings.Builder, label string, values []string) {
	for _, value := range values {
		fmt.Fprintf(builder, "\n  %s: %s", label, compactTrafficText(value))
	}
}

func summarizeTrafficBody(body, encoding string) (string, []string, []string, []string) {
	if strings.HasPrefix(encoding, "base64") {
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return "binary", nil, nil, nil
		}
		body = string(decoded)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "non-JSON", nil, nil, nil
	}
	shape := trafficJSONShape(value)
	var instructions, users, assistants []string
	walkTrafficJSON(value, "", &instructions, &users, &assistants)
	return shape, uniqueTrafficValues(instructions), uniqueTrafficValues(users), uniqueTrafficValues(assistants)
}

func trafficJSONShape(value any) string {
	switch typed := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key, child := range typed {
			suffix := ""
			if list, ok := child.([]any); ok {
				suffix = fmt.Sprintf("[%d]", len(list))
			}
			keys = append(keys, key+suffix)
		}
		sort.Strings(keys)
		return "{" + strings.Join(keys, ", ") + "}"
	case []any:
		return fmt.Sprintf("array[%d]", len(typed))
	default:
		return fmt.Sprintf("%T", value)
	}
}

func walkTrafficJSON(value any, parentKey string, instructions, users, assistants *[]string) {
	switch typed := value.(type) {
	case map[string]any:
		if role, _ := typed["role"].(string); role == "user" || role == "assistant" || role == "system" || role == "developer" {
			text := trafficText(typed["content"])
			if text == "" {
				text = trafficText(typed["text"])
			}
			switch role {
			case "user":
				*users = append(*users, text)
			case "assistant":
				*assistants = append(*assistants, text)
			default:
				*instructions = append(*instructions, text)
			}
		}
		for key, child := range typed {
			lower := strings.ToLower(key)
			if lower == "instructions" || lower == "system" || lower == "developer" {
				if text := trafficText(child); text != "" {
					*instructions = append(*instructions, text)
				}
			}
			walkTrafficJSON(child, lower, instructions, users, assistants)
		}
	case []any:
		for _, child := range typed {
			walkTrafficJSON(child, parentKey, instructions, users, assistants)
		}
	case string:
		if parentKey == "delta" {
			*assistants = append(*assistants, typed)
		}
	}
}

func trafficText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, child := range typed {
			if text := trafficText(child); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		for _, key := range []string{"text", "input_text", "output_text", "content"} {
			if text := trafficText(typed[key]); text != "" {
				return text
			}
		}
	}
	return ""
}

func uniqueTrafficValues(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func compactTrafficText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	if len(value) > trafficOutputSnippetSize {
		return value[:trafficOutputSnippetSize] + fmt.Sprintf("... [%d more bytes]", len(value)-trafficOutputSnippetSize)
	}
	return value
}
