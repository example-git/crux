package antigravity

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func collectSSE(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for payload, err := range sseEvents(strings.NewReader(body)) {
		if err != nil {
			t.Fatalf("sse error: %v", err)
		}
		out = append(out, string(payload))
	}
	return out
}

func TestSSEEventsUnwrapAndSplit(t *testing.T) {
	t.Parallel()

	body := "data: {\"response\":{\"candidates\":[1]}}\n\ndata: {\"response\":{\"candidates\":[2]}}\n\n"
	events := collectSSE(t, body)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
}

// The endpoint sometimes emits consecutive events with no separator at all.
func TestSSEEventsConcatenated(t *testing.T) {
	t.Parallel()

	body := `data: {"a":1}data: {"b":2}`
	events := collectSSE(t, body)
	if len(events) != 2 || events[0] != `{"a":1}` || events[1] != `{"b":2}` {
		t.Fatalf("events = %q", events)
	}
}

func TestSSEEventsBracesInStrings(t *testing.T) {
	t.Parallel()

	body := `data: {"text":"code: fn() { return \"}\" }"}data: {"x":1}`
	events := collectSSE(t, body)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %q", len(events), events)
	}
}

func TestUnwrapPayload(t *testing.T) {
	t.Parallel()

	if got := string(unwrapPayload([]byte(`{"response":{"candidates":[]}}`))); got != `{"candidates":[]}` {
		t.Errorf("wrapped: got %q", got)
	}
	if got := string(unwrapPayload([]byte(`{"candidates":[]}`))); got != `{"candidates":[]}` {
		t.Errorf("flat: got %q", got)
	}
}

func TestJSONValueEnd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in       string
		end      int
		complete bool
	}{
		{`{}`, 2, true},
		{`{"a":"}"}extra`, 9, true},
		{`{"a":1`, 6, false},
		{`[1,2]`, 5, true},
	}
	for _, tt := range tests {
		end, complete := jsonValueEnd([]byte(tt.in))
		if end != tt.end || complete != tt.complete {
			t.Errorf("jsonValueEnd(%q) = (%d, %v), want (%d, %v)", tt.in, end, complete, tt.end, tt.complete)
		}
	}
}

// Requests must be wrapped in the Antigravity envelope with model id passed
// through verbatim for the whole lineup, Claude ids included.
func TestEnvelopeShape(t *testing.T) {
	var sessionIDs []string
	for _, id := range []string{"claude-opus-4-6", "gemini-pro-agent", "gpt-oss-120b-medium"} {
		var gotPath, gotAuth string
		var gotBody map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotPath = r.URL.Path
			gotAuth = r.Header.Get("Authorization")
			data, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(data, &gotBody)
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte("data: {\"response\":{\"candidates\":[]}}\n\n"))
		}))

		c := &client{
			httpClient: srv.Client(),
			baseURL:    srv.URL,
			token:      func() string { return "tok" },
			project:    func(_ context.Context, _ string) string { return "proj" },
			userAgent:  "test-ua",
		}
		chunks, err := c.streamGenerateContent(t.Context(), id, &wireRequest{Contents: []wireContent{}})
		if err != nil {
			srv.Close()
			t.Fatalf("%s: %v", id, err)
		}
		for range chunks {
		}
		srv.Close()

		if gotPath != "/v1internal:streamGenerateContent" {
			t.Errorf("%s: path = %q", id, gotPath)
		}
		if gotAuth != "Bearer tok" {
			t.Errorf("%s: auth = %q", id, gotAuth)
		}
		if gotBody["model"] != id {
			t.Errorf("%s: envelope model = %v", id, gotBody["model"])
		}
		if gotBody["project"] != "proj" || gotBody["userAgent"] != "antigravity" || gotBody["requestType"] != "agent" {
			t.Errorf("%s: envelope fields wrong: %v", id, gotBody)
		}
		inner, _ := gotBody["request"].(map[string]any)
		if inner == nil || inner["sessionId"] == "" {
			t.Errorf("%s: inner request missing sessionId: %v", id, inner)
			continue
		}
		sessionIDs = append(sessionIDs, inner["sessionId"].(string))
	}
	for i := 1; i < len(sessionIDs); i++ {
		if sessionIDs[i] == sessionIDs[i-1] {
			t.Errorf("sessionId reused across requests: %q", sessionIDs[i])
		}
	}
}

// 503 responses are retried; hard capacity exhaustion is not.
func TestRetryPolicy(t *testing.T) {
	t.Parallel()

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"hi"}]},"finishReason":"STOP"}]}}`))
	}))
	defer srv.Close()

	c := &client{httpClient: srv.Client(), baseURL: srv.URL}
	resp, err := c.generateContent(t.Context(), "m", &wireRequest{})
	if err != nil {
		t.Fatalf("expected retry success, got %v", err)
	}
	if len(resp.Candidates) != 1 {
		t.Fatalf("bad response: %+v", resp)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
}

func TestHardCapacityExhaustionNotRetried(t *testing.T) {
	t.Parallel()

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"code":429,"message":"no capacity","status":"RESOURCE_EXHAUSTED","details":[{"reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`))
	}))
	defer srv.Close()

	c := &client{httpClient: srv.Client(), baseURL: srv.URL}
	_, err := c.generateContent(t.Context(), "m", &wireRequest{})
	if err == nil {
		t.Fatal("expected error")
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
}
