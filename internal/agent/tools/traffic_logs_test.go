package tools

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/stretchr/testify/require"
)

func TestTrafficQueryDefaultsAndValidatesFilters(t *testing.T) {
	query, err := trafficQuery(TrafficLogsParams{})
	require.NoError(t, err)
	require.Equal(t, defaultTrafficLogLimit, query.Limit)
	require.True(t, query.IncludeBody)
	require.Zero(t, query.BodyLimit)

	tests := []struct {
		name   string
		params TrafficLogsParams
		want   string
	}{
		{name: "protocol", params: TrafficLogsParams{Protocol: "tcp"}, want: "protocol must"},
		{name: "direction", params: TrafficLogsParams{Direction: "sideways"}, want: "direction must"},
		{name: "phase", params: TrafficLogsParams{Phase: "connect"}, want: "phase must"},
		{name: "http frame", params: TrafficLogsParams{Protocol: "http", Phase: "frame"}, want: "not valid"},
		{name: "websocket request", params: TrafficLogsParams{Protocol: "websocket", Phase: "request"}, want: "not valid"},
		{name: "sort", params: TrafficLogsParams{Sort: "newest"}, want: "sort must"},
		{name: "limit low", params: TrafficLogsParams{Limit: -1}, want: "limit must"},
		{name: "limit high", params: TrafficLogsParams{Limit: maxTrafficLogLimit + 1}, want: "limit must"},
		{name: "since", params: TrafficLogsParams{Since: "yesterday"}, want: "RFC3339"},
		{name: "range", params: TrafficLogsParams{Since: "2026-08-24T01:00:00Z", Until: "2026-08-24T00:00:00Z"}, want: "must not be after"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := trafficQuery(test.params)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestSummarizeTrafficBodyExtractsShapeAndMessages(t *testing.T) {
	body := `{"instructions":"system prompt","input":[{"role":"user","content":[{"type":"input_text","text":"hello"}]},{"role":"assistant","content":"answer"}]}`
	shape, instructions, users, assistants := summarizeTrafficBody(body, "utf-8")
	require.Equal(t, "{input[2], instructions}", shape)
	require.Equal(t, []string{"system prompt"}, instructions)
	require.Equal(t, []string{"hello"}, users)
	require.Equal(t, []string{"answer"}, assistants)

	encoded := base64.StdEncoding.EncodeToString([]byte(`{"delta":"continued"}`))
	shape, _, _, assistants = summarizeTrafficBody(encoded, "base64")
	require.Equal(t, "{delta}", shape)
	require.Equal(t, []string{"continued"}, assistants)
}

func TestTrafficLogRecordCopiesStructuredFieldsAndBoundsBody(t *testing.T) {
	event := cruxlog.TrafficEvent{
		ID:            9,
		Timestamp:     time.Date(2026, 8, 24, 2, 3, 4, 5, time.UTC),
		ProcessID:     123,
		TraceID:       "trace-9",
		Protocol:      "http",
		Direction:     "outbound",
		Phase:         "response",
		Method:        "POST",
		URL:           "https://example.test/v1/responses",
		StatusCode:    200,
		Headers:       map[string][]string{"Z-Last": {"z"}, "A-First": {"a", "b"}},
		BodyEncoding:  "utf-8",
		ContentLength: 2048,
		DurationMS:    37,
		MessageType:   1,
		Error:         "  spaced\nerror  ",
		Body:          `{"instructions":"system prompt","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"}]}`,
	}

	withoutBody := trafficLogRecord(event, false)
	require.Equal(t, event.ID, withoutBody.ID)
	require.Equal(t, event.Timestamp, withoutBody.Timestamp)
	require.Equal(t, event.ProcessID, withoutBody.ProcessID)
	require.Equal(t, event.TraceID, withoutBody.TraceID)
	require.Equal(t, event.Protocol, withoutBody.Protocol)
	require.Equal(t, event.Direction, withoutBody.Direction)
	require.Equal(t, event.Phase, withoutBody.Phase)
	require.Equal(t, event.Method, withoutBody.Method)
	require.Equal(t, event.URL, withoutBody.URL)
	require.Equal(t, event.StatusCode, withoutBody.StatusCode)
	require.Equal(t, event.BodyEncoding, withoutBody.BodyEncoding)
	require.Equal(t, event.ContentLength, withoutBody.ContentLength)
	require.Equal(t, event.DurationMS, withoutBody.DurationMS)
	require.Equal(t, event.MessageType, withoutBody.MessageType)
	require.Equal(t, "spaced error", withoutBody.Error)
	require.Equal(t, []TrafficLogHeader{{Name: "A-First", Values: []string{"a", "b"}}, {Name: "Z-Last", Values: []string{"z"}}}, withoutBody.Headers)
	require.Equal(t, "{input[2], instructions}", withoutBody.Shape)
	require.Equal(t, []string{"system prompt"}, withoutBody.Instructions)
	require.Equal(t, []string{"hello"}, withoutBody.UserMessages)
	require.Equal(t, []string{"answer"}, withoutBody.AssistantMessages)
	require.Empty(t, withoutBody.Body)

	withBody := trafficLogRecord(event, true)
	require.Equal(t, event.Body, withBody.Body)

	event.Body = strings.Repeat("x", maxTrafficBodyOutput+5)
	bounded := trafficLogRecord(event, true)
	require.Contains(t, bounded.Body, "[truncated 5 bytes]")
	require.Less(t, len(bounded.Body), maxTrafficBodyOutput+100)
}

func TestFormatTrafficEventBoundsContextOutput(t *testing.T) {
	event := cruxlog.TrafficEvent{
		ID:            7,
		Timestamp:     time.Date(2026, 8, 24, 1, 2, 3, 4, time.UTC),
		ProcessID:     42,
		TraceID:       "trace-7",
		Protocol:      "websocket",
		Direction:     "outbound",
		Phase:         "frame",
		MessageType:   1,
		ContentLength: int64(maxTrafficBodyOutput + 1),
		Body:          `{"instructions":"` + strings.Repeat("x", maxTrafficBodyOutput) + `"}`,
		BodyEncoding:  "utf-8",
	}

	summary := formatTrafficEvent(event, false)
	require.Contains(t, summary, "trace=trace-7")
	require.Contains(t, summary, "shape: {instructions}")
	require.NotContains(t, summary, "body(utf-8)")
	require.Less(t, len(summary), 3000)

	withBody := formatTrafficEvent(event, true)
	require.Contains(t, withBody, "body(utf-8)")
	require.Contains(t, withBody, "[truncated")
	require.Less(t, len(withBody), maxTrafficBodyOutput+3000)
}
