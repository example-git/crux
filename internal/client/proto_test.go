package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/message"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
)

func TestSendEventAfterContextCancelIsIdempotent(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := make(chan any, 1)
	require.False(t, sendEvent(ctx, events, "one"))
	require.False(t, sendEvent(ctx, events, "two"))

	select {
	case ev := <-events:
		require.Failf(t, "unexpected event", "event: %v", ev)
	default:
	}
}

func TestSubscribeEventsContextCancelClosesEvents(t *testing.T) {
	t.Parallel()

	payload := marshalSSEPayload(t)
	firstEventSent := make(chan struct{})
	writeSecondEvent := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		require.True(t, ok)

		_, err := fmt.Fprintf(w, "data: %s\n\n", payload)
		require.NoError(t, err)
		flusher.Flush()
		close(firstEventSent)

		select {
		case <-writeSecondEvent:
		case <-time.After(5 * time.Second):
			return
		}
		_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
		flusher.Flush()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c := captureClient(t, srv)
	events, err := c.SubscribeEvents(ctx, "ws1")
	require.NoError(t, err)

	select {
	case <-firstEventSent:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for server event")
	}

	select {
	case <-events:
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for first event")
	}

	cancel()
	close(writeSecondEvent)

	select {
	case _, ok := <-events:
		require.False(t, ok)
	case <-time.After(5 * time.Second):
		require.Fail(t, "timed out waiting for event channel close")
	}
}

func TestSubscribeEventsPreservesOpaqueMessageMetadata(t *testing.T) {
	t.Parallel()

	payloadBytes := []byte(`{ "number" : 1.00e+2, "ordered" : [2,1] }`)
	envelope, err := message.NewProviderMetadataEnvelope("missing.plugin", 17, message.ProviderMetadataScopeContinuation, payloadBytes)
	require.NoError(t, err)
	messageEvent, err := json.Marshal(pubsub.Event[proto.Message]{
		Type: pubsub.UpdatedEvent,
		Payload: proto.Message{Role: proto.Assistant, Parts: []proto.ContentPart{
			proto.ProviderMetadataContent{ProviderMetadata: message.ProviderMetadata{envelope}},
		}},
	})
	require.NoError(t, err)
	ssePayload, err := json.Marshal(pubsub.Payload{Type: pubsub.PayloadTypeMessage, Payload: messageEvent})
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, writeErr := fmt.Fprintf(w, "data: %s\n\n", ssePayload)
		require.NoError(t, writeErr)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	events, err := c.SubscribeEvents(t.Context(), "ws1")
	require.NoError(t, err)
	event, ok := <-events
	require.True(t, ok)
	decoded, ok := event.(pubsub.Event[proto.Message])
	require.True(t, ok, "unexpected SSE event %T", event)
	metadata := decoded.Payload.Parts[0].(proto.ProviderMetadataContent).ProviderMetadata
	require.Len(t, metadata, 1)
	require.Equal(t, message.ProviderMetadataScopeContinuation, metadata[0].Scope)
	require.Equal(t, payloadBytes, metadata[0].Payload)
}

func TestCreateAgentDefinitionSendsConfigurationAndReturnsPath(t *testing.T) {
	t.Parallel()

	defaultFormat := "json"
	request := proto.CreateAgentDefinitionRequest{
		Scope:       "project",
		Name:        "reviewer",
		Description: "Reviews changes",
		Model:       "provider/model",
		Tools:       []string{"script"},
		Script: &proto.AgentDefinitionScript{
			Path:    "./scripts/review.py",
			Timeout: "30s",
			Variables: map[string]proto.AgentDefinitionScriptVariable{
				"input":  {Required: true},
				"format": {Default: &defaultFormat, Values: []string{"json", "text"}},
			},
		},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1/workspaces/ws1/agent/definitions", r.URL.Path)
		var received proto.CreateAgentDefinitionRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		require.Equal(t, request, received)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		require.NoError(t, json.NewEncoder(w).Encode(proto.CreateAgentDefinitionResponse{Path: "/project/.ai-cli/agents/reviewer.md"}))
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	path, err := c.CreateAgentDefinition(context.Background(), "ws1", request)
	require.NoError(t, err)
	require.Equal(t, "/project/.ai-cli/agents/reviewer.md", path)
}

func TestCreateAgentDefinitionReturnsServerValidationError(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "invalid agent definition: invalid scope"})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	_, err := c.CreateAgentDefinition(context.Background(), "ws1", proto.CreateAgentDefinitionRequest{})
	require.ErrorContains(t, err, "invalid scope")
}

func TestSendMessageAcceptsStatusAccepted(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SendMessage(context.Background(), "ws1", "sess1", "", "hello"))
}

func TestSendMessagePropagatesSubmissionID(t *testing.T) {
	t.Parallel()

	var received proto.AgentMessage
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&received))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	ctx := agent.WithSubmissionID(context.Background(), "submission-id")
	require.NoError(t, c.SendMessage(ctx, "ws1", "sess1", "", "hello"))
	require.Equal(t, "submission-id", received.SubmissionID)
}

func TestSendMessageAcceptsStatusOK(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	require.NoError(t, c.SendMessage(context.Background(), "ws1", "sess1", "", "hello"))
}

func TestSendMessageDecodesErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(proto.Error{Message: "session id is required"})
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 400")
	require.Contains(t, err.Error(), "session id is required")
}

func TestSendMessageFallsBackOnMalformedErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "sess1", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 500")
	require.NotContains(t, err.Error(), "not json")
}

func TestSendMessageFallsBackOnEmptyErrorBody(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := captureClient(t, srv)
	err := c.SendMessage(context.Background(), "ws1", "sess1", "", "hello")
	require.Error(t, err)
	require.Contains(t, err.Error(), "status code 500")
}

func marshalSSEPayload(t *testing.T) []byte {
	t.Helper()

	eventPayload, err := json.Marshal(pubsub.Event[proto.AgentEvent]{
		Type: pubsub.CreatedEvent,
		Payload: proto.AgentEvent{
			Type: proto.AgentEventTypeResponse,
		},
	})
	require.NoError(t, err)

	payload, err := json.Marshal(pubsub.Payload{
		Type:    pubsub.PayloadTypeAgentEvent,
		Payload: eventPayload,
	})
	require.NoError(t, err)
	return payload
}
