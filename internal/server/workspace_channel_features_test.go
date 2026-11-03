package server

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func messageEvent(t *testing.T, eventType pubsub.EventType, id string, parts ...map[string]any) []byte {
	t.Helper()
	payload, err := json.Marshal(map[string]any{"id": id, "session_id": "session", "role": "assistant", "model": "m", "provider": "p", "created_at": 1, "updated_at": len(parts), "parts": parts})
	require.NoError(t, err)
	data, err := json.Marshal(pubsub.Payload{Type: pubsub.PayloadTypeMessage, Payload: mustJSON(pubsub.Event[json.RawMessage]{Type: eventType, Payload: payload})})
	require.NoError(t, err)
	return data
}

func text(value string) map[string]any {
	return map[string]any{"type": "text", "data": map[string]any{"text": value}}
}

func decodeChannelPayload(t *testing.T, data []byte) (string, map[string]any) {
	t.Helper()
	var envelope pubsub.Payload
	require.NoError(t, json.Unmarshal(data, &envelope))
	var event struct {
		Payload map[string]any `json:"payload"`
	}
	require.NoError(t, json.Unmarshal(envelope.Payload, &event))
	return envelope.Type, event.Payload
}

func TestWorkspaceChannelFeaturesRejectUnknownNames(t *testing.T) {
	features, err := parseWorkspaceChannelFeatures("message_delta, session_busy")
	require.NoError(t, err)
	require.True(t, features.messageDelta)
	require.True(t, features.sessionBusy)
	_, err = parseWorkspaceChannelFeatures("message_delta,unknown")
	require.ErrorContains(t, err, "unknown")
	features, err = parseWorkspaceChannelFeatures("")
	require.NoError(t, err)
	require.Equal(t, workspaceChannelFeatures{}, features)
}

func TestMessageDeltaSendsAppendedTextAfterFullSnapshot(t *testing.T) {
	encoder := newMessageDeltaEncoder()
	long := strings.Repeat("streamed text ", 200)
	kind, first := decodeChannelPayload(t, encoder.encode(messageEvent(t, pubsub.CreatedEvent, "reply", text(long))))
	require.Equal(t, pubsub.PayloadTypeMessage, kind)
	require.EqualValues(t, 1, first["delta_revision"])

	data := encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "reply", text(long+"more")))
	kind, delta := decodeChannelPayload(t, data)
	require.Equal(t, payloadTypeMessageDelta, kind)
	require.EqualValues(t, 1, delta["base"])
	require.EqualValues(t, 2, delta["revision"])
	require.EqualValues(t, 1, delta["length"])
	require.Equal(t, []any{map[string]any{"index": float64(0), "field": "text", "append": "more"}}, delta["parts"])
	require.Less(t, len(data), len(long))
}

func TestMessageDeltaReplacesChangedPartsAndAddsNewOnes(t *testing.T) {
	encoder := newMessageDeltaEncoder()
	long := strings.Repeat("a", 2000)
	call := map[string]any{"type": "tool_call", "data": map[string]any{"id": "c", "name": "bash", "input": "{}", "finished": false}}
	encoder.encode(messageEvent(t, pubsub.CreatedEvent, "reply", text(long), call))
	call["data"].(map[string]any)["finished"] = true
	_, delta := decodeChannelPayload(t, encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "reply", text(long), call, text("tail"))))
	parts := delta["parts"].([]any)
	require.Len(t, parts, 2)
	require.EqualValues(t, 1, parts[0].(map[string]any)["index"])
	require.Equal(t, true, parts[0].(map[string]any)["part"].(map[string]any)["data"].(map[string]any)["finished"])
	require.EqualValues(t, 2, parts[1].(map[string]any)["index"])
}

func TestMessageDeltaFallsBackToFullSnapshots(t *testing.T) {
	encoder := newMessageDeltaEncoder()
	long := strings.Repeat("b", 2000)
	encoder.encode(messageEvent(t, pubsub.CreatedEvent, "reply", text(long), text(long)))
	kind, payload := decodeChannelPayload(t, encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "reply", text(long))))
	require.Equal(t, pubsub.PayloadTypeMessage, kind, "removed parts require a full snapshot")
	require.EqualValues(t, 2, payload["delta_revision"])

	kind, _ = decodeChannelPayload(t, encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "unknown", text(long))))
	require.Equal(t, pubsub.PayloadTypeMessage, kind, "untracked messages are sent in full")

	finished := map[string]any{"type": "finish", "data": map[string]any{"reason": "end_turn", "time": 1}}
	encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "reply", text(long), finished))
	kind, _ = decodeChannelPayload(t, encoder.encode(messageEvent(t, pubsub.UpdatedEvent, "reply", text(long+"x"), finished)))
	require.Equal(t, pubsub.PayloadTypeMessage, kind, "finished messages are released")

	other := []byte(`{"type":"session","payload":{"type":"updated","payload":{"id":"s"}}}`)
	require.Equal(t, other, encoder.encode(other))
}

func TestMessageDeltaTrackingIsBounded(t *testing.T) {
	encoder := newMessageDeltaEncoder()
	for index := range workspaceChannelDeltaTrackLimit + 10 {
		encoder.encode(messageEvent(t, pubsub.CreatedEvent, strings.Repeat("x", 4)+string(rune('a'+index%26))+strings.Repeat("y", index), text("t")))
	}
	require.LessOrEqual(t, len(encoder.messages), workspaceChannelDeltaTrackLimit)
}

func TestSessionIDFromEvent(t *testing.T) {
	require.Equal(t, "session", sessionIDFromEvent(messageEvent(t, pubsub.CreatedEvent, "m", text("x"))))
	require.Equal(t, "s", sessionIDFromEvent([]byte(`{"type":"session","payload":{"type":"updated","payload":{"id":"s"}}}`)))
	require.Equal(t, "", sessionIDFromEvent([]byte(`{"type":"lsp_event","payload":{"type":"updated","payload":{"name":"go"}}}`)))
}
