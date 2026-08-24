package openairesponses

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCreateFrameAndDecodeEvent(t *testing.T) {
	frame, err := CreateFrame("stream-a", json.RawMessage(`{"model":"gpt-test","input":[]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"type":"response.create","stream_id":"stream-a","model":"gpt-test","input":[]}`, string(frame))

	event, err := DecodeEvent([]byte(`{"type":"response.output_text.delta","stream_id":"stream-a","delta":"hello"}`))
	require.NoError(t, err)
	require.Equal(t, "response.output_text.delta", event.Type)
	require.Equal(t, "stream-a", event.StreamID)
	require.JSONEq(t, `{"type":"response.output_text.delta","stream_id":"stream-a","delta":"hello"}`, string(event.Raw))
}

func TestContinuationStateUsesPreviousResponseOnlyForCompatibleAppendOnlyHistory(t *testing.T) {
	state := &ContinuationState{}
	first := ContinuationRequest{
		Stable: json.RawMessage(`{"model":"gpt-test","tools":[{"name":"view"}]}`),
		Input:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"one"}`)},
		Store:  true,
	}
	plan, err := state.Plan(first)
	require.NoError(t, err)
	require.False(t, plan.Incremental)
	require.Equal(t, "no_previous_response", plan.FallbackReason)
	require.NoError(t, state.Commit(first, "resp_1", []json.RawMessage{json.RawMessage(`{"role":"assistant","content":"answer"}`)}))

	next := ContinuationRequest{
		Stable: json.RawMessage(`{ "tools": [{"name":"view"}], "model":"gpt-test" }`),
		Input: []json.RawMessage{
			json.RawMessage(`{"content":"one","role":"user"}`),
			json.RawMessage(`{"content":"answer","role":"assistant"}`),
			json.RawMessage(`{"role":"user","content":"two"}`),
		},
		Store: true,
	}
	plan, err = state.Plan(next)
	require.NoError(t, err)
	require.True(t, plan.Incremental)
	require.Equal(t, "resp_1", plan.PreviousResponseID)
	require.Len(t, plan.Input, 1)
	require.JSONEq(t, `{"role":"user","content":"two"}`, string(plan.Input[0]))

	changed := next
	changed.Stable = json.RawMessage(`{"model":"other"}`)
	plan, err = state.Plan(changed)
	require.NoError(t, err)
	require.False(t, plan.Incremental)
	require.Equal(t, "request_properties_changed", plan.FallbackReason)
}

func TestContinuationStateFallsBackForDisabledStorageAndChangedHistory(t *testing.T) {
	state := &ContinuationState{}
	request := ContinuationRequest{Stable: json.RawMessage(`{"model":"gpt-test"}`), Input: []json.RawMessage{json.RawMessage(`{"id":1}`)}, Store: true}
	require.NoError(t, state.Commit(request, "resp_1", nil))

	disabled := request
	disabled.Store = false
	plan, err := state.Plan(disabled)
	require.NoError(t, err)
	require.Equal(t, "storage_disabled", plan.FallbackReason)

	changed := request
	changed.Input = []json.RawMessage{json.RawMessage(`{"id":2}`)}
	plan, err = state.Plan(changed)
	require.NoError(t, err)
	require.Equal(t, "history_not_append_only", plan.FallbackReason)
}

func TestContinuationStateResetsAfterInvalidOrEmptyCommit(t *testing.T) {
	state := &ContinuationState{}
	request := ContinuationRequest{
		Stable: json.RawMessage(`{"model":"gpt-test"}`),
		Input:  []json.RawMessage{json.RawMessage(`{"role":"user","content":"one"}`)},
		Store:  true,
	}
	require.NoError(t, state.Commit(request, "resp_1", nil))
	require.NoError(t, state.Commit(request, "", nil))
	plan, err := state.Plan(request)
	require.NoError(t, err)
	require.Equal(t, "no_previous_response", plan.FallbackReason)

	require.NoError(t, state.Commit(request, "resp_2", nil))
	invalid := request
	invalid.Stable = json.RawMessage(`{"model":`)
	require.Error(t, state.Commit(invalid, "resp_3", nil))
	plan, err = state.Plan(request)
	require.NoError(t, err)
	require.Equal(t, "no_previous_response", plan.FallbackReason)

	invalid = request
	invalid.Input = []json.RawMessage{json.RawMessage(`{"role":`)}
	_, err = state.Plan(invalid)
	require.ErrorContains(t, err, "invalid JSON")
}

func TestPublicCompactionClientUsesDocumentedPathAndBounds(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses/compact", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer public", r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"cmp_1","output":[],"usage":{"input_tokens":4}}`))
	}))
	defer server.Close()

	client := CompactionClient{
		BaseURL: server.URL + "/v1",
		Headers: http.Header{"Authorization": []string{"Bearer public"}},
	}
	result, err := client.Compact(context.Background(), json.RawMessage(`{"model":"gpt-test","input":[]}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"cmp_1","output":[],"usage":{"input_tokens":4}}`, string(result))

	client.MaxResponseBytes = 4
	_, err = client.Compact(context.Background(), json.RawMessage(`{"model":"gpt-test","input":[]}`))
	require.ErrorContains(t, err, "exceeds 4 bytes")
}

func TestPublicCompactionClientRejectsInvalidFailureAndCancellation(t *testing.T) {
	client := CompactionClient{BaseURL: "https://example.invalid/v1"}
	_, err := client.Compact(t.Context(), json.RawMessage(`[]`))
	require.ErrorContains(t, err, "JSON object")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client = CompactionClient{BaseURL: server.URL + "/v1"}
	_, err = client.Compact(t.Context(), json.RawMessage(`{"model":"gpt-test"}`))
	require.ErrorContains(t, err, "HTTP 503")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.Compact(ctx, json.RawMessage(`{"model":"gpt-test"}`))
	require.ErrorIs(t, err, context.Canceled)
}
