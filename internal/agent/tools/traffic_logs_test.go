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
	require.False(t, query.IncludeBody)
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

func TestParseTrafficRecordIDRequiresCompositeTableIdentity(t *testing.T) {
	protocol, phase, id, err := parseTrafficRecordID("http/response/17")
	require.NoError(t, err)
	require.Equal(t, "http", protocol)
	require.Equal(t, "response", phase)
	require.EqualValues(t, 17, id)

	for _, value := range []string{"17", "http/frame/17", "websocket/response/17", "http/request/0", "http/request/nope"} {
		_, _, _, err := parseTrafficRecordID(value)
		require.Error(t, err, value)
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

func TestTrafficLogListRecordOmitsBodyDerivedFields(t *testing.T) {
	event := trafficTestEvent()
	record := trafficLogListRecord(event)
	require.Equal(t, "http/response/9", record.RecordID)
	require.Equal(t, event.ID, record.ID)
	require.Equal(t, event.Timestamp, record.Timestamp)
	require.Equal(t, event.TraceID, record.TraceID)
	require.Empty(t, record.Headers)
	require.Empty(t, record.BodyEncoding)
	require.Empty(t, record.Shape)
	require.Empty(t, record.Instructions)
	require.Empty(t, record.UserMessages)
	require.Empty(t, record.AssistantMessages)
	require.Empty(t, record.Body)

	formatted := formatTrafficEvent(event)
	require.Contains(t, formatted, "id=http/response/9")
	require.NotContains(t, formatted, "system prompt")
	require.NotContains(t, formatted, "hello")
	require.NotContains(t, formatted, "body(")
}

func TestTrafficLogDetailBoundsStructuredFieldsAndBody(t *testing.T) {
	event := trafficTestEvent()
	event.Body = `{"instructions":"` + strings.Repeat("x", maxTrafficBodyOutput+100) + `"}`
	for index := 0; index < maxTrafficHeaders+4; index++ {
		event.Headers[string(rune('A'+index))] = []string{"one", "two", "three"}
	}
	record := trafficLogRecord(event, true)
	require.Equal(t, "http/response/9", record.RecordID)
	require.LessOrEqual(t, len(record.Headers), maxTrafficHeaders)
	for _, header := range record.Headers {
		require.LessOrEqual(t, len(header.Values), maxTrafficHeaderValues)
	}
	require.Contains(t, record.Body, "[truncated")
	require.Less(t, len(record.Body), maxTrafficBodyOutput+100)

	output := formatTrafficLogDetail(record, true)
	require.Contains(t, output, "http/response/9")
	require.Contains(t, output, "body(utf-8)")
	require.LessOrEqual(t, len(output), maxTrafficDetailOutput+64)
}

func TestTrafficBodySearchIsLiteralCaseInsensitiveAndBounded(t *testing.T) {
	body := strings.Repeat("prefix NEEDLE suffix ", maxTrafficSearchMatches+10)
	matches := findTrafficBodyMatches(body, "needle")
	require.Len(t, matches, maxTrafficSearchMatches)
	for _, match := range matches {
		require.Contains(t, strings.ToLower(match.Snippet), "[[needle]]")
		require.LessOrEqual(t, len(match.Snippet), trafficSearchContextBytes*2+maxTrafficSearchQuery+32)
	}

	literal := findTrafficBodyMatches("before a.b after axb", "a.b")
	require.Len(t, literal, 1)
	require.Contains(t, literal[0].Snippet, "[[a.b]]")
}

func TestTrafficLogDescriptionsKeepBodiesOutOfList(t *testing.T) {
	require.Contains(t, trafficLogsDescription, "Never returns headers, body summaries, or raw bodies")
	require.Contains(t, trafficLogSearchDescription, "never returns the complete body")
	require.Contains(t, trafficLogDetailDescription, "body prefix")
}

func trafficTestEvent() cruxlog.TrafficEvent {
	return cruxlog.TrafficEvent{
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
		Headers:       map[string][]string{"Content-Type": {"application/json"}},
		BodyEncoding:  "utf-8",
		ContentLength: 2048,
		DurationMS:    37,
		Error:         "spaced error",
		Body:          `{"instructions":"system prompt","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"answer"}]}`,
	}
}
