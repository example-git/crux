package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGenerateContentWirePreservesFunctionsAndThoughtSignatures(t *testing.T) {
	request := GenerateContentRequest{Contents: []Content{
		{Role: "model", Parts: []Part{
			{Thought: true, ThoughtSignature: "opaque-signature"},
			{FunctionCall: &FunctionCall{ID: "call-1", Name: "weather", Args: json.RawMessage(`{"city":"Paris"}`)}},
		}},
		{Role: "user", Parts: []Part{{FunctionResponse: &FunctionResponse{ID: "call-1", Name: "weather", Response: json.RawMessage(`{"temperature":21}`)}}}},
	}}
	data, err := json.Marshal(request)
	require.NoError(t, err)
	require.Contains(t, string(data), `"thoughtSignature":"opaque-signature"`)
	require.Contains(t, string(data), `"functionCall":{"id":"call-1","name":"weather","args":{"city":"Paris"}}`)
	require.Contains(t, string(data), `"functionResponse":{"id":"call-1","name":"weather","response":{"temperature":21}}`)
}

func TestParseSSEHandlesStandardMultilineDataAndTerminal(t *testing.T) {
	input := "event: message\n" +
		"data: {\"candidates\":[\n" +
		"data: {\"finishReason\":\"STOP\"}]}\n\n" +
		"data: [DONE]\n\n"
	var events []json.RawMessage
	err := ParseSSE(context.Background(), bytes.NewBufferString(input), SSEOptions{RequireTerminal: true}, func(event json.RawMessage) error {
		events = append(events, event)
		return nil
	})
	require.NoError(t, err)
	require.Len(t, events, 1)
	require.JSONEq(t, `{"candidates":[{"finishReason":"STOP"}]}`, string(events[0]))
}

func TestParseSSEBoundsCancellationAndMissingTerminal(t *testing.T) {
	err := ParseSSE(context.Background(), bytes.NewBufferString("data: {\"text\":\"0123456789\"}\n\n"), SSEOptions{MaxEventBytes: 8}, func(json.RawMessage) error { return nil })
	require.ErrorContains(t, err, "exceeds 8 bytes")

	err = ParseSSE(context.Background(), bytes.NewBufferString("data: {\"value\":1}\n\n"), SSEOptions{RequireTerminal: true}, func(json.RawMessage) error { return nil })
	require.ErrorIs(t, err, ErrMissingTerminal)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = ParseSSE(ctx, bytes.NewBufferString("data: {\"value\":1}\n\n"), SSEOptions{}, func(json.RawMessage) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestClientUsesPublicGenerateContentAndInteractionsPaths(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		require.Equal(t, "Bearer test", r.Header.Get("Authorization"))
		switch r.Header.Get("Accept") {
		case "text/event-stream":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"event_type\":\"interaction.complete\"}\n\ndata: [DONE]\n\n"))
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"result-1"}`))
		}
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL, Headers: http.Header{"Authorization": []string{"Bearer test"}}, SSE: SSEOptions{RequireTerminal: true}}
	result, err := client.GenerateContent(context.Background(), "gemini-test", json.RawMessage(`{"contents":[]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"result-1"}`, string(result))
	result, err = client.CreateInteraction(context.Background(), json.RawMessage(`{"input":"hello"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"result-1"}`, string(result))
	var streamed []json.RawMessage
	err = client.StreamGenerateContent(context.Background(), "gemini-test", json.RawMessage(`{"contents":[]}`), func(event json.RawMessage) error {
		streamed = append(streamed, event)
		return nil
	})
	require.NoError(t, err)
	err = client.StreamInteraction(context.Background(), json.RawMessage(`{"input":"hello","stream":true}`), func(event json.RawMessage) error { return nil })
	require.NoError(t, err)

	require.Equal(t, []string{
		"/v1beta/models/gemini-test:generateContent",
		"/v1beta/interactions",
		"/v1beta/models/gemini-test:streamGenerateContent?alt=sse",
		"/v1beta/interactions",
	}, paths)
	require.Len(t, streamed, 1)
}

func TestInteractionContinuationAndRequestField(t *testing.T) {
	state := &InteractionState{}
	request := InteractionRequest{Stable: json.RawMessage(`{"model":"gemini-test","tools":[]}`), Input: json.RawMessage(`"hello"`), Store: true}
	plan, err := state.Plan(request)
	require.NoError(t, err)
	require.Equal(t, "no_previous_interaction", plan.FallbackReason)
	require.NoError(t, state.Commit(request, "interaction-1"))

	next := InteractionRequest{Stable: json.RawMessage(`{"tools":[],"model":"gemini-test"}`), Input: json.RawMessage(`"next"`), Store: true}
	plan, err = state.Plan(next)
	require.NoError(t, err)
	require.True(t, plan.Continued)
	require.Equal(t, "interaction-1", plan.PreviousInteractionID)
	body, err := ApplyPreviousInteraction(json.RawMessage(`{"input":"next"}`), plan.PreviousInteractionID)
	require.NoError(t, err)
	require.JSONEq(t, `{"input":"next","previous_interaction_id":"interaction-1"}`, string(body))

	changed := next
	changed.Stable = json.RawMessage(`{"model":"other"}`)
	plan, err = state.Plan(changed)
	require.NoError(t, err)
	require.Equal(t, "request_properties_changed", plan.FallbackReason)
}

func TestParseSSEPropagatesYieldError(t *testing.T) {
	expected := errors.New("stop")
	err := ParseSSE(context.Background(), bytes.NewBufferString("data: {\"value\":1}\n\n"), SSEOptions{}, func(json.RawMessage) error { return expected })
	require.ErrorIs(t, err, expected)
}
