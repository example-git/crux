package server

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/example-git/crux/internal/agent/notify"
	"github.com/example-git/crux/internal/agent/tools/mcp"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/skills"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

// TestMessageToProtoToolResult ensures that ToolResult metadata,
// data, and MIME type survive the conversion to proto. Without these
// fields the TUI cannot render rich tool output (e.g. syntax-
// highlighted code from view, diffs from edit, images, etc.) and
// falls back to the raw LLM-facing string.
func TestMessageToProtoToolResult(t *testing.T) {
	t.Parallel()

	payload := []byte("{ \"unknown\": 1.00e+2 }")
	envelope, err := message.NewProviderMetadataEnvelope("missing.plugin", 3, message.ProviderMetadataScopeToolResult, payload)
	require.NoError(t, err)

	src := message.Message{
		ID:   "m1",
		Role: message.Tool,
		Parts: []message.ContentPart{
			message.ToolResult{
				ToolCallID:       "call-1",
				Name:             "view",
				Content:          "<file>\n  1| hi\n</file>",
				Data:             "base64data",
				MIMEType:         "image/png",
				Metadata:         `{"file_path":"/tmp/x","content":"hi"}`,
				IsError:          false,
				ProviderExecuted: true,
				ProviderMetadata: message.ProviderMetadata{envelope},
			},
		},
	}

	got := messageToProto(src)
	require.Len(t, got.Parts, 1)
	tr, ok := got.Parts[0].(proto.ToolResult)
	require.True(t, ok, "expected proto.ToolResult, got %T", got.Parts[0])
	require.Equal(t, "call-1", tr.ToolCallID)
	require.Equal(t, "view", tr.Name)
	require.Equal(t, "<file>\n  1| hi\n</file>", tr.Content)
	require.Equal(t, "base64data", tr.Data)
	require.Equal(t, "image/png", tr.MIMEType)
	require.Equal(t, `{"file_path":"/tmp/x","content":"hi"}`, tr.Metadata)
	require.False(t, tr.IsError)
	require.True(t, tr.ProviderExecuted)
	require.Len(t, tr.ProviderMetadata, 1)
	require.Equal(t, payload, tr.ProviderMetadata[0].Payload)

	encoded, err := json.Marshal(got)
	require.NoError(t, err)
	var decoded proto.Message
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	decodedResult := decoded.Parts[0].(proto.ToolResult)
	require.Equal(t, payload, decodedResult.ProviderMetadata[0].Payload)
}

func TestMessageToProtoPreservesAllProviderMetadataScopes(t *testing.T) {
	t.Parallel()

	makeEnvelope := func(scope message.ProviderMetadataScope) message.ProviderMetadataEnvelope {
		envelope, err := message.NewProviderMetadataEnvelope("missing.plugin", 11, scope, []byte(`{ "number" : 1.00e+2 }`))
		require.NoError(t, err)
		return envelope
	}
	src := message.Message{Role: message.Assistant, Parts: []message.ContentPart{
		message.TextContent{Text: "text", ProviderMetadata: message.ProviderMetadata{makeEnvelope(message.ProviderMetadataScopeText), makeEnvelope(message.ProviderMetadataScopeCompaction)}},
		message.ReasoningContent{Thinking: "reasoning", ProviderMetadata: message.ProviderMetadata{makeEnvelope(message.ProviderMetadataScopeReasoning)}},
		message.ToolCall{ID: "call", Name: "tool", Input: "{}", Finished: true, ProviderExecuted: true, ProviderMetadata: message.ProviderMetadata{makeEnvelope(message.ProviderMetadataScopeToolCall)}},
		message.ToolResult{ToolCallID: "call", Name: "tool", Content: "result", ProviderExecuted: true, ProviderMetadata: message.ProviderMetadata{makeEnvelope(message.ProviderMetadataScopeToolResult)}},
		message.ProviderMetadataContent{ProviderMetadata: message.ProviderMetadata{
			makeEnvelope(message.ProviderMetadataScopeMessage),
			makeEnvelope(message.ProviderMetadataScopeContinuation),
		}},
	}}

	encoded, err := json.Marshal(messageToProto(src))
	require.NoError(t, err)
	var decoded proto.Message
	require.NoError(t, json.Unmarshal(encoded, &decoded))
	require.Len(t, decoded.Parts, 5)
	text := decoded.Parts[0].(proto.TextContent)
	require.Equal(t, message.ProviderMetadataScopeText, text.ProviderMetadata[0].Scope)
	require.Equal(t, message.ProviderMetadataScopeCompaction, text.ProviderMetadata[1].Scope)
	require.Equal(t, message.ProviderMetadataScopeReasoning, decoded.Parts[1].(proto.ReasoningContent).ProviderMetadata[0].Scope)
	toolCall := decoded.Parts[2].(proto.ToolCall)
	require.True(t, toolCall.ProviderExecuted)
	require.Equal(t, message.ProviderMetadataScopeToolCall, toolCall.ProviderMetadata[0].Scope)
	toolResult := decoded.Parts[3].(proto.ToolResult)
	require.True(t, toolResult.ProviderExecuted)
	require.Equal(t, message.ProviderMetadataScopeToolResult, toolResult.ProviderMetadata[0].Scope)
	metadata := decoded.Parts[4].(proto.ProviderMetadataContent).ProviderMetadata
	require.Equal(t, message.ProviderMetadataScopeMessage, metadata[0].Scope)
	require.Equal(t, message.ProviderMetadataScopeContinuation, metadata[1].Scope)
	for _, part := range decoded.Parts {
		switch value := part.(type) {
		case proto.TextContent:
			require.Equal(t, []byte(`{ "number" : 1.00e+2 }`), value.ProviderMetadata[0].Payload)
		case proto.ReasoningContent:
			require.Equal(t, []byte(`{ "number" : 1.00e+2 }`), value.ProviderMetadata[0].Payload)
		case proto.ToolCall:
			require.Equal(t, []byte(`{ "number" : 1.00e+2 }`), value.ProviderMetadata[0].Payload)
		case proto.ToolResult:
			require.Equal(t, []byte(`{ "number" : 1.00e+2 }`), value.ProviderMetadata[0].Payload)
		case proto.ProviderMetadataContent:
			require.Equal(t, []byte(`{ "number" : 1.00e+2 }`), value.ProviderMetadata[0].Payload)
		}
	}
}

// TestSkillsEventToProto_RoundTrip verifies that a pubsub.Event[skills.Event]
// can be wrapped, marshaled, and unmarshaled back through the SSE
// envelope without losing state values or error messages.
func TestSkillsEventToProto_RoundTrip(t *testing.T) {
	t.Parallel()

	src := pubsub.Event[skills.Event]{
		Type: pubsub.UpdatedEvent,
		Payload: skills.Event{
			States: []*skills.SkillState{
				{Name: "ok", Path: "/p/ok", State: skills.StateNormal},
				{Name: "broken", Path: "/p/broken", State: skills.StateError, Err: errors.New("bad frontmatter")},
			},
		},
	}

	env := wrapEvent(src)
	require.NotNil(t, env)
	require.Equal(t, pubsub.PayloadTypeSkillsEvent, env.Type)

	var decoded pubsub.Event[proto.SkillsEvent]
	require.NoError(t, json.Unmarshal(env.Payload, &decoded))
	require.Equal(t, pubsub.UpdatedEvent, decoded.Type)
	require.Len(t, decoded.Payload.States, 2)

	require.Equal(t, "ok", decoded.Payload.States[0].Name)
	require.Equal(t, "/p/ok", decoded.Payload.States[0].Path)
	require.Equal(t, proto.SkillStateNormal, decoded.Payload.States[0].State)
	require.Empty(t, decoded.Payload.States[0].Error)

	require.Equal(t, "broken", decoded.Payload.States[1].Name)
	require.Equal(t, proto.SkillStateError, decoded.Payload.States[1].State)
	require.Equal(t, "bad frontmatter", decoded.Payload.States[1].Error)
}

// TestRunCompleteToProto_RoundTrip verifies that the authoritative
// per-run completion event survives the SSE envelope conversion with
// all reconciliation fields intact. SessionID, MessageID, and Text
// are what non-interactive clients (e.g. `crux run`) rely on to
// terminate the run loop and guarantee final text on stdout when
// message events arrive out of order.
func TestTaskNotificationToProtoRoundTrip(t *testing.T) {
	source := pubsub.Event[managedtask.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: managedtask.Notification{
			ID:              "notification",
			TaskID:          "a12345678",
			TaskType:        managedtask.TypeAgent,
			ToolUseID:       "tool-call",
			WorkspaceID:     "workspace",
			ParentSessionID: "parent",
			Status:          managedtask.StatusCompleted,
			Summary:         "Agent completed",
			OutputRef:       "session:child",
			FinalOutput:     "finished",
		},
	}

	envelope := wrapEvent(source)
	require.NotNil(t, envelope)
	require.Equal(t, pubsub.PayloadTypeTaskNotification, envelope.Type)
	var decoded pubsub.Event[proto.TaskNotification]
	require.NoError(t, json.Unmarshal(envelope.Payload, &decoded))
	require.Equal(t, source.Payload, managedtask.Notification(decoded.Payload))
}

func TestRunCompleteToProto_RoundTrip(t *testing.T) {
	t.Parallel()

	src := pubsub.Event[notify.RunComplete]{
		Type: pubsub.UpdatedEvent,
		Payload: notify.RunComplete{
			SessionID: "S",
			RunID:     "run-42",
			MessageID: "M",
			Text:      "VERDICT: APPROVED",
			Error:     "",
			Cancelled: false,
		},
	}

	env := wrapEvent(src)
	require.NotNil(t, env)
	require.Equal(t, pubsub.PayloadTypeRunComplete, env.Type)

	var decoded pubsub.Event[proto.RunComplete]
	require.NoError(t, json.Unmarshal(env.Payload, &decoded))
	require.Equal(t, pubsub.UpdatedEvent, decoded.Type)
	require.Equal(t, "S", decoded.Payload.SessionID)
	require.Equal(t, "run-42", decoded.Payload.RunID,
		"RunID must survive the SSE envelope so clients can correlate "+
			"this event with the SendMessage call that produced it")
	require.Equal(t, "M", decoded.Payload.MessageID)
	require.Equal(t, "VERDICT: APPROVED", decoded.Payload.Text)
	require.Empty(t, decoded.Payload.Error)
	require.False(t, decoded.Payload.Cancelled)
}

// TestAgentErrorToProto_PreservesRunID verifies that an async agent
// error notification carries its originating RunID (and SessionID)
// through the SSE envelope. Without these correlators, `crux run`
// cannot tell whether an error event belongs to its own run and
// would abort on any unrelated workspace failure.
func TestAgentErrorToProto_PreservesRunID(t *testing.T) {
	t.Parallel()

	src := pubsub.Event[notify.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: notify.Notification{
			SessionID:  "S",
			RunID:      "run-99",
			Type:       notify.TypeAgentError,
			Message:    "boom",
			ProviderID: "provider",
		},
	}

	env := wrapEvent(src)
	require.NotNil(t, env)
	require.Equal(t, pubsub.PayloadTypeAgentEvent, env.Type)

	var decoded pubsub.Event[proto.AgentEvent]
	require.NoError(t, json.Unmarshal(env.Payload, &decoded))
	require.Equal(t, proto.AgentEventTypeError, decoded.Payload.Type)
	require.Equal(t, "S", decoded.Payload.SessionID)
	require.Equal(t, "run-99", decoded.Payload.RunID,
		"RunID must survive so observers can attribute the error to its run")
	require.Equal(t, "provider", decoded.Payload.ProviderID)
	require.Nil(t, decoded.Payload.Owner)
	require.NotContains(t, string(env.Payload), `"owner"`)
	require.NotNil(t, decoded.Payload.Error)
	require.Equal(t, "boom", decoded.Payload.Error.Error())
}

func TestReAuthenticateToProtoPreservesExactOwner(t *testing.T) {
	t.Parallel()

	owner := providerregistry.RegistrationOwner{
		ProviderID:      "provider",
		Construction:    providerregistry.ConstructionGenericJSON,
		HasManifest:     true,
		ManifestID:      "plugin.provider",
		ManifestVersion: "2.0.0",
	}
	env := wrapEvent(pubsub.Event[notify.Notification]{
		Type: pubsub.CreatedEvent,
		Payload: notify.Notification{
			Type:       notify.TypeReAuthenticate,
			ProviderID: owner.ProviderID,
			Owner:      owner,
		},
	})
	require.NotNil(t, env)
	require.Equal(t, pubsub.PayloadTypeAgentEvent, env.Type)

	var decoded pubsub.Event[proto.AgentEvent]
	require.NoError(t, json.Unmarshal(env.Payload, &decoded))
	require.Equal(t, proto.AgentEventType(notify.TypeReAuthenticate), decoded.Payload.Type)
	require.Equal(t, owner.ProviderID, decoded.Payload.ProviderID)
	require.NotNil(t, decoded.Payload.Owner)
	require.Equal(t, owner, *decoded.Payload.Owner)
}

// TestRunCompleteToProto_Error verifies that error- and cancel-shaped
// RunComplete events round-trip cleanly so clients can distinguish
// "agent failed" (returns non-zero from `crux run`) from "agent
// cancelled by user" (clean exit).
func TestRunCompleteToProto_Error(t *testing.T) {
	t.Parallel()

	src := pubsub.Event[notify.RunComplete]{
		Type: pubsub.UpdatedEvent,
		Payload: notify.RunComplete{
			SessionID: "S",
			MessageID: "M",
			Text:      "partial",
			Error:     "context canceled",
			Cancelled: true,
		},
	}

	env := wrapEvent(src)
	require.NotNil(t, env)
	var decoded pubsub.Event[proto.RunComplete]
	require.NoError(t, json.Unmarshal(env.Payload, &decoded))
	require.Equal(t, "context canceled", decoded.Payload.Error)
	require.True(t, decoded.Payload.Cancelled)
}

// TestMCPChannelMessageNotWrappedAsStateChange verifies that an
// EventChannelMessage — which has no proto representation until session
// delivery is wired up in a later PR — is NOT wrapped as a spurious
// state_changed MCP event by the SSE event pipeline. Before the fix,
// mcpEventTypeToProto's default branch mapped every unknown event type to
// MCPEventStateChanged, so a channel notification looked like a state
// change to every SSE client.
func TestMCPChannelMessageNotWrappedAsStateChange(t *testing.T) {
	t.Parallel()

	src := pubsub.Event[mcp.Event]{
		Type: pubsub.CreatedEvent,
		Payload: mcp.Event{
			Type:           mcp.EventChannelMessage,
			Name:           "webhook",
			ChannelMessage: `<channel source="webhook">build failed</channel>`,
		},
	}

	env := wrapEvent(src)
	require.Nil(t, env, "EventChannelMessage must not be wrapped as an SSE event (no proto representation yet)")
}

// TestMCPUnknownEventTypeNotMappedToStateChange verifies that any
// unrecognized MCP event type is not silently coerced to state_changed —
// the mapping must return ok=false so wrapEvent can drop it instead of
// fabricating a state change.
func TestMCPUnknownEventTypeNotMappedToStateChange(t *testing.T) {
	t.Parallel()

	// Use a value well outside the known range.
	unknown := mcp.EventType(99)
	pt := mcpEventTypeToProto(unknown)
	require.Equal(t, proto.MCPEventType(""), pt,
		"unknown MCP event types must map to empty proto type, not state_changed")
}
