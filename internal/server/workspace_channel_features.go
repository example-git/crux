package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/pubsub"
)

const (
	workspaceChannelFeatureMessageDelta = "message_delta"
	workspaceChannelFeatureSessionBusy  = "session_busy"

	payloadTypeMessageDelta = "message_delta"
	payloadTypeSessionBusy  = "session_busy"

	workspaceChannelBusyInterval    = 500 * time.Millisecond
	workspaceChannelBusyTrackLimit  = 256
	workspaceChannelDeltaTrackLimit = 512
)

type workspaceChannelFeatures struct {
	messageDelta bool
	sessionBusy  bool
}

func parseWorkspaceChannelFeatures(value string) (workspaceChannelFeatures, error) {
	var features workspaceChannelFeatures
	if value == "" {
		return features, nil
	}
	for name := range strings.SplitSeq(value, ",") {
		switch strings.TrimSpace(name) {
		case workspaceChannelFeatureMessageDelta:
			features.messageDelta = true
		case workspaceChannelFeatureSessionBusy:
			features.sessionBusy = true
		default:
			return features, errors.New("unsupported workspace channel feature: " + name)
		}
	}
	return features, nil
}

type messageDeltaPart struct {
	Index  int             `json:"index"`
	Part   json.RawMessage `json:"part,omitempty"`
	Field  string          `json:"field,omitempty"`
	Append string          `json:"append,omitempty"`
}

type messageDelta struct {
	ID        string             `json:"id"`
	SessionID string             `json:"session_id"`
	UpdatedAt int64              `json:"updated_at"`
	Base      int                `json:"base"`
	Revision  int                `json:"revision"`
	Length    int                `json:"length"`
	Parts     []messageDeltaPart `json:"parts"`
}

type trackedMessage struct {
	revision int
	parts    []json.RawMessage
	header   json.RawMessage
	used     uint64
}

type messageDeltaEncoder struct {
	messages map[string]*trackedMessage
	clock    uint64
}

func newMessageDeltaEncoder() *messageDeltaEncoder {
	return &messageDeltaEncoder{messages: map[string]*trackedMessage{}}
}

type messageSnapshot struct {
	ID        string            `json:"id"`
	SessionID string            `json:"session_id"`
	UpdatedAt int64             `json:"updated_at"`
	Parts     []json.RawMessage `json:"parts"`
}

func messageHeader(raw map[string]json.RawMessage) (json.RawMessage, error) {
	header := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		if key != "parts" && key != "updated_at" {
			header[key] = value
		}
	}
	return json.Marshal(header)
}

func (e *messageDeltaEncoder) encode(data []byte) []byte {
	var envelope pubsub.Payload
	if json.Unmarshal(data, &envelope) != nil || envelope.Type != pubsub.PayloadTypeMessage {
		return data
	}
	var event struct {
		Type    pubsub.EventType `json:"type"`
		Payload json.RawMessage  `json:"payload"`
	}
	if json.Unmarshal(envelope.Payload, &event) != nil {
		return data
	}
	var snapshot messageSnapshot
	var raw map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &snapshot) != nil || json.Unmarshal(event.Payload, &raw) != nil || snapshot.ID == "" {
		return data
	}
	key := snapshot.SessionID + "\x00" + snapshot.ID
	if event.Type == pubsub.DeletedEvent {
		delete(e.messages, key)
		return data
	}
	header, err := messageHeader(raw)
	if err != nil {
		return data
	}
	e.clock++
	previous := e.messages[key]
	if previous == nil || event.Type != pubsub.UpdatedEvent || !bytes.Equal(previous.header, header) || len(snapshot.Parts) < len(previous.parts) {
		return e.full(key, header, snapshot, raw, event.Type, data)
	}
	delta := messageDelta{ID: snapshot.ID, SessionID: snapshot.SessionID, UpdatedAt: snapshot.UpdatedAt, Base: previous.revision, Revision: previous.revision + 1, Length: len(snapshot.Parts)}
	for index, part := range snapshot.Parts {
		if index < len(previous.parts) && bytes.Equal(previous.parts[index], part) {
			continue
		}
		change := messageDeltaPart{Index: index, Part: part}
		if index < len(previous.parts) {
			if field, appended, ok := appendedField(previous.parts[index], part); ok {
				change = messageDeltaPart{Index: index, Field: field, Append: appended}
			}
		}
		delta.Parts = append(delta.Parts, change)
	}
	encoded, err := json.Marshal(pubsub.Payload{Type: payloadTypeMessageDelta, Payload: mustJSON(pubsub.Event[messageDelta]{Type: pubsub.UpdatedEvent, Payload: delta})})
	if err != nil || len(encoded) >= len(data) {
		return e.full(key, header, snapshot, raw, event.Type, data)
	}
	previous.revision = delta.Revision
	previous.parts = snapshot.Parts
	previous.used = e.clock
	e.release(key, snapshot.Parts)
	return encoded
}

func (e *messageDeltaEncoder) release(key string, parts []json.RawMessage) {
	for _, part := range parts {
		var kind struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(part, &kind) == nil && kind.Type == "finish" {
			delete(e.messages, key)
			return
		}
	}
}

func (e *messageDeltaEncoder) full(key string, header json.RawMessage, snapshot messageSnapshot, raw map[string]json.RawMessage, eventType pubsub.EventType, original []byte) []byte {
	tracked := e.messages[key]
	if tracked == nil {
		if len(e.messages) >= workspaceChannelDeltaTrackLimit {
			e.evict()
		}
		tracked = &trackedMessage{}
		e.messages[key] = tracked
	}
	tracked.revision++
	tracked.header = header
	tracked.parts = snapshot.Parts
	tracked.used = e.clock
	raw["delta_revision"] = mustJSON(tracked.revision)
	payload, err := json.Marshal(raw)
	if err != nil {
		delete(e.messages, key)
		return original
	}
	encoded, err := json.Marshal(pubsub.Payload{Type: pubsub.PayloadTypeMessage, Payload: mustJSON(pubsub.Event[json.RawMessage]{Type: eventType, Payload: payload})})
	if err != nil {
		delete(e.messages, key)
		return original
	}
	e.release(key, snapshot.Parts)
	return encoded
}

func (e *messageDeltaEncoder) evict() {
	var oldest string
	var used uint64
	for key, tracked := range e.messages {
		if oldest == "" || tracked.used < used {
			oldest, used = key, tracked.used
		}
	}
	delete(e.messages, oldest)
}

func appendedField(before, after json.RawMessage) (string, string, bool) {
	var left, right struct {
		Type string                     `json:"type"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if json.Unmarshal(before, &left) != nil || json.Unmarshal(after, &right) != nil || left.Type != right.Type || len(left.Data) != len(right.Data) {
		return "", "", false
	}
	field := ""
	appended := ""
	for key, value := range right.Data {
		old, ok := left.Data[key]
		if !ok {
			return "", "", false
		}
		if bytes.Equal(old, value) {
			continue
		}
		var oldText, newText string
		if field != "" || json.Unmarshal(old, &oldText) != nil || json.Unmarshal(value, &newText) != nil || !strings.HasPrefix(newText, oldText) || len(newText) == len(oldText) {
			return "", "", false
		}
		field, appended = key, newText[len(oldText):]
	}
	return field, appended, field != ""
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

type sessionBusy struct {
	SessionID string `json:"session_id"`
	Busy      bool   `json:"busy"`
}

type sessionBusyTracker struct {
	known map[string]bool
	order []string
}

func newSessionBusyTracker() *sessionBusyTracker {
	return &sessionBusyTracker{known: map[string]bool{}}
}

func (t *sessionBusyTracker) observe(sessionID string) {
	if sessionID == "" {
		return
	}
	if _, ok := t.known[sessionID]; ok {
		return
	}
	if len(t.order) >= workspaceChannelBusyTrackLimit {
		delete(t.known, t.order[0])
		t.order = t.order[1:]
	}
	t.known[sessionID] = false
	t.order = append(t.order, sessionID)
}

func (t *sessionBusyTracker) changes(ws *backend.Workspace) []sessionBusy {
	var changed []sessionBusy
	for _, id := range t.order {
		busy := isSessionBusy(ws, id)
		if busy != t.known[id] {
			t.known[id] = busy
			changed = append(changed, sessionBusy{SessionID: id, Busy: busy})
		}
	}
	return changed
}

func sessionIDFromEvent(data []byte) string {
	var envelope pubsub.Payload
	if json.Unmarshal(data, &envelope) != nil {
		return ""
	}
	var event struct {
		Payload struct {
			ID        string `json:"id"`
			SessionID string `json:"session_id"`
		} `json:"payload"`
	}
	if json.Unmarshal(envelope.Payload, &event) != nil {
		return ""
	}
	switch envelope.Type {
	case pubsub.PayloadTypeSession:
		return event.Payload.ID
	case pubsub.PayloadTypeMessage, pubsub.PayloadTypeRunComplete, pubsub.PayloadTypeAgentEvent:
		return event.Payload.SessionID
	}
	return ""
}
