package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	providersse "github.com/example-git/crux/internal/providertransport/sse"
	"github.com/stretchr/testify/require"
)

func TestClientCreateUsesPublicResponsesPath(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "Bearer token", r.Header.Get("Authorization"))
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		_, _ = fmt.Fprint(w, `{"id":"resp_1"}`)
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL + "/v1", HTTPClient: server.Client(), Headers: http.Header{"Authorization": {"Bearer token"}}}
	response, err := client.Create(context.Background(), json.RawMessage(`{"model":"gpt-test"}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"id":"resp_1"}`, string(response))
}

func TestClientStreamParsesBoundedPublicEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/responses", r.URL.Path)
		require.Equal(t, "text/event-stream", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\"}\n\n")
	}))
	defer server.Close()

	client := Client{
		BaseURL: server.URL + "/v1", HTTPClient: server.Client(),
		SSE: providersse.Options{RequireTerminal: true, MaxEventBytes: 1024},
	}
	var types []string
	err := client.Stream(context.Background(), json.RawMessage(`{"model":"gpt-test","stream":true}`), func(event Event) error {
		types = append(types, event.Type)
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, []string{"response.output_text.delta", "response.completed"}, types)
}

func TestClientStreamRejectsWebSocketStreamID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"stream_id\":\"stream-1\"}\n\n")
	}))
	defer server.Close()

	client := Client{BaseURL: server.URL + "/v1", HTTPClient: server.Client()}
	err := client.Stream(context.Background(), json.RawMessage(`{}`), func(Event) error { return nil })
	require.ErrorContains(t, err, "unexpectedly contains stream_id")
}
