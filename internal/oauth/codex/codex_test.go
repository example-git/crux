package codex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/gorilla/websocket"
)

type codexRoundTripFunc func(*http.Request) (*http.Response, error)

func testCodexImages() *manifest.ImagePolicy {
	return &manifest.ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/jpeg", "image/png", "image/webp"},
		MaxSourceBytes:     25 * 1024 * 1024,
		MaxSidePixels:      1920,
		MaxOutputBytes:     512 * 1024,
		MaxPatches:         2500,
		OutputMediaType:    "image/jpeg",
		FlattenAlpha:       "white",
		QualitySteps:       []int{85, 75, 65, 55, 45, 35, 25},
		ResizePercent:      80,
		HistoryBudget: &manifest.ImageHistoryBudget{
			RequestBytes:      14 * 1024 * 1024,
			RetryRequestBytes: 10 * 1024 * 1024,
			PerImageTargets:   []int64{512 * 1024, 256 * 1024, 64 * 1024},
			OmitOldImages:     true,
			RetainNewestImage: true,
		},
	}
}

func (roundTrip codexRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestNewProviderRequiresExecutableImagePolicy(t *testing.T) {
	validate := func() error { return nil }
	_, err := NewProvider("", nil, nil, nil, nil, nil, nil, nil, validate)
	if err == nil || !strings.Contains(err.Error(), "image history budget is unavailable") {
		t.Fatalf("NewProvider() error = %v", err)
	}

	withoutHistory := testCodexImages()
	withoutHistory.HistoryBudget = nil
	_, err = NewProvider("", nil, nil, nil, nil, nil, nil, withoutHistory, validate)
	if err == nil || !strings.Contains(err.Error(), "image history budget is unavailable") {
		t.Fatalf("NewProvider() error = %v", err)
	}

	malformed := testCodexImages()
	malformed.MaxSourceBytes = 0
	_, err = NewProvider("", nil, nil, nil, nil, nil, nil, malformed, validate)
	if err == nil || !strings.Contains(err.Error(), "max_source_bytes is outside the executable range") {
		t.Fatalf("NewProvider() error = %v", err)
	}
}

func TestNewProviderExecutesOperationMaxEventBytes(t *testing.T) {
	t.Setenv("CODEX_VERSION", "test")
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		payload := `{"type":"response.completed","padding":"` + strings.Repeat("a", 128) + `"}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}))
	defer server.Close()

	operation := &providertransport.Operation{Streaming: &manifest.StreamingPolicy{MaxEventBytes: 64}}
	provider, err := NewProvider(
		strings.Replace(server.URL, "http://", "ws://", 1),
		func() string { return "token" },
		func() string { return "account" },
		nil,
		responses.NewSessionStore(),
		operation,
		nil,
		testCodexImages(),
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	_, err = model.Generate(t.Context(), fantasy.Call{})
	if err == nil || !strings.Contains(err.Error(), "WebSocket event exceeds 64 bytes") {
		t.Fatalf("Generate() error = %v", err)
	}
	if connections.Load() != 1 {
		t.Fatalf("connections = %d, want 1", connections.Load())
	}
}

func TestNewProviderRecoversAbnormalClosureBeyondOperationRetryPolicy(t *testing.T) {
	t.Setenv("CODEX_VERSION", "test")
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection := connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if connection < 4 {
			_ = conn.UnderlyingConn().Close()
			return
		}
		item := map[string]any{
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{{
				"type": "output_text",
				"text": "answer",
			}},
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "answer"}); err != nil {
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "response.output_item.done", "item": item}); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response", "output": []any{}}})
	}))
	defer server.Close()

	operation := &providertransport.Operation{Retry: manifest.RetryPolicy{
		MaxAttempts:       3,
		TransportErrors:   true,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}}
	store := responses.NewSessionStore()
	defer store.Close()
	provider, err := NewProvider(
		strings.Replace(server.URL, "http://", "ws://", 1),
		func() string { return "token" },
		func() string { return "account" },
		nil,
		store,
		operation,
		nil,
		testCodexImages(),
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	response, err := model.Generate(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Content.Text() != "answer" {
		t.Fatalf("response text = %q, want answer", response.Content.Text())
	}
	if connections.Load() != 4 {
		t.Fatalf("connections = %d, want 4", connections.Load())
	}
}

func TestNewProviderExecutesCompactionOperationRetryPolicy(t *testing.T) {
	t.Setenv("CODEX_VERSION", "test")
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection := connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if connection < 3 {
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type": "response.output_item.done",
			"item": map[string]any{"type": "compaction", "encrypted_content": "opaque"},
		})
		_ = conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "response"},
		})
	}))
	defer server.Close()

	inference := &providertransport.Operation{Retry: manifest.RetryPolicy{
		MaxAttempts:       1,
		Authentication:    "never",
		ReplayRequirement: "never",
	}}
	compaction := &providertransport.Operation{Retry: manifest.RetryPolicy{
		MaxAttempts:       3,
		TransportErrors:   true,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}}
	store := responses.NewSessionStore()
	defer store.Close()
	provider, err := NewProvider(
		strings.Replace(server.URL, "http://", "ws://", 1),
		func() string { return "token" },
		func() string { return "account" },
		nil,
		store,
		inference,
		compaction,
		testCodexImages(),
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	compactor, ok := model.(interface {
		Compact(context.Context, fantasy.Call) (*responses.CompactionResult, error)
	})
	if !ok {
		t.Fatal("LanguageModel does not expose remote compaction")
	}
	result, err := compactor.Compact(t.Context(), fantasy.Call{})
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if result == nil || result.History == nil {
		t.Fatal("Compact returned no checkpoint")
	}
	if connections.Load() != 3 {
		t.Fatalf("connections = %d, want 3", connections.Load())
	}
}

func TestNewProviderExecutesCompactionOperationMaxEventBytes(t *testing.T) {
	t.Setenv("CODEX_VERSION", "test")
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		payload := `{"type":"response.completed","padding":"` + strings.Repeat("a", 128) + `"}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(payload))
	}))
	defer server.Close()

	compaction := &providertransport.Operation{
		Streaming: &manifest.StreamingPolicy{MaxEventBytes: 64},
		Retry: manifest.RetryPolicy{
			MaxAttempts:       1,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
	}
	store := responses.NewSessionStore()
	defer store.Close()
	provider, err := NewProvider(
		strings.Replace(server.URL, "http://", "ws://", 1),
		func() string { return "token" },
		func() string { return "account" },
		nil,
		store,
		nil,
		compaction,
		testCodexImages(),
		func() error { return nil },
	)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	model, err := provider.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatalf("LanguageModel: %v", err)
	}
	compactor, ok := model.(interface {
		Compact(context.Context, fantasy.Call) (*responses.CompactionResult, error)
	})
	if !ok {
		t.Fatal("LanguageModel does not expose remote compaction")
	}
	_, err = compactor.Compact(t.Context(), fantasy.Call{})
	if err == nil || !strings.Contains(err.Error(), "WebSocket event exceeds 64 bytes") {
		t.Fatalf("Compact() error = %v", err)
	}
	if connections.Load() != 1 {
		t.Fatalf("connections = %d, want 1", connections.Load())
	}
}

func TestNewProviderExecutesCompactionOperationTimeouts(t *testing.T) {
	t.Setenv("CODEX_VERSION", "test")
	tests := []struct {
		name             string
		configure        func(*providertransport.Operation)
		stalledHandshake bool
		wantError        string
		wantDeadline     bool
	}{
		{
			name: "connect",
			configure: func(operation *providertransport.Operation) {
				operation.ConnectTimeout = 40 * time.Millisecond
			},
			stalledHandshake: true,
		},
		{
			name: "request",
			configure: func(operation *providertransport.Operation) {
				operation.RequestTimeout = 40 * time.Millisecond
			},
			wantDeadline: true,
		},
		{
			name: "idle",
			configure: func(operation *providertransport.Operation) {
				operation.StreamIdleTimeout = 40 * time.Millisecond
			},
			wantError: "idle timeout waiting for WebSocket activity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var connections atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				connections.Add(1)
				if test.stalledHandshake {
					<-r.Context().Done()
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()

			compaction := &providertransport.Operation{
				ConnectTimeout:    2 * time.Second,
				RequestTimeout:    2 * time.Second,
				StreamIdleTimeout: 2 * time.Second,
				Retry: manifest.RetryPolicy{
					MaxAttempts:       1,
					Authentication:    "never",
					ReplayRequirement: "before-first-event",
				},
			}
			test.configure(compaction)
			store := responses.NewSessionStore()
			defer store.Close()
			provider, err := NewProvider(
				strings.Replace(server.URL, "http://", "ws://", 1),
				func() string { return "token" },
				func() string { return "account" },
				nil,
				store,
				nil,
				compaction,
				testCodexImages(),
				func() error { return nil },
			)
			if err != nil {
				t.Fatalf("NewProvider: %v", err)
			}
			model, err := provider.LanguageModel(t.Context(), "model")
			if err != nil {
				t.Fatalf("LanguageModel: %v", err)
			}
			compactor, ok := model.(interface {
				Compact(context.Context, fantasy.Call) (*responses.CompactionResult, error)
			})
			if !ok {
				t.Fatal("LanguageModel does not expose remote compaction")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			started := time.Now()
			_, err = compactor.Compact(ctx, fantasy.Call{})
			elapsed := time.Since(started)
			if err == nil {
				t.Fatal("Compact() expected timeout error")
			}
			if test.wantError != "" && !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Compact() error = %v, want %q", err, test.wantError)
			}
			if test.wantDeadline && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Compact() error = %v, want context deadline", err)
			}
			if elapsed >= 1500*time.Millisecond {
				t.Fatalf("Compact() elapsed = %s, declared timeout was not applied", elapsed)
			}
			if connections.Load() != 1 {
				t.Fatalf("connections = %d, want 1", connections.Load())
			}
		})
	}
}

func TestOAuthAndIdentityRejectOwnerReplacementBeforeDispatch(t *testing.T) {
	original := http.DefaultClient.Transport
	var dispatched atomic.Int64
	http.DefaultClient.Transport = codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
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
	if email := AccountEmail(ctx, "token"); email != "" {
		t.Fatalf("AccountEmail() = %q", email)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatched = %d", dispatched.Load())
	}
}

func TestOAuthClientIDRequired(t *testing.T) {
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "")

	opened := false
	_, err := Authorize(context.Background(), func(string) error {
		opened = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "CODEX_OAUTH_CLIENT_ID") {
		t.Fatalf("Authorize() error = %v, want missing client ID guidance", err)
	}
	if opened {
		t.Fatal("Authorize() opened a browser without a configured OAuth client ID")
	}

	t.Setenv("CODEX_OAUTH_CLIENT_ID", "client-id")
	clientID, err := oauthClientID()
	if err != nil {
		t.Fatalf("oauthClientID() error = %v", err)
	}
	if clientID != "client-id" {
		t.Fatalf("oauthClientID() = %q", clientID)
	}
}
