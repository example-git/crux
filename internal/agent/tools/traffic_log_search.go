package tools

import (
	"context"
	_ "embed"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

const (
	TrafficLogSearchToolName  = "traffic_log_search"
	maxTrafficSearchQuery     = 256
	maxTrafficSearchMatches   = 8
	trafficSearchContextBytes = 240
	maxTrafficSearchOutput    = 8 * 1024
)

//go:embed traffic_log_search.md
var trafficLogSearchDescription string

type TrafficLogSearchParams struct {
	RecordID string `json:"record_id" description:"Composite record ID from traffic_logs, for example http/request/17"`
	Query    string `json:"query" description:"Case-insensitive literal text to find in the selected record body"`
}

type TrafficLogSearchResponseMetadata struct {
	RecordID string                  `json:"record_id"`
	Query    string                  `json:"query"`
	Matches  []TrafficLogSearchMatch `json:"matches,omitempty"`
}

type TrafficLogSearchMatch struct {
	Offset  int    `json:"offset"`
	Snippet string `json:"snippet"`
}

func NewTrafficLogSearchTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		TrafficLogSearchToolName,
		trafficLogSearchDescription,
		func(ctx context.Context, params TrafficLogSearchParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, metadata, err := runTrafficLogSearch(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}

func runTrafficLogSearch(ctx context.Context, params TrafficLogSearchParams) (string, TrafficLogSearchResponseMetadata, error) {
	if strings.TrimSpace(params.Query) == "" {
		return "", TrafficLogSearchResponseMetadata{}, fmt.Errorf("query must not be empty")
	}
	if len(params.Query) > maxTrafficSearchQuery {
		return "", TrafficLogSearchResponseMetadata{}, fmt.Errorf("query must not exceed %d bytes", maxTrafficSearchQuery)
	}
	event, err := loadTrafficRecord(ctx, params.RecordID, 0)
	if err != nil {
		return "", TrafficLogSearchResponseMetadata{}, err
	}
	body, err := searchableTrafficBody(event.Body, event.BodyEncoding)
	if err != nil {
		return "", TrafficLogSearchResponseMetadata{}, err
	}
	matches := findTrafficBodyMatches(body, params.Query)
	metadata := TrafficLogSearchResponseMetadata{RecordID: params.RecordID, Query: params.Query, Matches: matches}
	if len(matches) == 0 {
		return fmt.Sprintf("No body matches for %q in %s", compactTrafficText(params.Query), params.RecordID), metadata, nil
	}
	var output strings.Builder
	fmt.Fprintf(&output, "%d body matches for %q in %s", len(matches), compactTrafficText(params.Query), params.RecordID)
	for index, match := range matches {
		fmt.Fprintf(&output, "\nmatch %d offset=%d: %s", index+1, match.Offset, match.Snippet)
	}
	return boundTrafficOutput(output.String(), maxTrafficSearchOutput), metadata, nil
}

func searchableTrafficBody(body, encoding string) (string, error) {
	if strings.HasPrefix(encoding, "base64") {
		decoded, err := base64.StdEncoding.DecodeString(body)
		if err != nil {
			return "", fmt.Errorf("decode traffic body: %w", err)
		}
		body = string(decoded)
	}
	return strings.ToValidUTF8(body, "�"), nil
}

func findTrafficBodyMatches(body, query string) []TrafficLogSearchMatch {
	pattern := regexp.MustCompile("(?i)" + regexp.QuoteMeta(query))
	indices := pattern.FindAllStringIndex(body, maxTrafficSearchMatches)
	matches := make([]TrafficLogSearchMatch, 0, len(indices))
	for _, index := range indices {
		start := max(0, index[0]-trafficSearchContextBytes)
		end := min(len(body), index[1]+trafficSearchContextBytes)
		snippet := body[start:index[0]] + "[[" + body[index[0]:index[1]] + "]]" + body[index[1]:end]
		snippet = compactTrafficTextLimit(snippet, trafficSearchContextBytes*2+maxTrafficSearchQuery+4)
		if start > 0 {
			snippet = "..." + snippet
		}
		if end < len(body) {
			snippet += "..."
		}
		matches = append(matches, TrafficLogSearchMatch{Offset: index[0], Snippet: snippet})
	}
	return matches
}
