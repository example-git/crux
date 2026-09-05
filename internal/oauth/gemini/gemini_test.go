package gemini

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

type geminiRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip geminiRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestNewProviderExecutesOperationMaxEventBytes(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "test")
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"response":{"padding":"` + strings.Repeat("a", 64) + `"}}`))
	}))
	defer server.Close()

	operation := &providertransport.Operation{Streaming: &manifest.StreamingPolicy{MaxEventBytes: 32}}
	provider, err := NewProvider(server.URL, func() string { return "" }, nil, operation, func() error { return nil })
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("test")}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for part := range stream {
		if part.Type != fantasy.StreamPartTypeError {
			continue
		}
		if part.Error == nil || !strings.Contains(part.Error.Error(), "SSE event exceeds 32 bytes") {
			t.Fatalf("stream error = %v", part.Error)
		}
		if attempts.Load() != 1 {
			t.Fatalf("attempts = %d, want 1", attempts.Load())
		}
		return
	}
	t.Fatal("expected stream overflow error")
}

func TestNewProviderExecutesOperationRetryPolicy(t *testing.T) {
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "test")
	var attempts atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}`))
	}))
	defer server.Close()

	operation := &providertransport.Operation{Retry: manifest.RetryPolicy{
		MaxAttempts:       3,
		Statuses:          []int{http.StatusServiceUnavailable},
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}}
	provider, err := NewProvider(server.URL, func() string { return "" }, nil, operation, func() error { return nil })
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	response, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("test")}})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Content) != 1 || response.Content[0].(fantasy.TextContent).Text != "ok" {
		t.Fatalf("response = %#v", response)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestInferenceHTTPClientExecutesOperationTimeouts(t *testing.T) {
	for _, test := range []struct {
		name    string
		request time.Duration
		idle    time.Duration
		want    string
	}{
		{name: "request", request: 25 * time.Millisecond, idle: time.Second, want: "Client.Timeout"},
		{name: "stream idle", request: time.Second, idle: 25 * time.Millisecond, want: "stream idle timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				response.WriteHeader(http.StatusOK)
				response.(http.Flusher).Flush()
				time.Sleep(250 * time.Millisecond)
			}))
			defer server.Close()
			operation := &providertransport.Operation{
				ConnectTimeout:    time.Second,
				RequestTimeout:    test.request,
				StreamIdleTimeout: test.idle,
			}
			client := inferenceHTTPClient(operation, func() error { return nil })
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			response, err := client.Do(request)
			if err == nil {
				_, err = io.ReadAll(response.Body)
				_ = response.Body.Close()
			}
			if err == nil || (test.name == "request" && !errors.Is(err, context.DeadlineExceeded)) || (test.name != "request" && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("timeout error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestOAuthIdentityAndProjectRejectOwnerReplacementBeforeDispatch(t *testing.T) {
	original := http.DefaultClient.Transport
	var dispatched atomic.Int64
	http.DefaultClient.Transport = geminiRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	})
	t.Cleanup(func() { http.DefaultClient.Transport = original })
	ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed")
	})

	_, err := tokenRequest(ctx, url.Values{})
	if err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("tokenRequest() error = %v", err)
	}
	if project := fetchProject(ctx, "token"); project != "" {
		t.Fatalf("fetchProject() = %q", project)
	}
	if email := AccountEmail(ctx, "token"); email != "" {
		t.Fatalf("AccountEmail() = %q", email)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatched = %d", dispatched.Load())
	}
}

func TestOAuthClientCredentialsRequired(t *testing.T) {
	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "")

	opened := false
	_, err := Authorize(context.Background(), func(string) error {
		opened = true
		return nil
	}, func() (string, error) {
		return "", nil
	})
	if err == nil || !strings.Contains(err.Error(), "GEMINI_OAUTH_CLIENT_ID") || !strings.Contains(err.Error(), "GEMINI_OAUTH_CLIENT_SECRET") {
		t.Fatalf("Authorize() error = %v, want missing credential guidance", err)
	}
	if opened {
		t.Fatal("Authorize() opened a browser without configured OAuth credentials")
	}

	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "client-id")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "client-secret")
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		t.Fatalf("oauthClientCredentials() error = %v", err)
	}
	if clientID != "client-id" || clientSecret != "client-secret" {
		t.Fatalf("oauthClientCredentials() = (%q, %q)", clientID, clientSecret)
	}
}

func TestParsePastedCode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		in        string
		wantCode  string
		wantState string
		wantErr   bool
	}{
		{"4/0AX4Xf", "4/0AX4Xf", "", false},
		{"code=abc&state=xyz", "abc", "xyz", false},
		{"https://antigravity.google/oauth-callback?code=abc&state=xyz", "abc", "xyz", false},
		{"  4/0AX4Xf  ", "4/0AX4Xf", "", false},
		{"", "", "", true},
	}

	for _, tt := range tests {
		code, state, err := parsePastedCode(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("parsePastedCode(%q) expected error", tt.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parsePastedCode(%q) unexpected error: %v", tt.in, err)
			continue
		}
		if code != tt.wantCode || state != tt.wantState {
			t.Errorf("parsePastedCode(%q) = (%q, %q), want (%q, %q)",
				tt.in, code, state, tt.wantCode, tt.wantState)
		}
	}
}
