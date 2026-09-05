package sse

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseStopsAtDoneMarker(t *testing.T) {
	input := "data: {\"value\":1}\n\ndata: [DONE]\n\ndata: not-json\n\n"
	var events []json.RawMessage
	err := Parse(context.Background(), bytes.NewBufferString(input), Options{RequireTerminal: true}, func(raw json.RawMessage) error {
		events = append(events, raw)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
}

func TestParseExecutesStandardJSONFraming(t *testing.T) {
	input := ": keepalive\r\nevent: message\r\nid: ignored\r\ndata: {\"value\":\r\ndata: 1}\r\n\r\ndata: {\"final\":true}"
	var events []json.RawMessage
	err := Parse(context.Background(), bytes.NewBufferString(input), Options{}, func(raw json.RawMessage) error {
		events = append(events, bytes.Clone(raw))
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 2)
	require.JSONEq(t, `{"value":1}`, string(events[0]))
	require.JSONEq(t, `{"final":true}`, string(events[1]))
}

func TestParseRejectsMalformedAndUnterminatedStreams(t *testing.T) {
	err := Parse(context.Background(), bytes.NewBufferString("data: not-json\n\n"), Options{}, func(json.RawMessage) error { return nil })
	require.ErrorContains(t, err, "SSE data is not valid JSON")

	err = Parse(context.Background(), bytes.NewBufferString("data: {\"value\":1}\n\n"), Options{RequireTerminal: true}, func(json.RawMessage) error { return nil })
	require.ErrorIs(t, err, ErrMissingTerminal)
}

func TestParseEnforcesAggregateEventSize(t *testing.T) {
	input := "data: {\"value\":\ndata: \"too-long\"}\n\n"
	err := Parse(context.Background(), bytes.NewBufferString(input), Options{MaxEventBytes: 16}, func(json.RawMessage) error { return nil })
	require.ErrorContains(t, err, "SSE event exceeds 16 bytes")
}

func TestParseStopsAtSemanticTerminal(t *testing.T) {
	input := "data: {\"type\":\"delta\"}\n\ndata: {\"type\":\"finish\"}\n\ndata: not-json\n\n"
	var events []json.RawMessage
	err := Parse(context.Background(), bytes.NewBufferString(input), Options{RequireTerminal: true}, func(raw json.RawMessage) error {
		events = append(events, raw)
		var event map[string]any
		require.NoError(t, json.Unmarshal(raw, &event))
		if event["type"] == "finish" {
			return ErrTerminal
		}
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 2)
}
