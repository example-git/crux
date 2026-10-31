package responses

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestTimeoutOptionsProjectToLanguageModelClient(t *testing.T) {
	retry := manifest.RetryPolicy{
		MaxAttempts:       4,
		Statuses:          []int{503},
		Codes:             []string{"temporary"},
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}
	mappings := []manifest.ErrorMapping{{Class: "server", Statuses: []int{503}, Codes: []string{"temporary"}, CodePointer: "/error/code", Retryable: true}}
	provider, err := New(
		WithTimeouts(11*time.Second, 12*time.Second, 13*time.Second),
		WithMaxEventBytes(14),
		WithRetryPolicy(retry),
		WithErrorMappings(mappings),
		WithCompactionPolicy(time.Second, 2*time.Second, 3*time.Second, 15, retry, mappings),
	)
	require.NoError(t, err)
	modelValue, err := provider.LanguageModel(t.Context(), "model")
	require.NoError(t, err)
	model := modelValue.(*languageModel)
	require.Equal(t, 11*time.Second, model.client.connectTimeout)
	require.Equal(t, 12*time.Second, model.client.requestTimeout)
	require.Equal(t, 13*time.Second, model.client.readTimeout)
	require.Equal(t, int64(14), model.client.maxEventBytes)
	require.Equal(t, retry, model.client.retry)
	require.Equal(t, mappings, model.client.errors)
	require.Equal(t, mappings, model.client.compaction.errors)
	retry.Statuses[0] = 429
	retry.Codes[0] = "changed"
	mappings[0].Class = "authentication"
	mappings[0].Statuses[0] = 401
	mappings[0].Codes[0] = "changed"
	require.Equal(t, []int{503}, model.client.retry.Statuses)
	require.Equal(t, []string{"temporary"}, model.client.retry.Codes)
	require.Equal(t, "server", model.client.errors[0].Class)
	require.Equal(t, []int{503}, model.client.errors[0].Statuses)
	require.Equal(t, []string{"temporary"}, model.client.errors[0].Codes)
	require.Equal(t, "server", model.client.compaction.errors[0].Class)
}

func TestPerCallRequestHeadersReachHandshakeWithoutLeaking(t *testing.T) {
	headers := make(chan string, 2)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers <- r.Header.Get(MemgenHeader)
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response"}}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	_, err := model.Generate(t.Context(), fantasy.Call{ProviderOptions: fantasy.ProviderOptions{
		Name: &ProviderOptions{RequestHeaders: map[string]string{MemgenHeader: "true"}},
	}})
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Equal(t, "true", <-headers)
	require.Empty(t, <-headers)
	require.Empty(t, model.client.headers)
}

func TestGenerateRejectsOversizedWebSocketEventWithoutReplay(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		payload := `{"type":"response.completed","padding":"` + strings.Repeat("a", 128) + `"}`
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
	}))
	defer server.Close()

	model := &languageModel{client: &client{
		url:           strings.Replace(server.URL, "http://", "ws://", 1),
		maxEventBytes: 64,
	}}
	_, err := model.Generate(context.Background(), fantasy.Call{})
	require.ErrorContains(t, err, "WebSocket event exceeds 64 bytes")
	require.Equal(t, int32(1), connections.Load())
}

func TestGenerateDoesNotReplayTransportFailureAfterFirstEvent(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": "partial",
		}))
		_ = conn.UnderlyingConn().Close()
	}))
	defer server.Close()

	model := &languageModel{client: &client{
		url: strings.Replace(server.URL, "http://", "ws://", 1),
		retry: manifest.RetryPolicy{
			MaxAttempts:       4,
			TransportErrors:   true,
			UnexpectedEOF:     true,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
	}}
	_, err := model.Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	require.Equal(t, int32(1), connections.Load())
}

func TestMappedTerminalErrorRetriesBeforeOutput(t *testing.T) {
	var attempts atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		if attempts.Add(1) == 1 {
			require.NoError(t, conn.WriteJSON(map[string]any{
				"type": "response.failed",
				"response": map[string]any{"error": map[string]any{
					"code":    "temporary_capacity",
					"message": "retry request",
				}},
			}))
			return
		}
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.output_text.delta", "delta": "recovered"}))
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "response"}}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{
		url: strings.Replace(server.URL, "http://", "ws://", 1),
		retry: manifest.RetryPolicy{
			MaxAttempts:       2,
			Authentication:    "never",
			ReplayRequirement: "before-first-event",
		},
		errors: []manifest.ErrorMapping{{
			Class:          "capacity",
			Codes:          []string{"temporary_capacity"},
			CodePointer:    "/response/error/code",
			MessagePointer: "/response/error/message",
			Retryable:      true,
		}},
	}}
	response, err := model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Equal(t, "recovered", response.Content.Text())
	require.Equal(t, int32(2), attempts.Load())
}

func TestConnectionRecoveryErrorsDoNotReplayAfterOutput(t *testing.T) {
	tests := []struct {
		name              string
		event             map[string]any
		errorMappings     []manifest.ErrorMapping
		wantConnectionErr bool
		wantClass         fantasy.ProviderErrorClass
	}{
		{
			name: "connection limit",
			event: map[string]any{
				"type":    "error",
				"code":    "websocket_connection_limit_reached",
				"message": fantasy.ConnectionLimitMessage,
			},
			wantConnectionErr: true,
		},
		{
			name: "mapped capacity",
			event: map[string]any{
				"type": "response.failed",
				"response": map[string]any{"error": map[string]any{
					"code":    "temporary_capacity",
					"message": "stop request",
				}},
			},
			errorMappings: []manifest.ErrorMapping{{
				Class:          "capacity",
				Codes:          []string{"temporary_capacity"},
				CodePointer:    "/response/error/code",
				MessagePointer: "/response/error/message",
				Retryable:      true,
			}},
			wantClass: fantasy.ProviderErrorClassCapacity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type":  "response.output_text.delta",
					"delta": "partial",
				}))
				require.NoError(t, conn.WriteJSON(test.event))
			}))
			defer server.Close()

			model := &languageModel{client: &client{
				url:    strings.Replace(server.URL, "http://", "ws://", 1),
				errors: test.errorMappings,
			}}
			retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](fantasy.RetryOptions{
				MaxRetries:     2,
				InitialDelayIn: time.Millisecond,
				BackoffFactor:  1,
			})
			_, err := retry(t.Context(), func() (*fantasy.Response, error) {
				return model.Generate(t.Context(), fantasy.Call{})
			})
			require.Error(t, err)
			require.Equal(t, int32(1), attempts.Load())
			var nonRetryable interface{ NonRetryable() bool }
			require.ErrorAs(t, err, &nonRetryable)
			require.True(t, nonRetryable.NonRetryable())
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.True(t, providerErr.IsRetryable())
			if test.wantConnectionErr {
				require.True(t, fantasy.IsConnectionLimitError(err))
			}
			if test.wantClass != "" {
				require.Equal(t, test.wantClass, providerErr.Class)
			}
		})
	}
}

func TestStreamDoesNotUseConnectionRecoveryForAccountOrAccessErrors(t *testing.T) {
	tests := []struct {
		name        string
		event       map[string]any
		mapping     manifest.ErrorMapping
		wantClass   fantasy.ProviderErrorClass
		wantMessage string
	}{
		{
			name: "account usage limit",
			event: map[string]any{
				"type":   "error",
				"status": 429,
				"error": map[string]any{
					"type":    "usage_limit_reached",
					"message": "The usage limit has been reached",
				},
			},
			mapping: manifest.ErrorMapping{
				Class:          "rate-limit",
				Codes:          []string{"usage_limit_reached"},
				CodePointer:    "/error/type",
				MessagePointer: "/error/message",
				Retryable:      true,
			},
			wantClass:   fantasy.ProviderErrorClassRateLimit,
			wantMessage: "The usage limit has been reached",
		},
		{
			name: "rate limit",
			event: map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "rate_limit_exceeded",
					"message": "request rate exceeded",
				},
			},
			mapping: manifest.ErrorMapping{
				Class:          "rate-limit",
				Codes:          []string{"rate_limit_exceeded"},
				CodePointer:    "/error/code",
				MessagePointer: "/error/message",
				Retryable:      true,
			},
			wantClass:   fantasy.ProviderErrorClassRateLimit,
			wantMessage: "request rate exceeded",
		},
		{
			name: "account credits",
			event: map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "insufficient_quota",
					"message": "account credits exhausted",
				},
			},
			mapping: manifest.ErrorMapping{
				Class:          "rate-limit",
				Codes:          []string{"insufficient_quota"},
				CodePointer:    "/error/code",
				MessagePointer: "/error/message",
				Retryable:      true,
			},
			wantClass:   fantasy.ProviderErrorClassRateLimit,
			wantMessage: "account credits exhausted",
		},
		{
			name: "authentication",
			event: map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "invalid_api_key",
					"message": "authentication failed",
				},
			},
			mapping: manifest.ErrorMapping{
				Class:          "authentication",
				Codes:          []string{"invalid_api_key"},
				CodePointer:    "/error/code",
				MessagePointer: "/error/message",
				Retryable:      true,
			},
			wantClass:   fantasy.ProviderErrorClassAuthentication,
			wantMessage: "authentication failed",
		},
		{
			name: "authorization",
			event: map[string]any{
				"type": "error",
				"error": map[string]any{
					"code":    "account_not_allowed",
					"message": "authorization failed",
				},
			},
			mapping: manifest.ErrorMapping{
				Class:          "authorization",
				Codes:          []string{"account_not_allowed"},
				CodePointer:    "/error/code",
				MessagePointer: "/error/message",
				Retryable:      true,
			},
			wantClass:   fantasy.ProviderErrorClassAuthorization,
			wantMessage: "authorization failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteJSON(test.event))
			}))
			defer server.Close()

			model := &languageModel{client: &client{
				url: strings.Replace(server.URL, "http://", "ws://", 1),
				retry: manifest.RetryPolicy{
					MaxAttempts:       1,
					Authentication:    "never",
					ReplayRequirement: "before-first-event",
				},
				errors: []manifest.ErrorMapping{test.mapping},
			}}
			_, err := model.Generate(t.Context(), fantasy.Call{})
			require.Error(t, err)
			require.Equal(t, int32(1), attempts.Load())
			var providerErr *fantasy.ProviderError
			require.ErrorAs(t, err, &providerErr)
			require.Equal(t, test.wantClass, providerErr.Class)
			require.Equal(t, test.wantMessage, providerErr.Message)
			require.True(t, providerErr.IsRetryable())
		})
	}
}

func TestStreamWarnsForUnknownEvent(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{"type": "response.future"}))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "response"},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	response, err := model.Generate(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	require.Len(t, response.Warnings, 1)
	require.Equal(t, fantasy.CallWarningTypeOther, response.Warnings[0].Type)
	require.Equal(t, "unrecognized Codex stream event: response.future", response.Warnings[0].Message)
}

func TestStreamNormalizesCachedTokenUsage(t *testing.T) {
	tests := []struct {
		name           string
		providerInput  int64
		providerCached int64
		providerOutput int64
		wantInput      int64
		wantCacheRead  int64
		wantTotal      int64
	}{
		{name: "no cache", providerInput: 100, providerOutput: 10, wantInput: 100, wantTotal: 110},
		{name: "partial cache", providerInput: 100, providerCached: 80, providerOutput: 10, wantInput: 20, wantCacheRead: 80, wantTotal: 110},
		{name: "fully cached", providerInput: 100, providerCached: 100, providerOutput: 10, wantCacheRead: 100, wantTotal: 110},
		{name: "cached exceeds input", providerInput: 100, providerCached: 120, providerOutput: 10, wantCacheRead: 120, wantTotal: 110},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type": "response.completed",
					"response": map[string]any{
						"usage": map[string]any{
							"input_tokens":  test.providerInput,
							"output_tokens": test.providerOutput,
							"input_tokens_details": map[string]any{
								"cached_tokens": test.providerCached,
							},
						},
					},
				}))
			}))
			defer server.Close()

			model := &languageModel{
				client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)},
			}
			response, err := model.Generate(context.Background(), fantasy.Call{})
			require.NoError(t, err)
			require.Equal(t, test.wantInput, response.Usage.InputTokens)
			require.Equal(t, test.wantCacheRead, response.Usage.CacheReadTokens)
			require.Equal(t, test.providerOutput, response.Usage.OutputTokens)
			require.Equal(t, test.wantTotal, response.Usage.TotalTokens)
		})
	}
}

func TestStreamMarksContextLengthErrors(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type": "response.failed",
			"response": map[string]any{
				"error": map[string]any{
					"code":    "context_length_exceeded",
					"message": "input exceeds context",
				},
			},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	_, err := model.Generate(context.Background(), fantasy.Call{})
	require.Error(t, err)
	var providerError *fantasy.ProviderError
	require.ErrorAs(t, err, &providerError)
	require.True(t, providerError.IsContextTooLarge())
}

func TestStreamRecoversServerOverloadBeforeOutput(t *testing.T) {
	requests := make(chan requestFrame, 2)
	var attempts atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		requests <- request
		if attempts.Add(1) == 1 {
			payload := `{"type":"error","message":"` + fantasy.ServerOverloadMessage + `"}`
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
			return
		}
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": "recovered",
		}))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "response"},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	response, err := model.Generate(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")},
	})
	require.NoError(t, err)
	require.Equal(t, "recovered", response.Content.Text())
	require.Equal(t, int32(2), attempts.Load())
	for range 2 {
		request := <-requests
		require.Empty(t, request.PreviousResponseID)
		require.Equal(t, []inputItem{testMessageItem("user", "first")}, request.Input)
	}
}

func TestStreamRecoversConnectionLimitBeforeOutput(t *testing.T) {
	tests := map[string]func(conn *websocket.Conn){
		"text frame": func(conn *websocket.Conn) {
			payload := `{"type":"error","message":"` + fantasy.ConnectionLimitMessage + `"}`
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(payload)))
		},
		"close reason": func(conn *websocket.Conn) {
			require.NoError(t, conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, fantasy.ConnectionLimitMessage),
				time.Now().Add(time.Second),
			))
		},
	}
	for name, respond := range tests {
		t.Run(name, func(t *testing.T) {
			requests := make(chan requestFrame, 2)
			var attempts atomic.Int32
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				requests <- request
				if attempts.Add(1) == 1 {
					respond(conn)
					return
				}
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type":  "response.output_text.delta",
					"delta": "recovered",
				}))
				require.NoError(t, conn.WriteJSON(map[string]any{
					"type":     "response.completed",
					"response": map[string]any{"id": "response"},
				}))
			}))
			defer server.Close()

			model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
			response, err := model.Generate(t.Context(), fantasy.Call{
				Prompt: fantasy.Prompt{fantasy.NewUserMessage("first")},
			})
			require.NoError(t, err)
			require.Equal(t, "recovered", response.Content.Text())
			require.Equal(t, int32(2), attempts.Load())
			for range 2 {
				request := <-requests
				require.Empty(t, request.PreviousResponseID)
				require.Equal(t, []inputItem{testMessageItem("user", "first")}, request.Input)
			}
		})
	}
}

func TestStreamDoesNotClassifyRetryPhrasesInOutput(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": fantasy.ServerOverloadMessage + " / " + fantasy.ConnectionLimitMessage,
		}))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	response, err := model.Generate(context.Background(), fantasy.Call{})
	require.NoError(t, err)
	require.Contains(t, response.Content.Text(), fantasy.ServerOverloadMessage)
	require.Contains(t, response.Content.Text(), fantasy.ConnectionLimitMessage)
}

func TestStreamTypedOverloadRetriesAndRecovers(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		if attempts.Add(1) == 1 {
			require.NoError(t, conn.WriteJSON(map[string]any{
				"type":    "error",
				"code":    "overloaded_error",
				"message": "busy",
			}))
			return
		}
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":  "response.output_text.delta",
			"delta": "recovered",
		}))
		require.NoError(t, conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{},
		}))
	}))
	defer server.Close()

	model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
	retry := fantasy.RetryWithExponentialBackoffRespectingRetryHeaders[*fantasy.Response](fantasy.RetryOptions{
		MaxRetries:     1,
		InitialDelayIn: time.Millisecond,
		BackoffFactor:  1,
	})
	response, err := retry(context.Background(), func() (*fantasy.Response, error) {
		return model.Generate(context.Background(), fantasy.Call{})
	})
	require.NoError(t, err)
	require.Equal(t, int32(2), attempts.Load())
	require.Equal(t, "recovered", response.Content.Text())
}

func TestStreamRejectsMalformedProtocolEvents(t *testing.T) {
	tests := map[string]string{
		"malformed frame":           `{`,
		"malformed output item":     `{"type":"response.output_item.done","item":"{"}`,
		"completed without payload": `{"type":"response.completed"}`,
	}
	for name, event := range tests {
		t.Run(name, func(t *testing.T) {
			upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := upgrader.Upgrade(w, r, nil)
				require.NoError(t, err)
				defer conn.Close()
				var request requestFrame
				require.NoError(t, conn.ReadJSON(&request))
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(event)))
			}))
			defer server.Close()

			model := &languageModel{client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)}}
			_, err := model.Generate(context.Background(), fantasy.Call{})
			require.Error(t, err)
		})
	}
}

func TestStreamSeparatesCodexReasoningSummaryItems(t *testing.T) {
	t.Parallel()

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()

		var request requestFrame
		require.NoError(t, conn.ReadJSON(&request))
		for _, event := range []string{
			`{"type":"response.reasoning_summary_part.added"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Planning the first step"}`,
			`{"type":"response.reasoning_summary_part.done"}`,
			`{"type":"response.reasoning_summary_part.added"}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Evaluating the next step"}`,
			`{"type":"response.reasoning_summary_part.done"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"opaque-1","summary":[]}}`,
			`{"type":"response.reasoning_summary_text.delta","delta":"Using the legacy item boundary"}`,
			`{"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_2","encrypted_content":"opaque-2","summary":[]}}`,
			`{"type":"response.completed","response":{}}`,
		} {
			require.NoError(t, conn.WriteMessage(websocket.TextMessage, []byte(event)))
		}
	}))
	defer server.Close()

	model := &languageModel{
		client: &client{url: strings.Replace(server.URL, "http://", "ws://", 1)},
	}
	stream, err := model.Stream(context.Background(), fantasy.Call{})
	require.NoError(t, err)

	var reasoning strings.Builder
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeReasoningDelta {
			reasoning.WriteString(part.Delta)
		}
	}
	require.Equal(t, "Planning the first step\n\nEvaluating the next step\n\nUsing the legacy item boundary", reasoning.String())
}
