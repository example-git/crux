package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestAuthenticationRefreshNativeModelContract(t *testing.T) {
	for _, method := range []string{"generate", "stream", "generate-object", "stream-object"} {
		for _, scenario := range []struct {
			name                                   string
			attempts                               int
			expired, never, rejectFresh, transient bool
			retryAuthStatus                        bool
			wantRequests, wantRefresh              int
			wantError                              bool
		}{
			{name: "refresh-once", attempts: 2, wantRequests: 2, wantRefresh: 1},
			{name: "refresh-before-auth-status-retry", attempts: 2, retryAuthStatus: true, wantRequests: 2, wantRefresh: 1},
			{name: "expired", attempts: 2, expired: true, wantRequests: 1, wantRefresh: 1},
			{name: "never", attempts: 2, never: true, wantRequests: 1, wantError: true},
			{name: "fresh-rejected", attempts: 3, rejectFresh: true, wantRequests: 2, wantRefresh: 1, wantError: true},
			{name: "http-and-auth-budget", attempts: 3, transient: true, wantRequests: 3, wantRefresh: 1},
			{name: "budget-exhausted", attempts: 2, transient: true, wantRequests: 2, wantError: true},
		} {
			t.Run(method+"/"+scenario.name, func(t *testing.T) {
				var mu sync.Mutex
				var credentials []string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var document map[string]any
					require.NoError(t, json.NewDecoder(r.Body).Decode(&document))
					mu.Lock()
					credentials = append(credentials, r.Header.Get("Authorization"))
					index := len(credentials)
					mu.Unlock()
					if scenario.transient && index == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":{"message":"temporary"}}`))
						return
					}
					if r.Header.Get("Authorization") != "Bearer fresh" || scenario.rejectFresh {
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(`{"error":{"message":"expired","type":"authentication_error"}}`))
						return
					}
					writeAuthResponse(w, document["stream"] == true)
				}))
				defer server.Close()
				policy := manifest.RetryPolicy{MaxAttempts: scenario.attempts, Statuses: []int{503}, Authentication: "refresh-once", ReplayRequirement: "before-first-event"}
				if scenario.retryAuthStatus {
					policy.Statuses = append(policy.Statuses, 401)
				}
				if scenario.never {
					policy.Authentication = "never"
				}
				build := func(key string) fantasy.LanguageModel {
					transportPolicy := policy
					transportPolicy.MaxAttempts = 1
					transport := &providertransport.PolicyTransport{Operation: &providertransport.Operation{Endpoint: manifest.Endpoint{BaseURL: server.URL}, Path: "/responses", Retry: transportPolicy}}
					provider, err := openai.New(openai.WithAPIKey(key), openai.WithBaseURL(server.URL), openai.WithHTTPClient(&http.Client{Transport: transport}), openai.WithUseResponsesAPI(), openai.WithResponsesAPIFunc(func(string) bool { return true }))
					require.NoError(t, err)
					inner, err := provider.LanguageModel(t.Context(), "gpt-test")
					require.NoError(t, err)
					return NewLifecycleModelWithErrorMappingsAndStore(inner, policy, nil, nil, NewContinuationStore(), "account")
				}
				refreshes := 0
				model := providertransport.NewAuthRefreshModel(build("old"), policy, func() bool { return scenario.expired }, func(ctx context.Context) (fantasy.LanguageModel, error) {
					refreshes++
					_, budget := providertransport.ContextWithAttemptBudget(ctx, 10)
					require.EqualValues(t, 10, budget.Remaining(), "token exchange must not consume inference attempts")
					return build("fresh"), nil
				})
				text, err := callAuthModel(t.Context(), model, method)
				if scenario.wantError {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					require.Contains(t, text, "verified")
				}
				require.Equal(t, scenario.wantRefresh, refreshes)
				mu.Lock()
				defer mu.Unlock()
				require.Len(t, credentials, scenario.wantRequests)
				if scenario.expired {
					require.Equal(t, []string{"Bearer fresh"}, credentials)
				}
				if scenario.wantRefresh > 0 {
					require.Equal(t, "Bearer fresh", credentials[len(credentials)-1])
				}
			})
		}
	}
}

func callAuthModel(ctx context.Context, model fantasy.LanguageModel, method string) (string, error) {
	// Responses emits an unsupported-top-k warning before inspecting stream
	// status. It must not make an otherwise eligible 401 refresh unreachable.
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("answer")}, TopK: new(int64(1))}
	object := fantasy.ObjectCall{Prompt: call.Prompt, TopK: call.TopK, Schema: fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"answer": {Type: "string"}}, Required: []string{"answer"}}}
	switch method {
	case "generate":
		response, err := model.Generate(ctx, call)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(response.Content), nil
	case "generate-object":
		response, err := model.GenerateObject(ctx, object)
		if err != nil {
			return "", err
		}
		return fmt.Sprint(response.Object), nil
	case "stream-object":
		stream, err := model.StreamObject(ctx, object)
		if err != nil {
			return "", err
		}
		text := ""
		for part := range stream {
			if part.Error != nil {
				return text, part.Error
			}
			text += part.Delta
			if part.Object != nil {
				text += fmt.Sprint(part.Object)
			}
		}
		return text, nil
	default:
		stream, err := model.Stream(ctx, call)
		if err != nil {
			return "", err
		}
		text := ""
		for part := range stream {
			if part.Error != nil {
				return text, part.Error
			}
			text += part.Delta
		}
		return text, nil
	}
}

func writeAuthResponse(w http.ResponseWriter, stream bool) {
	text := `{"answer":"verified"}`
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp_verified","object":"response","status":"completed","model":"gpt-test","output":[{"id":"msg","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":%q,"annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`, text)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_verified\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
	_, _ = fmt.Fprintf(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg\",\"delta\":%q}\n\n", text)
	_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_verified\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
}
