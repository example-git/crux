// Package openairesponses implements provider-neutral transport primitives for
// the documented OpenAI Responses protocol. Consumer endpoints, credentials,
// identity headers, private envelopes, and provider-specific retry policy live
// outside this package.
package openairesponses

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/tidwall/sjson"
)

const CreateEventType = "response.create"

// Event is one bounded WebSocket JSON event. Raw retains the complete event for
// protocol-specific normalization without imposing a consumer-specific schema.
type Event struct {
	Type     string
	StreamID string
	Raw      json.RawMessage
}

// DecodeEvent validates the common public event envelope while preserving its
// complete JSON representation.
func DecodeEvent(data []byte) (Event, error) {
	if !json.Valid(data) {
		return Event{}, fmt.Errorf("decode Responses event: invalid JSON")
	}
	var envelope struct {
		Type     string `json:"type"`
		StreamID string `json:"stream_id"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Event{}, fmt.Errorf("decode Responses event: %w", err)
	}
	if envelope.Type == "" {
		return Event{}, fmt.Errorf("Responses event is missing type")
	}
	return Event{Type: envelope.Type, StreamID: envelope.StreamID, Raw: bytes.Clone(data)}, nil
}

// CreateFrame adds the documented response.create type and stream identifier to
// a JSON object containing ordinary Responses request fields.
func CreateFrame(streamID string, request json.RawMessage) ([]byte, error) {
	if streamID == "" {
		return nil, fmt.Errorf("Responses WebSocket stream ID is required")
	}
	if !json.Valid(request) {
		return nil, fmt.Errorf("Responses request is not valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(request, &object); err != nil || object == nil {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	frame, err := sjson.SetBytes(request, "type", CreateEventType)
	if err != nil {
		return nil, fmt.Errorf("set Responses frame type: %w", err)
	}
	frame, err = sjson.SetBytes(frame, "stream_id", streamID)
	if err != nil {
		return nil, fmt.Errorf("set Responses stream ID: %w", err)
	}
	return frame, nil
}

func terminalEvent(eventType string) bool {
	switch eventType {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}
