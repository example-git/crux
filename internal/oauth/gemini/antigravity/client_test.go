package antigravity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip testRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func collectSSE(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for payload, err := range sseEvents(strings.NewReader(body), 0) {
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

func TestSSEEventsMaxEventBytes(t *testing.T) {
	t.Parallel()

	payload := `{"x":"` + strings.Repeat("a", 8) + `"}`
	var events []string
	for event, err := range sseEvents(strings.NewReader("data: "+payload+"data: {}"), int64(len(payload))) {
		if err != nil {
			t.Fatalf("exact-bound event: %v", err)
		}
		events = append(events, string(event))
	}
	if len(events) != 2 || events[0] != payload || events[1] != `{}` {
		t.Fatalf("events = %q", events)
	}

	overflow := payload[:len(payload)-2] + "a" + payload[len(payload)-2:]
	for event, err := range sseEvents(strings.NewReader("noise-data: "+overflow), int64(len(payload))) {
		if err == nil || !strings.Contains(err.Error(), "SSE event exceeds") {
			t.Fatalf("overflow event = %q, error = %v", event, err)
		}
		return
	}
	t.Fatal("expected overflow error")
}

func TestStreamGenerateContentRejectsOversizedEvent(t *testing.T) {
	t.Parallel()

	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"response":{"padding":"` + strings.Repeat("a", 64) + `"}}`))
	}))
	defer srv.Close()

	c := &client{httpClient: srv.Client(), baseURL: srv.URL, maxEventBytes: 32}
	chunks, err := c.streamGenerateContent(t.Context(), "model", &wireRequest{})
	if err != nil {
		t.Fatalf("streamGenerateContent: %v", err)
	}
	for chunk, streamErr := range chunks {
		if streamErr == nil || !strings.Contains(streamErr.Error(), "SSE event exceeds 32 bytes") {
			t.Fatalf("chunk = %#v, error = %v", chunk, streamErr)
		}
		if attempts != 1 {
			t.Fatalf("attempts = %d, want 1", attempts)
		}
		return
	}
	t.Fatal("expected stream overflow error")
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

	c := &client{
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		retry: manifest.RetryPolicy{
			MaxAttempts:       3,
			Statuses:          []int{http.StatusServiceUnavailable},
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
	}
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

func TestRetryPolicyUsesOneBudgetAcrossFailureClasses(t *testing.T) {
	var attempts atomic.Int32
	httpClient := &http.Client{Transport: testRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch attempts.Add(1) {
		case 1, 3:
			return nil, errors.New("network unavailable")
		default:
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"busy"}}`)),
				Request:    request,
			}, nil
		}
	})}
	c := &client{
		httpClient: httpClient,
		baseURL:    "https://example.invalid",
		retry: manifest.RetryPolicy{
			MaxAttempts:       3,
			Statuses:          []int{http.StatusServiceUnavailable},
			TransportErrors:   true,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
	}
	_, err := c.generateContent(t.Context(), "m", &wireRequest{})
	if err == nil {
		t.Fatal("expected retry exhaustion")
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestRetryPolicyLeavesAuthenticationForOuterRefresh(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &client{
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		retry: manifest.RetryPolicy{
			MaxAttempts:       4,
			Statuses:          []int{http.StatusTooManyRequests, http.StatusServiceUnavailable},
			TransportErrors:   true,
			Authentication:    "refresh-once",
			ReplayRequirement: "before-first-event",
		},
	}
	_, err := c.generateContent(t.Context(), "m", &wireRequest{})
	if err == nil {
		t.Fatal("expected authentication error")
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", attempts.Load())
	}
}

func TestStreamRetryPolicyReplaysOnlyBeforeFirstEvent(t *testing.T) {
	for _, test := range []struct {
		name         string
		firstPayload string
		wantAttempts int32
	}{
		{name: "before first event", wantAttempts: 2},
		{name: "truncated before first event", firstPayload: `data: {"response":{"candidates":[`, wantAttempts: 2},
		{name: "after first event", firstPayload: `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}`, wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempt := attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if attempt == 1 && test.firstPayload != "" {
					_, _ = w.Write([]byte(test.firstPayload))
					return
				}
				if attempt == 2 {
					_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"finishReason":"STOP"}]}}`))
				}
			}))
			defer srv.Close()

			c := &client{
				httpClient: srv.Client(),
				baseURL:    srv.URL,
				retry: manifest.RetryPolicy{
					MaxAttempts:       2,
					UnexpectedEOF:     true,
					Authentication:    "never",
					ReplayRequirement: "before-first-event",
				},
			}
			chunks, err := c.streamGenerateContent(t.Context(), "m", &wireRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var chunkCount int
			for _, streamErr := range chunks {
				if streamErr != nil {
					t.Fatal(streamErr)
				}
				chunkCount++
			}
			if chunkCount != 1 {
				t.Fatalf("chunks = %d, want 1", chunkCount)
			}
			if attempts.Load() != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", attempts.Load(), test.wantAttempts)
			}
		})
	}
}

func TestStreamRetryPolicyDoesNotReplayMalformedCompleteEvent(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"response":}`))
	}))
	defer srv.Close()

	c := &client{
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		retry: manifest.RetryPolicy{
			MaxAttempts:       2,
			UnexpectedEOF:     true,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
	}
	chunks, err := c.streamGenerateContent(t.Context(), "m", &wireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for chunk, streamErr := range chunks {
		if streamErr == nil || !strings.Contains(streamErr.Error(), "antigravity: decode chunk") {
			t.Fatalf("chunk = %#v, error = %v", chunk, streamErr)
		}
		if attempts.Load() != 1 {
			t.Fatalf("attempts = %d, want 1", attempts.Load())
		}
		return
	}
	t.Fatal("expected malformed event error")
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

func TestMappedHTTPErrorOverridesHardCapacityRetryVeto(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":429,"message":"declared capacity","status":"RESOURCE_EXHAUSTED","details":[{"reason":"MODEL_CAPACITY_EXHAUSTED"}]}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]} ,"finishReason":"STOP"}]}}`))
	}))
	defer srv.Close()

	c := &client{
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		retry: manifest.RetryPolicy{
			MaxAttempts:       2,
			Statuses:          []int{http.StatusTooManyRequests},
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
		errors: []manifest.ErrorMapping{{
			Class:          "capacity",
			Statuses:       []int{http.StatusTooManyRequests},
			Codes:          []string{"RESOURCE_EXHAUSTED"},
			CodePointer:    "/error/status",
			MessagePointer: "/error/message",
			Retryable:      true,
		}},
	}
	response, err := c.generateContent(t.Context(), "m", &wireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Candidates) != 1 || attempts.Load() != 2 {
		t.Fatalf("response = %#v, attempts = %d", response, attempts.Load())
	}
}

func TestMappedBodyErrorUsesOneRetryBudget(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			_, _ = w.Write([]byte(`{"response":{"error":{"code":418,"message":"retry body","status":"TEMPORARY"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"finishReason":"STOP"}]}}`))
	}))
	defer srv.Close()

	c := &client{
		httpClient: srv.Client(),
		baseURL:    srv.URL,
		retry: manifest.RetryPolicy{
			MaxAttempts:       2,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
		errors: []manifest.ErrorMapping{{
			Class:          "server",
			Codes:          []string{"TEMPORARY"},
			CodePointer:    "/error/status",
			MessagePointer: "/error/message",
			Retryable:      true,
		}},
	}
	if _, err := c.generateContent(t.Context(), "m", &wireRequest{}); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestMappedStreamErrorReplaysOnlyBeforeOutput(t *testing.T) {
	for _, test := range []struct {
		name         string
		firstPayload string
		wantAttempts int32
		wantChunks   int
		wantError    bool
	}{
		{
			name:         "before output",
			firstPayload: `data: {"response":{"error":{"code":418,"message":"retry stream","status":"TEMPORARY"}}}`,
			wantAttempts: 2,
			wantChunks:   1,
		},
		{
			name:         "after output",
			firstPayload: `data: {"response":{"candidates":[{"content":{"parts":[{"text":"partial"}]}}]}}data: {"response":{"error":{"code":418,"message":"stop stream","status":"TEMPORARY"}}}`,
			wantAttempts: 1,
			wantChunks:   1,
			wantError:    true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempt := attempts.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				if attempt == 1 {
					_, _ = w.Write([]byte(test.firstPayload))
					return
				}
				_, _ = w.Write([]byte(`data: {"response":{"candidates":[{"finishReason":"STOP"}]}}`))
			}))
			defer srv.Close()

			c := &client{
				httpClient: srv.Client(),
				baseURL:    srv.URL,
				retry: manifest.RetryPolicy{
					MaxAttempts:       2,
					Authentication:    "never",
					ReplayRequirement: "before-first-event",
				},
				errors: []manifest.ErrorMapping{{
					Class:          "capacity",
					Codes:          []string{"TEMPORARY"},
					CodePointer:    "/error/status",
					MessagePointer: "/error/message",
					Retryable:      true,
				}},
			}
			stream, err := c.streamGenerateContent(t.Context(), "m", &wireRequest{})
			if err != nil {
				t.Fatal(err)
			}
			var chunks int
			var streamErr error
			for chunk, err := range stream {
				if err != nil {
					streamErr = err
					break
				}
				if chunk != nil {
					chunks++
				}
			}
			if attempts.Load() != test.wantAttempts || chunks != test.wantChunks || (streamErr != nil) != test.wantError {
				t.Fatalf("attempts = %d, chunks = %d, error = %v", attempts.Load(), chunks, streamErr)
			}
			if test.wantError {
				var providerErr *fantasy.ProviderError
				if !errors.As(streamErr, &providerErr) || providerErr.Class != fantasy.ProviderErrorClassCapacity || providerErr.Message != "stop stream" {
					t.Fatalf("mapped error = %#v", streamErr)
				}
			}
		})
	}
}
