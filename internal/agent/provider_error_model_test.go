package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	declarativetransport "github.com/example-git/crux/internal/providertransport/declarative"
	"github.com/stretchr/testify/require"
)

type errorMappingModel struct {
	generateErr error
	streamErr   error
}

func (m errorMappingModel) Generate(context.Context, fantasy.Call) (*fantasy.Response, error) {
	return nil, m.generateErr
}

func (m errorMappingModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	if m.streamErr == nil {
		return nil, nil
	}
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: m.streamErr})
	}, nil
}

func (m errorMappingModel) GenerateObject(context.Context, fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return nil, m.generateErr
}

func (m errorMappingModel) StreamObject(context.Context, fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return nil, m.streamErr
}

func (errorMappingModel) Provider() string { return "test" }
func (errorMappingModel) Model() string    { return "test" }

type mappedAuthenticationStreamModel struct {
	errorMappingModel
	attempts int
	lastErr  *fantasy.ProviderError
}

func (m *mappedAuthenticationStreamModel) Stream(context.Context, fantasy.Call) (fantasy.StreamResponse, error) {
	m.attempts++
	m.lastErr = &fantasy.ProviderError{
		StatusCode:   http.StatusUnprocessableEntity,
		ResponseBody: []byte(`{"error":{"code":"session_expired"}}`),
	}
	return nil, m.lastErr
}

func TestLanguageModelErrorMappingsApplyToImmediateAndStreamErrors(t *testing.T) {
	registration := providerregistry.Registration{Errors: []manifest.ErrorMapping{{
		Class: "authentication", Statuses: []int{http.StatusForbidden}, Title: "Authentication required",
	}}}
	immediate := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	streamed := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	model := mapLanguageModelErrors(errorMappingModel{generateErr: immediate, streamErr: streamed}, registration)

	_, err := model.Generate(t.Context(), fantasy.Call{})
	require.Error(t, err)
	require.True(t, immediate.AuthError)
	require.Equal(t, "Authentication required", immediate.Title)

	stream, err := model.Stream(t.Context(), fantasy.Call{})
	require.NoError(t, err)
	parts := make([]fantasy.StreamPart, 0, 1)
	stream(func(part fantasy.StreamPart) bool {
		parts = append(parts, part)
		return true
	})
	require.Len(t, parts, 1)
	require.True(t, streamed.AuthError)
	require.Equal(t, "Authentication required", streamed.Title)

	objectErr := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	objectModel := mapLanguageModelErrors(errorMappingModel{generateErr: objectErr}, registration)
	_, err = objectModel.GenerateObject(t.Context(), fantasy.ObjectCall{})
	require.Error(t, err)
	require.True(t, objectErr.AuthError)

	objectStreamErr := &fantasy.ProviderError{StatusCode: http.StatusForbidden}
	objectStreamModel := mapLanguageModelErrors(errorMappingModel{streamErr: objectStreamErr}, registration)
	_, err = objectStreamModel.StreamObject(t.Context(), fantasy.ObjectCall{})
	require.Error(t, err)
	require.True(t, objectStreamErr.AuthError)
}

func TestManifestErrorMappingTriggersOneOuterAuthenticationRefresh(t *testing.T) {
	expired := &mappedAuthenticationStreamModel{}
	registration := providerregistry.Registration{Errors: []manifest.ErrorMapping{{
		Class: "authentication", Statuses: []int{http.StatusUnprocessableEntity},
		Codes: []string{"session_expired"}, CodePointer: "/error/code",
	}}}
	mappedExpired := mapLanguageModelErrors(expired, registration)
	current := mappedExpired
	refreshCalls := 0
	modelProviderCalls := 0
	maxRetries := 0

	_, err := fantasy.NewAgent(mappedExpired).Stream(t.Context(), fantasy.AgentStreamCall{
		Prompt:     "hello",
		MaxRetries: &maxRetries,
		OnAuthRefresh: func(context.Context, *fantasy.ProviderError) error {
			refreshCalls++
			current = &finishStreamModel{text: "refreshed"}
			return nil
		},
		ModelProvider: func() fantasy.LanguageModel {
			modelProviderCalls++
			return current
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, refreshCalls)
	require.Equal(t, 2, modelProviderCalls)
	require.Equal(t, 1, expired.attempts)
	require.NotNil(t, expired.lastErr)
	require.True(t, expired.lastErr.AuthError)
	require.Equal(t, fantasy.ProviderErrorClassAuthentication, expired.lastErr.Class)
}

func TestDeclarativeSSEErrorMappingsApplyToOversizedEvents(t *testing.T) {
	body := `{"type":"error","error":{"code":"overloaded","message":"capacity unavailable"},"extra":"` + strings.Repeat("x", 1<<20) + `"}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: "+body+"\n\n")
	}))
	defer server.Close()
	registration := providerregistry.Registration{Errors: []manifest.ErrorMapping{{
		Class: "capacity", Codes: []string{"overloaded"}, CodePointer: "/error/code", MessagePointer: "/error/message",
	}}}
	provider := &declarativetransport.Provider{
		ID:         "synthetic",
		HTTPClient: server.Client(),
		Errors:     registration.Errors,
		Operation: &providertransport.Operation{
			ID:       "inference",
			Key:      providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL},
			Method:   http.MethodPost,
			Path:     "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource:      "sse-data-json",
				EventTypePointer: "/type",
				MaxEventBytes:    2 << 20,
				Mappings: []manifest.EventMapping{{
					Source: "error", Event: "error", Fields: map[string]string{"message": "/error/message"},
				}},
			},
		},
	}
	baseModel, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	model := mapLanguageModelErrors(baseModel, registration)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	parts := make([]fantasy.StreamPart, 0, 1)
	stream(func(part fantasy.StreamPart) bool {
		parts = append(parts, part)
		return true
	})
	require.Len(t, parts, 1)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, parts[0].Error, &providerErr)
	require.Equal(t, fantasy.ProviderErrorClassCapacity, providerErr.Class)
	require.Equal(t, "Provider capacity unavailable", providerErr.Title)
	require.Equal(t, "capacity unavailable", providerErr.Message)
	require.False(t, providerErr.IsRetryable())
	require.LessOrEqual(t, len(providerErr.ResponseBody), 1<<20)
	require.JSONEq(t, `{"truncated":true}`, string(providerErr.ResponseBody))
}

func TestDeclarativeHTTPErrorMappingsApplyForAllModelMethods(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("X-Provider-Trace", "trace-1")
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(response, `{"error":{"code":"context_limit","message":"selected provider message"}}`)
	}))
	defer server.Close()
	provider := &declarativetransport.Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}},
		},
	}
	baseModel, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	registration := providerregistry.Registration{Errors: []manifest.ErrorMapping{{
		Class: "authentication", Statuses: []int{http.StatusUnprocessableEntity}, Codes: []string{"context_limit"},
		CodePointer: "/error/code", MessagePointer: "/error/message", Title: "Mapped provider error",
		Retryable: true, ContextOverflow: true,
	}}}
	model := mapLanguageModelErrors(baseModel, registration)
	methods := []struct {
		name   string
		invoke func() error
	}{
		{name: "generate", invoke: func() error {
			_, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			return err
		}},
		{name: "stream", invoke: func() error {
			_, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			return err
		}},
		{name: "generate object", invoke: func() error {
			_, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			return err
		}},
		{name: "stream object", invoke: func() error {
			_, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			return err
		}},
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			err := method.invoke()
			require.Error(t, err)
			var providerErr *fantasy.ProviderError
			require.True(t, errors.As(err, &providerErr))
			require.Equal(t, http.StatusUnprocessableEntity, providerErr.StatusCode)
			require.Equal(t, "Mapped provider error", providerErr.Title)
			require.Equal(t, "selected provider message", providerErr.Message)
			require.True(t, providerErr.AuthError)
			require.True(t, providerErr.TransientError)
			require.True(t, providerErr.ContextTooLargeErr)
			require.Equal(t, "trace-1", providerErr.ResponseHeaders["X-Provider-Trace"])
			require.JSONEq(t, `{"error":{"code":"context_limit","message":"selected provider message"}}`, string(providerErr.ResponseBody))
		})
	}
}
