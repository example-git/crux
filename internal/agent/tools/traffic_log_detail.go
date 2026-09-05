package tools

import (
	"context"
	_ "embed"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

const (
	TrafficLogDetailToolName = "traffic_log_detail"
	trafficDetailBodyLimit   = maxTrafficBodyOutput - 128
)

//go:embed traffic_log_detail.md
var trafficLogDetailDescription string

type TrafficLogDetailParams struct {
	RecordID    string `json:"record_id" description:"Composite record ID from traffic_logs, for example http/request/17"`
	IncludeBody bool   `json:"include_body,omitempty" description:"Include a bounded body prefix; defaults to false"`
}

type TrafficLogDetailResponseMetadata struct {
	Record TrafficLogRecord `json:"record"`
}

func NewTrafficLogDetailTool() fantasy.AgentTool {
	return fantasy.NewParallelAgentTool(
		TrafficLogDetailToolName,
		trafficLogDetailDescription,
		func(ctx context.Context, params TrafficLogDetailParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			result, metadata, err := runTrafficLogDetail(ctx, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			return fantasy.WithResponseMetadata(fantasy.NewTextResponse(result), metadata), nil
		},
	)
}

func runTrafficLogDetail(ctx context.Context, params TrafficLogDetailParams) (string, TrafficLogDetailResponseMetadata, error) {
	event, err := loadTrafficRecord(ctx, params.RecordID, trafficDetailBodyLimit)
	if err != nil {
		return "", TrafficLogDetailResponseMetadata{}, err
	}
	record := trafficLogRecord(event, params.IncludeBody)
	return formatTrafficLogDetail(record, params.IncludeBody), TrafficLogDetailResponseMetadata{Record: record}, nil
}

func formatTrafficLogDetail(record TrafficLogRecord, includeBody bool) string {
	var output strings.Builder
	fmt.Fprintf(&output, "%s %s %s %s", record.RecordID, record.Timestamp.Format("2006-01-02T15:04:05.999999999Z07:00"), record.Direction, record.Protocol)
	if record.Method != "" {
		fmt.Fprintf(&output, " %s", record.Method)
	}
	if record.URL != "" {
		fmt.Fprintf(&output, " %s", record.URL)
	}
	if record.StatusCode != 0 {
		fmt.Fprintf(&output, " status=%d", record.StatusCode)
	}
	fmt.Fprintf(&output, "\npid=%d trace=%s phase=%s bytes=%d duration_ms=%d message_type=%d", record.ProcessID, record.TraceID, record.Phase, record.ContentLength, record.DurationMS, record.MessageType)
	if record.Error != "" {
		fmt.Fprintf(&output, "\nerror: %s", record.Error)
	}
	for _, header := range record.Headers {
		fmt.Fprintf(&output, "\nheader %s: %s", header.Name, strings.Join(header.Values, ", "))
	}
	if record.Shape != "" {
		fmt.Fprintf(&output, "\nshape: %s", record.Shape)
	}
	appendTrafficValues(&output, "instructions", record.Instructions)
	appendTrafficValues(&output, "user", record.UserMessages)
	appendTrafficValues(&output, "assistant", record.AssistantMessages)
	if includeBody {
		if record.Body == "" {
			fmt.Fprint(&output, "\nbody: empty")
		} else {
			fmt.Fprintf(&output, "\nbody(%s): %s", record.BodyEncoding, record.Body)
		}
	}
	return boundTrafficOutput(output.String(), maxTrafficDetailOutput)
}
