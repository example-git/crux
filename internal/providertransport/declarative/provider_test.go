package declarative

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestProviderMapsUsageAndValidatedMetadata(t *testing.T) {
	model := &languageModel{provider: &Provider{
		ID: "synthetic",
		Operation: &providertransport.Operation{Streaming: &manifest.StreamingPolicy{
			EventTypePointer: "/type", UnknownEvent: "error",
			Mappings: []manifest.EventMapping{
				{Source: "delta", Event: "text-delta", Fields: map[string]string{"delta": "/delta", "metadata.trace": "/trace"}, MetadataNamespace: "example.meta"},
				{Source: "finish", Event: "finish", Fields: map[string]string{"finish_reason": "/finish_reason"}},
			},
		}},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{
			{Target: "input_tokens", Pointer: "/usage/input"},
			{Target: "output_tokens", Pointer: "/usage/output"},
		}},
		Metadata: []manifest.MetadataContract{{
			Namespace: "example.meta", Version: 1, Scope: "text",
			Schema: map[string]any{"type": "object", "required": []string{"trace"}, "properties": map[string]any{"trace": map[string]any{"type": "string"}}},
		}},
	}}
	parts, err := model.mapDocument(map[string]any{"type": "delta", "delta": "hello", "trace": "trace-1"})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, "hello", parts[0].Delta)
	require.Contains(t, parts[0].ProviderMetadata, "example.meta")

	parts, err = model.mapDocument(map[string]any{"type": "finish", "finish_reason": "stop", "usage": map[string]any{"input": float64(3), "output": float64(2)}})
	require.NoError(t, err)
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.FinishReasonStop, parts[0].FinishReason)
	require.Equal(t, int64(3), parts[0].Usage.InputTokens)
	require.Equal(t, int64(2), parts[0].Usage.OutputTokens)
	require.Equal(t, int64(5), parts[0].Usage.TotalTokens)
}

func TestProviderMapsSSEErrorsToBoundedProviderErrors(t *testing.T) {
	message := strings.Repeat("x", 5000)
	model := &languageModel{provider: &Provider{ID: "synthetic", Operation: &providertransport.Operation{Streaming: &manifest.StreamingPolicy{
		EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "error", Event: "error", Fields: map[string]string{"message": "/message"}}},
	}}}}
	parts, terminal, err := model.mapDocumentEvent(map[string]any{"type": "error", "code": "capacity", "message": message}, nil)
	require.NoError(t, err)
	require.True(t, terminal)
	require.Len(t, parts, 1)
	var providerErr *fantasy.ProviderError
	require.ErrorAs(t, parts[0].Error, &providerErr)
	require.Len(t, []rune(strings.TrimPrefix(providerErr.Message, "provider synthetic: ")), 4096)
	require.JSONEq(t, `{"type":"error","code":"capacity","message":"`+message+`"}`, string(providerErr.ResponseBody))
}

func TestProviderExecutesEventConditionsAndUnknownWarnings(t *testing.T) {
	model := &languageModel{provider: &Provider{
		ID: "synthetic",
		Operation: &providertransport.Operation{Streaming: &manifest.StreamingPolicy{
			EventTypePointer: "/type", UnknownEvent: "warn",
			Mappings: []manifest.EventMapping{
				{Source: "item", Event: "text-delta", Fields: map[string]string{"delta": "/delta"}, Condition: &manifest.Predicate{Operation: "equals", Path: "/kind", Value: &manifest.Template{Kind: "literal", Value: "text"}}},
				{Source: "item", Event: "finish", Condition: &manifest.Predicate{Operation: "equals", Path: "/kind", Value: &manifest.Template{Kind: "literal", Value: "finish"}}},
			},
		}},
	}}
	parts, terminal, err := model.mapDocumentEvent(map[string]any{"type": "item", "kind": "text", "delta": "hello"}, nil)
	require.NoError(t, err)
	require.False(t, terminal)
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeTextDelta, parts[0].Type)
	require.Equal(t, "hello", parts[0].Delta)

	parts, terminal, err = model.mapDocumentEvent(map[string]any{"type": "item", "kind": "finish"}, nil)
	require.NoError(t, err)
	require.True(t, terminal)
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeFinish, parts[0].Type)

	parts, terminal, err = model.mapDocumentEvent(map[string]any{"type": "other"}, nil)
	require.NoError(t, err)
	require.False(t, terminal)
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeWarnings, parts[0].Type)
	require.Equal(t, "provider synthetic returned unknown event \"other\"", parts[0].Warnings[0].Message)
}

func TestProviderValidatesCompleteMetadataSchemaAtRuntime(t *testing.T) {
	contract := manifest.MetadataContract{
		Namespace: "example.meta", Version: 1, Scope: "message",
		Schema: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object",
			"properties": map[string]any{
				"profile": map[string]any{
					"type":       "object",
					"properties": map[string]any{"mode": map[string]any{"type": "string", "enum": []any{"fast", "safe"}}},
					"required":   []any{"mode"}, "additionalProperties": false,
				},
				"tags":  map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "minItems": 1},
				"score": map[string]any{"type": "number", "minimum": 0, "maximum": 1},
			},
			"required": []any{"profile", "tags", "score"}, "additionalProperties": false,
		},
	}
	model := &languageModel{provider: &Provider{
		ID: "synthetic",
		Operation: &providertransport.Operation{Streaming: &manifest.StreamingPolicy{
			EventTypePointer: "/type", Mappings: []manifest.EventMapping{{
				Source: "done", Event: "finish", MetadataNamespace: contract.Namespace,
				Fields: map[string]string{"metadata.profile": "/profile", "metadata.tags": "/tags", "metadata.score": "/score"},
			}},
		}},
		Metadata: []manifest.MetadataContract{contract},
	}}
	valid := map[string]any{"type": "done", "profile": map[string]any{"mode": "safe"}, "tags": []any{"one"}, "score": 0.5}
	parts, err := model.mapDocument(valid)
	require.NoError(t, err)
	require.Len(t, parts, 1)

	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{name: "nested additional property", edit: func(value map[string]any) { value["profile"].(map[string]any)["other"] = true }},
		{name: "enum", edit: func(value map[string]any) { value["profile"].(map[string]any)["mode"] = "other" }},
		{name: "array items", edit: func(value map[string]any) { value["tags"] = []any{1} }},
		{name: "array minimum", edit: func(value map[string]any) { value["tags"] = []any{} }},
		{name: "number range", edit: func(value map[string]any) { value["score"] = 2.0 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := map[string]any{"type": "done", "profile": map[string]any{"mode": "safe"}, "tags": []any{"one"}, "score": 0.5}
			test.edit(value)
			_, err := model.mapDocument(value)
			require.ErrorContains(t, err, "schema validation failed")
		})
	}
}

func TestProviderRejectsUnsupportedFramingBeforeRequest(t *testing.T) {
	requests := 0
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("unexpected request")
	})}
	for _, test := range []struct {
		name      string
		transport string
		streaming *manifest.StreamingPolicy
		want      string
	}{
		{name: "missing policy", transport: "sse", want: "requires a streaming policy using sse-data-json"},
		{name: "JSON sequence", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "json-sequence"}, want: "requires a streaming policy using sse-data-json"},
		{name: "WebSocket events", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "websocket-json"}, want: "requires a streaming policy using sse-data-json"},
		{name: "HTTP JSON policy", transport: "http-json", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json"}, want: "must not declare streaming policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := &Provider{ID: "synthetic", HTTPClient: client, Operation: &providertransport.Operation{
				ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: test.transport},
				Endpoint: manifest.Endpoint{BaseURL: "https://example.invalid"}, Method: http.MethodPost, Path: "/generate", Streaming: test.streaming,
			}}
			_, err := provider.LanguageModel(t.Context(), "model-one")
			require.ErrorContains(t, err, test.want)
			require.Zero(t, requests)
		})
	}
}

func TestProviderGeneratesThroughDeclaredProtocols(t *testing.T) {
	for _, test := range []struct {
		name     string
		protocol string
		path     string
		assert   func(*testing.T, map[string]any)
	}{
		{
			name: "generic JSON", protocol: "generic-json", path: "/v1/models/{model}:generate",
			assert: func(t *testing.T, document map[string]any) {
				require.Equal(t, "model-one", document["model"])
				messages := document["messages"].([]any)
				require.Equal(t, "user", messages[0].(map[string]any)["role"])
			},
		},
		{
			name: "Gemini content", protocol: "gemini-generate-content", path: "/v1/models/{model}:generateContent",
			assert: func(t *testing.T, document map[string]any) {
				contents := document["contents"].([]any)
				require.Equal(t, "user", contents[0].(map[string]any)["role"])
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				require.Equal(t, "/v1/models/model-one:"+map[bool]string{true: "generateContent", false: "generate"}[test.protocol == "gemini-generate-content"], request.URL.Path)
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				var document map[string]any
				require.NoError(t, json.Unmarshal(body, &document))
				test.assert(t, document)
				reasoning := document["reasoning"].(map[string]any)
				require.Equal(t, "high", reasoning["effort"])
				_, _ = io.WriteString(response, `{"text":"generated"}`)
			}))
			defer server.Close()
			provider := &Provider{
				ID: "synthetic", HTTPClient: server.Client(), RuntimeControl: map[string]any{"/reasoning/effort": "high"},
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: test.protocol, Transport: "http-json"},
					Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: test.path,
				},
			}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			require.Equal(t, "generated", result.Content.Text())
		})
	}
}

func TestProviderStreamStopsAtTransformedTerminalEvent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"raw_type\":\"finish\",\"finish_reason\":\"stop\"}\n\ndata: not-json\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			ResponseTransform: &manifest.JSONPipeline{Operations: []manifest.JSONOperation{{Operation: "copy", From: "/raw_type", Path: "/type"}}},
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{{Source: "finish", Event: "finish", Fields: map[string]string{"finish_reason": "/finish_reason"}}},
			},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeFinish, parts[0].Type)
	require.Equal(t, fantasy.FinishReasonStop, parts[0].FinishReason)
}

func TestProviderStreamExecutesAllMappingsBeforeTerminal(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"complete\",\"delta\":\"done\",\"usage\":{\"input\":4}}\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{
					{Source: "complete", Event: "finish", Fields: map[string]string{"finish_reason": "/type"}},
					{Source: "complete", Event: "text-delta", Fields: map[string]string{"delta": "/delta"}},
					{Source: "complete", Event: "usage"},
				},
			},
		},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 4)
	require.Equal(t, fantasy.StreamPartTypeTextStart, parts[0].Type)
	require.Equal(t, fantasy.StreamPartTypeTextDelta, parts[1].Type)
	require.Equal(t, "done", parts[1].Delta)
	require.Equal(t, fantasy.StreamPartTypeTextEnd, parts[2].Type)
	require.Equal(t, fantasy.StreamPartTypeFinish, parts[3].Type)
	require.Equal(t, int64(4), parts[3].Usage.InputTokens)
	require.Equal(t, int64(4), parts[3].Usage.TotalTokens)
}

func TestProviderStreamMergesUsageAndMetadataIntoMappedFinish(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"usage\",\"usage\":{\"input\":3,\"output\":2},\"request_id\":\"request-1\"}\n\ndata: {\"type\":\"finish\",\"finish_reason\":\"stop\",\"trace\":\"trace-1\"}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	metadataSchema := func(property string) map[string]any {
		return map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object",
			"properties": map[string]any{
				property: map[string]any{"type": "string"},
			},
			"required": []any{property}, "additionalProperties": false,
		}
	}
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{
					{Source: "usage", Event: "usage", MetadataNamespace: "synthetic.usage", Fields: map[string]string{"metadata.request_id": "/request_id"}},
					{Source: "finish", Event: "finish", MetadataNamespace: "synthetic.finish", Fields: map[string]string{"finish_reason": "/finish_reason", "metadata.trace": "/trace"}},
				},
			},
		},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{
			{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"},
			{Target: "output_tokens", Pointer: "/usage/output", Operation: "replace"},
		}},
		Metadata: []manifest.MetadataContract{
			{Namespace: "synthetic.usage", Version: 1, Scope: "message", Schema: metadataSchema("request_id")},
			{Namespace: "synthetic.finish", Version: 1, Scope: "message", Schema: metadataSchema("trace")},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeFinish, parts[0].Type)
	require.Equal(t, int64(3), parts[0].Usage.InputTokens)
	require.Equal(t, int64(2), parts[0].Usage.OutputTokens)
	require.Equal(t, int64(5), parts[0].Usage.TotalTokens)
	require.Contains(t, parts[0].ProviderMetadata, "synthetic.usage")
	require.Contains(t, parts[0].ProviderMetadata, "synthetic.finish")
}

func TestProviderStreamRejectsMalformedPresentUsage(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"usage\",\"usage\":{\"input\":\"private-invalid-value\"}}\n\ndata: {\"type\":\"finish\"}\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{{Source: "usage", Event: "usage"}, {Source: "finish", Event: "finish"}},
			},
		},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeError, parts[0].Type)
	require.ErrorContains(t, parts[0].Error, "usage target \"input_tokens\"")
	require.NotContains(t, parts[0].Error.Error(), "private-invalid-value")
}

func TestProviderGenerateMapsResponseUsageWithExactPresence(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr string
	}{
		{name: "missing", body: `{"text":"generated","usage":{}}`},
		{name: "explicit null", body: `{"text":"generated","usage":{"input":null}}`, wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "private string", body: `{"text":"generated","usage":{"input":"private-invalid-value"}}`, wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "fraction", body: `{"text":"generated","usage":{"input":1.5}}`, wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "negative", body: `{"text":"generated","usage":{"input":-1}}`, wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "integer", body: `{"text":"generated","usage":{"input":7}}`, want: 7},
		{name: "maximum integer", body: `{"text":"generated","usage":{"input":9223372036854775807}}`, want: int64(9223372036854775807)},
		{name: "out of range", body: `{"text":"generated","usage":{"input":9223372036854775808}}`, wantErr: `usage target "input_tokens" is not a non-negative integer`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()
			provider := &Provider{
				ID: "synthetic", HTTPClient: server.Client(),
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
					Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
				},
				Usage: &manifest.UsagePolicy{Source: "response", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}},
			}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				require.NotContains(t, err.Error(), "private-invalid-value")
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, result.Usage.InputTokens)
			require.Equal(t, test.want, result.Usage.TotalTokens)
		})
	}
}

func TestProviderResponseTransformPreservesExactUsageNumber(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(response, `{"text":"generated","usage":{"input":9223372036854775807}}`)
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			ResponseTransform: &manifest.JSONPipeline{Operations: []manifest.JSONOperation{{Operation: "copy", From: "/usage/input", Path: "/normalized/input"}}},
		},
		Usage: &manifest.UsagePolicy{Source: "response", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/normalized/input", Operation: "replace"}}},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	require.Equal(t, int64(9223372036854775807), result.Usage.InputTokens)
}

func TestProviderStreamPreservesUsagePresenceAndRange(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    int64
		wantErr string
	}{
		{name: "missing retains prior value", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":7}}\n\ndata: {\"type\":\"usage\",\"usage\":{}}\n\ndata: {\"type\":\"finish\"}\n\n", want: 7},
		{name: "explicit null", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":null}}\n\n", wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "private string", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":\"private-invalid-value\"}}\n\n", wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "maximum integer", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":9223372036854775807}}\n\ndata: {\"type\":\"finish\"}\n\n", want: int64(9223372036854775807)},
		{name: "out of range", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":9223372036854775808}}\n\n", wantErr: `usage target "input_tokens" is not a non-negative integer`},
		{name: "total overflow", body: "data: {\"type\":\"usage\",\"usage\":{\"input\":9223372036854775807,\"output\":1}}\n\n", wantErr: "usage total exceeds the supported range"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(response, test.body)
			}))
			defer server.Close()
			provider := &Provider{
				ID: "synthetic", HTTPClient: server.Client(),
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
					Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
					Streaming: &manifest.StreamingPolicy{
						EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
						Mappings: []manifest.EventMapping{{Source: "usage", Event: "usage"}, {Source: "finish", Event: "finish"}},
					},
				},
				Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{
					{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"},
					{Target: "output_tokens", Pointer: "/usage/output", Operation: "replace"},
				}},
			}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			var parts []fantasy.StreamPart
			for part := range stream {
				parts = append(parts, part)
			}
			require.Len(t, parts, 1)
			if test.wantErr != "" {
				require.Equal(t, fantasy.StreamPartTypeError, parts[0].Type)
				require.ErrorContains(t, parts[0].Error, test.wantErr)
				require.NotContains(t, parts[0].Error.Error(), "private-invalid-value")
				return
			}
			require.Equal(t, fantasy.StreamPartTypeFinish, parts[0].Type)
			require.Equal(t, test.want, parts[0].Usage.InputTokens)
			require.Equal(t, test.want, parts[0].Usage.TotalTokens)
		})
	}
}

func TestMapUsageRejectsAccumulationOverflow(t *testing.T) {
	policy := &manifest.UsagePolicy{Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "accumulate"}}}
	current := fantasy.Usage{InputTokens: int64(9223372036854775807), TotalTokens: int64(9223372036854775807)}
	result, err := mapUsage(map[string]any{"usage": map[string]any{"input": json.Number("1")}}, policy, current)
	require.ErrorContains(t, err, `usage target "input_tokens" exceeds the supported range`)
	require.Equal(t, current, result)
}

func TestProviderStreamRejectsConflictingMetadataFields(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"usage-one\",\"trace\":\"first\"}\n\ndata: {\"type\":\"usage-two\",\"trace\":\"second\"}\n\ndata: {\"type\":\"finish\"}\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{
					{Source: "usage-one", Event: "usage", MetadataNamespace: "synthetic.meta", Fields: map[string]string{"metadata.trace": "/trace"}},
					{Source: "usage-two", Event: "usage", MetadataNamespace: "synthetic.meta", Fields: map[string]string{"metadata.trace": "/trace"}},
					{Source: "finish", Event: "finish"},
				},
			},
		},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero"},
		Metadata: []manifest.MetadataContract{{
			Namespace: "synthetic.meta", Version: 1, Scope: "message",
			Schema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object", "properties": map[string]any{"trace": map[string]any{"type": "string"}}, "additionalProperties": false},
		}},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeError, parts[0].Type)
	require.ErrorContains(t, parts[0].Error, `metadata namespace "synthetic.meta" field "trace" conflicts with an earlier event`)
	require.NotContains(t, parts[0].Error.Error(), "first")
	require.NotContains(t, parts[0].Error.Error(), "second")
}

func TestMergeProviderMetadataComparesHugeJSONNumbersExactly(t *testing.T) {
	namespace := "synthetic.meta"
	baseValue := Metadata{"value": json.Number("1e1000000000")}
	equivalentValue := Metadata{"value": json.Number("10e999999999")}
	merged, err := mergeProviderMetadata(
		fantasy.ProviderMetadata{namespace: &baseValue},
		fantasy.ProviderMetadata{namespace: &equivalentValue},
	)
	require.NoError(t, err)
	require.Contains(t, merged, namespace)

	differentValue := Metadata{"value": json.Number("2e1000000000")}
	_, err = mergeProviderMetadata(
		fantasy.ProviderMetadata{namespace: &baseValue},
		fantasy.ProviderMetadata{namespace: &differentValue},
	)
	require.ErrorContains(t, err, `metadata namespace "synthetic.meta" field "value" conflicts with an earlier event`)
	require.NotContains(t, err.Error(), "1000000000")
}

func TestProviderStreamBoundsMappedErrorAndRetainsExactFields(t *testing.T) {
	message := strings.Repeat("m", 5000)
	body := `{"type":"error","error":{"code":9223372036854775807,"message":"` + message + `"},"extra":"` + strings.Repeat("x", maxProviderErrorBodyBytes) + `"}`
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: "+body+"\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Errors: []manifest.ErrorMapping{{CodePointer: "/error/code", MessagePointer: "/error/message"}},
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 2 << 20, RequireTerminal: true,
				Mappings: []manifest.EventMapping{{Source: "error", Event: "error", Fields: map[string]string{"message": "/error/message"}}, {Source: "finish", Event: "finish"}},
			},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	eventErr, ok := parts[0].Error.(*providerEventError)
	require.True(t, ok)
	code, ok := eventErr.ProviderErrorField("/error/code")
	require.True(t, ok)
	require.Equal(t, "9223372036854775807", code)
	require.Equal(t, []byte(`{"truncated":true}`), eventErr.providerError.ResponseBody)
	require.Len(t, []rune(strings.TrimPrefix(eventErr.providerError.Message, "provider synthetic: ")), 4096)
}

func TestProviderRejectsMultipleRuntimeTerminalMatches(t *testing.T) {
	model := &languageModel{provider: &Provider{
		ID: "synthetic",
		Operation: &providertransport.Operation{Streaming: &manifest.StreamingPolicy{
			EventTypePointer: "/type",
			Mappings: []manifest.EventMapping{
				{Source: "done", Event: "finish", Condition: &manifest.Predicate{Operation: "exists", Path: "/value"}},
				{Source: "done", Event: "error", Condition: &manifest.Predicate{Operation: "exists", Path: "/value"}},
			},
		}},
	}}
	_, _, err := model.mapDocumentEvent(map[string]any{"type": "done", "value": true}, nil)
	require.ErrorContains(t, err, `event "done" matched multiple terminal mappings`)
}

func TestProviderStreamRejectsDoneMarkerWithoutMappedTerminal(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: [DONE]\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", DoneMarker: "[DONE]", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{{Source: "finish", Event: "finish"}},
			},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	var parts []fantasy.StreamPart
	for part := range stream {
		parts = append(parts, part)
	}
	require.Len(t, parts, 1)
	require.Equal(t, fantasy.StreamPartTypeError, parts[0].Type)
	require.ErrorContains(t, parts[0].Error, "without a mapped finish or error event")
}

func TestProviderStreamObjectStopsAfterYieldReturnsFalse(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, "data: {\"type\":\"delta\",\"delta\":\"{\\\"ok\\\":true}\"}\n\ndata: {\"type\":\"finish\",\"finish_reason\":\"stop\"}\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{
					{Source: "delta", Event: "text-delta", Fields: map[string]string{"delta": "/delta"}},
					{Source: "finish", Event: "finish", Fields: map[string]string{"finish_reason": "/finish_reason"}},
				},
			},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	calls := 0
	stream(func(fantasy.ObjectStreamPart) bool {
		calls++
		return false
	})
	require.Equal(t, 1, calls)
}

func TestProviderGenerateExecutesRedirectAndRequestTimeoutPolicy(t *testing.T) {
	for _, follow := range []bool{false, true} {
		t.Run(map[bool]string{false: "reject redirects", true: "follow redirects"}[follow], func(t *testing.T) {
			var targetCalls atomic.Int64
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.URL.Path == "/redirect" {
					http.Redirect(response, request, "/target", http.StatusTemporaryRedirect)
					return
				}
				targetCalls.Add(1)
				_, _ = io.WriteString(response, `{"text":"generated"}`)
			}))
			defer server.Close()
			provider := &Provider{ID: "synthetic", HTTPClient: server.Client(), Operation: &providertransport.Operation{
				ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
				Endpoint: manifest.Endpoint{BaseURL: server.URL, FollowRedirects: follow}, Method: http.MethodPost, Path: "/redirect", RequestTimeout: time.Second,
			}}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			if follow {
				require.NoError(t, err)
				require.Equal(t, "generated", result.Content.Text())
				require.Equal(t, int64(1), targetCalls.Load())
				return
			}
			require.Error(t, err)
			require.Zero(t, targetCalls.Load())
		})
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer server.Close()
	provider := &Provider{ID: "synthetic", HTTPClient: server.Client(), Operation: &providertransport.Operation{
		ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
		Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate", RequestTimeout: 20 * time.Millisecond,
	}}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.ErrorContains(t, err, "Client.Timeout exceeded")
}

func TestProviderGenerateExecutesDeclaredRetryPolicy(t *testing.T) {
	attempts := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempts++
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.JSONEq(t, `{"model":"model-one","messages":[{"role":"user","content":"hello"}]}`, string(body))
		if attempts == 1 {
			response.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(response, `{"text":"generated","usage":{"input":7}}`)
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input"}}},
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Retry: manifest.RetryPolicy{MaxAttempts: 2, Statuses: []int{http.StatusTooManyRequests}, Authentication: "never", ReplayRequirement: "idempotent"},
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	require.Equal(t, "generated", result.Content.Text())
	require.Zero(t, result.Usage.InputTokens)
	require.Equal(t, 2, attempts)
}

func TestProviderHTTPJSONTransportSupportsAllMethods(t *testing.T) {
	methods := []struct {
		name   string
		invoke func(*testing.T, fantasy.LanguageModel)
	}{
		{name: "generate", invoke: func(t *testing.T, model fantasy.LanguageModel) {
			result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			require.Equal(t, `{"ok":true}`, result.Content.Text())
			require.Equal(t, int64(3), result.Usage.InputTokens)
		}},
		{name: "stream", invoke: func(t *testing.T, model fantasy.LanguageModel) {
			stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			var parts []fantasy.StreamPart
			for part := range stream {
				parts = append(parts, part)
			}
			require.Len(t, parts, 4)
			require.Equal(t, fantasy.StreamPartTypeTextStart, parts[0].Type)
			require.Equal(t, fantasy.StreamPartTypeTextDelta, parts[1].Type)
			require.Equal(t, `{"ok":true}`, parts[1].Delta)
			require.Equal(t, fantasy.StreamPartTypeTextEnd, parts[2].Type)
			require.Equal(t, fantasy.StreamPartTypeFinish, parts[3].Type)
			require.Equal(t, int64(3), parts[3].Usage.InputTokens)
		}},
		{name: "generate object", invoke: func(t *testing.T, model fantasy.LanguageModel) {
			result, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			object, ok := result.Object.(map[string]any)
			require.True(t, ok)
			require.Equal(t, true, object["ok"])
			require.Equal(t, int64(3), result.Usage.InputTokens)
		}},
		{name: "stream object", invoke: func(t *testing.T, model fantasy.LanguageModel) {
			stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
			require.NoError(t, err)
			var object map[string]any
			var finish *fantasy.ObjectStreamPart
			for part := range stream {
				switch part.Type {
				case fantasy.ObjectStreamPartTypeObject:
					object, _ = part.Object.(map[string]any)
				case fantasy.ObjectStreamPartTypeError:
					require.NoError(t, part.Error)
				case fantasy.ObjectStreamPartTypeFinish:
					value := part
					finish = &value
				}
			}
			require.Equal(t, true, object["ok"])
			require.NotNil(t, finish)
			require.Equal(t, int64(3), finish.Usage.InputTokens)
		}},
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				require.Equal(t, "application/json", request.Header.Get("Accept"))
				_, _ = io.WriteString(response, `{"text":"{\"ok\":true}","usage":{"input":3}}`)
			}))
			defer server.Close()
			provider := &Provider{
				ID: "synthetic", HTTPClient: server.Client(),
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
					Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
				},
				Usage: &manifest.UsagePolicy{Source: "response", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}},
			}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			method.invoke(t, model)
		})
	}
}

func TestProviderGenerateAggregatesDeclaredSSETransport(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "text/event-stream", request.Header.Get("Accept"))
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(response, strings.Join([]string{
			`data: {"type":"warning","message":"check request"}`,
			`data: {"type":"text","id":"text-one","delta":"answer"}`,
			`data: {"type":"reasoning-start","id":"reason-one","delta":"why "}`,
			`data: {"type":"reasoning-delta","id":"reason-one","delta":"now"}`,
			`data: {"type":"reasoning-end","id":"reason-one"}`,
			`data: {"type":"tool-start","id":"tool-one","name":"lookup"}`,
			`data: {"type":"tool-delta","id":"tool-one","delta":"{\"q\":\"x\"}"}`,
			`data: {"type":"tool-end","id":"tool-one","trace":"tool-trace"}`,
			`data: {"type":"tool-call","id":"tool-one","name":"lookup","input":"{\"q\":\"x\"}","provider_executed":true}`,
			`data: {"type":"tool-result","id":"tool-one","name":"lookup"}`,
			`data: {"type":"source","id":"source-one","source_type":"url","url":"https://example.invalid/source","title":"Source"}`,
			`data: {"type":"usage","usage":{"input":3,"output":2}}`,
			`data: {"type":"finish","finish_reason":"stop"}`,
		}, "\n\n")+"\n\n")
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
			Streaming: &manifest.StreamingPolicy{
				EventSource: "sse-data-json", EventTypePointer: "/type", RequireTerminal: true,
				Mappings: []manifest.EventMapping{
					{Source: "warning", Event: "warning", Fields: map[string]string{"message": "/message"}},
					{Source: "text", Event: "text-delta", Fields: map[string]string{"id": "/id", "delta": "/delta"}},
					{Source: "reasoning-start", Event: "reasoning-start", Fields: map[string]string{"id": "/id", "delta": "/delta"}},
					{Source: "reasoning-delta", Event: "reasoning-delta", Fields: map[string]string{"id": "/id", "delta": "/delta"}},
					{Source: "reasoning-end", Event: "reasoning-end", Fields: map[string]string{"id": "/id"}},
					{Source: "tool-start", Event: "tool-input-start", Fields: map[string]string{"id": "/id", "name": "/name"}},
					{Source: "tool-delta", Event: "tool-input-delta", Fields: map[string]string{"id": "/id", "delta": "/delta"}},
					{Source: "tool-end", Event: "tool-input-end", MetadataNamespace: "synthetic.tool", Fields: map[string]string{"id": "/id", "metadata.trace": "/trace"}},
					{Source: "tool-call", Event: "tool-call", Fields: map[string]string{"id": "/id", "name": "/name", "input": "/input", "provider_executed": "/provider_executed"}},
					{Source: "tool-result", Event: "tool-result", Fields: map[string]string{"id": "/id", "name": "/name"}},
					{Source: "source", Event: "source", Fields: map[string]string{"id": "/id", "source_type": "/source_type", "url": "/url", "title": "/title"}},
					{Source: "usage", Event: "usage"},
					{Source: "finish", Event: "finish", Fields: map[string]string{"finish_reason": "/finish_reason"}},
				},
			},
		},
		Usage: &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{
			{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"},
			{Target: "output_tokens", Pointer: "/usage/output", Operation: "replace"},
		}},
		Metadata: []manifest.MetadataContract{{
			Namespace: "synthetic.tool", Version: 1, Scope: "tool-call",
			Schema: map[string]any{"type": "object", "required": []string{"trace"}, "properties": map[string]any{"trace": map[string]any{"type": "string"}}, "additionalProperties": false},
		}},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	result, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.NoError(t, err)
	require.Equal(t, "answer", result.Content.Text())
	require.Equal(t, "why now", result.Content.ReasoningText())
	require.Equal(t, fantasy.FinishReasonStop, result.FinishReason)
	require.Equal(t, fantasy.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5}, result.Usage)
	require.Equal(t, []fantasy.CallWarning{{Type: fantasy.CallWarningTypeOther, Message: "check request"}}, result.Warnings)

	toolCalls := result.Content.ToolCalls()
	require.Len(t, toolCalls, 1)
	require.Equal(t, "tool-one", toolCalls[0].ToolCallID)
	require.Equal(t, "lookup", toolCalls[0].ToolName)
	require.JSONEq(t, `{"q":"x"}`, toolCalls[0].Input)
	require.True(t, toolCalls[0].ProviderExecuted)
	require.Contains(t, toolCalls[0].ProviderMetadata, "synthetic.tool")

	toolResults := result.Content.ToolResults()
	require.Len(t, toolResults, 1)
	require.Equal(t, "tool-one", toolResults[0].ToolCallID)
	require.Equal(t, "lookup", toolResults[0].ToolName)
	require.True(t, toolResults[0].ProviderExecuted)

	sources := result.Content.Sources()
	require.Equal(t, []fantasy.SourceContent{{
		SourceType: fantasy.SourceTypeURL,
		ID:         "source-one",
		URL:        "https://example.invalid/source",
		Title:      "Source",
	}}, sources)
}

func TestProviderAppliesExplicitControlsAfterRequestTransformsForAllMethods(t *testing.T) {
	methods := []struct {
		name   string
		invoke func(*testing.T, fantasy.LanguageModel, fantasy.ProviderOptions) error
	}{
		{name: "generate", invoke: func(t *testing.T, model fantasy.LanguageModel, options fantasy.ProviderOptions) error {
			_, err := model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}, ProviderOptions: options})
			return err
		}},
		{name: "stream", invoke: func(t *testing.T, model fantasy.LanguageModel, options fantasy.ProviderOptions) error {
			stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}, ProviderOptions: options})
			if err != nil {
				return err
			}
			for part := range stream {
				if part.Error != nil {
					return part.Error
				}
			}
			return nil
		}},
		{name: "generate object", invoke: func(t *testing.T, model fantasy.LanguageModel, options fantasy.ProviderOptions) error {
			_, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}, ProviderOptions: options})
			return err
		}},
		{name: "stream object", invoke: func(t *testing.T, model fantasy.LanguageModel, options fantasy.ProviderOptions) error {
			stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}, ProviderOptions: options})
			if err != nil {
				return err
			}
			for part := range stream {
				if part.Error != nil {
					return part.Error
				}
			}
			return nil
		}},
	}
	for _, method := range methods {
		t.Run(method.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				var document map[string]any
				require.NoError(t, json.Unmarshal(body, &document))
				controls := document["controls"].(map[string]any)
				require.Equal(t, "explicit-set", controls["set"])
				require.Equal(t, "explicit-delete", controls["delete"])
				require.Equal(t, "explicit-move", controls["move"])
				require.Equal(t, "default-move", controls["moved"])
				require.Equal(t, "explicit-value", document["value"])
				require.Equal(t, "text/event-stream", request.Header.Get("Accept"))
				response.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(response, "data: {\"type\":\"delta\",\"delta\":\"{\\\"ok\\\":true}\"}\n\ndata: {\"type\":\"finish\",\"finish_reason\":\"stop\"}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			provider := &Provider{
				ID: "synthetic", HTTPClient: server.Client(),
				RuntimeControl: map[string]any{
					"/controls/set":    "default-set",
					"/controls/delete": "default-delete",
					"/controls/move":   "default-move",
				},
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "sse"},
					Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
					RequestTransform: &manifest.JSONPipeline{MaxOperations: 4, Operations: []manifest.JSONOperation{
						{Operation: "set", Path: "/controls/set", Value: &manifest.Template{Kind: "literal", Value: "transformed-set"}},
						{Operation: "delete", Path: "/controls/delete"},
						{Operation: "move", From: "/controls/move", Path: "/controls/moved"},
						{Operation: "set", Path: "/value", Value: &manifest.Template{Kind: "literal", Value: "transformed-value"}},
					}},
					Streaming: &manifest.StreamingPolicy{
						EventSource: "sse-data-json", DoneMarker: "[DONE]", EventTypePointer: "/type",
						Mappings: []manifest.EventMapping{
							{Source: "delta", Event: "text-delta", Fields: map[string]string{"delta": "/delta"}},
							{Source: "finish", Event: "finish", Fields: map[string]string{"finish_reason": "/finish_reason"}},
						},
					},
				},
			}
			model, err := provider.LanguageModel(t.Context(), "model-one")
			require.NoError(t, err)
			options := fantasy.ProviderOptions{"synthetic": &Options{
				Values: map[string]any{"value": "explicit-value"},
				Controls: map[string]any{
					"/controls/set":    "explicit-set",
					"/controls/delete": "explicit-delete",
					"/controls/move":   "explicit-move",
				},
			}}
			require.NoError(t, method.invoke(t, model, options))
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestProviderErrorResponseIsBoundedAndSafe(t *testing.T) {
	secret := "private-provider-detail"
	server := httptest.NewTLSServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(response, secret+strings.Repeat("x", maxProviderErrorBodyBytes))
	}))
	defer server.Close()
	provider := &Provider{
		ID: "synthetic", HTTPClient: server.Client(),
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: "generic-json", Transport: "http-json"},
			Endpoint: manifest.Endpoint{BaseURL: server.URL}, Method: http.MethodPost, Path: "/generate",
		},
	}
	model, err := provider.LanguageModel(t.Context(), "model-one")
	require.NoError(t, err)
	_, err = model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")}})
	require.Error(t, err)
	var providerErr *fantasy.ProviderError
	require.True(t, errors.As(err, &providerErr))
	require.Equal(t, http.StatusUnprocessableEntity, providerErr.StatusCode)
	require.Equal(t, server.URL+"/generate", providerErr.URL)
	require.Len(t, providerErr.ResponseBody, maxProviderErrorBodyBytes)
	require.NotContains(t, providerErr.Message, secret)
	require.Equal(t, "provider synthetic request failed with HTTP 422", providerErr.Message)
}
