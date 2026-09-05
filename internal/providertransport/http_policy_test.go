package providertransport

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type policyRoundTripFunc func(*http.Request) (*http.Response, error)

func (f policyRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestPolicyTransportAppliesManifestPolicyAtNetworkBoundary(t *testing.T) {
	operation := &Operation{
		ID: "inference", Method: http.MethodPost, Path: "/v1/inference",
		Endpoint: manifest.Endpoint{BaseURL: "https://provider.example.invalid/base"},
		RequestTransform: &manifest.JSONPipeline{MaxOperations: 2, Operations: []manifest.JSONOperation{
			{Operation: "set", Path: "/configured", Value: &manifest.Template{Kind: "config", Ref: "mode"}},
			{Operation: "set", Path: "/credential", Value: &manifest.Template{Kind: "credential", Ref: "account"}},
		}},
		ResponseTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{
			{Operation: "set", Path: "/normalized", Value: &manifest.Template{Kind: "literal", Value: true}},
		}},
		PromptTransform: &manifest.PromptPipeline{Operations: []manifest.PromptOperation{
			{Operation: "prepend", Role: "developer", Text: &manifest.Template{Kind: "literal", Value: "policy"}},
		}},
		RoleMap: &manifest.RoleMap{System: "system", Developer: "system", User: "user", Assistant: "assistant", Tool: "tool", Unknown: "reject"},
	}
	transport := &PolicyTransport{
		Operation: operation,
		Values: TemplateValues{
			Config: map[string]any{"mode": "strict"}, Credentials: map[string]string{"account": "bound-secret"},
		},
		Headers:         map[string]string{"X-Policy": "active"},
		RuntimeControls: map[string]any{"/reasoning/effort": "high"},
		Base: policyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			require.Equal(t, http.MethodPost, request.Method)
			require.Equal(t, "https://provider.example.invalid/v1/inference", request.URL.String())
			require.Equal(t, "active", request.Header.Get("X-Policy"))
			body, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			replayed, err := request.GetBody()
			require.NoError(t, err)
			replayBody, err := io.ReadAll(replayed)
			require.NoError(t, err)
			require.Equal(t, body, replayBody)
			require.Equal(t, int64(len(body)), request.ContentLength)
			var document map[string]any
			require.NoError(t, json.Unmarshal(body, &document))
			require.Equal(t, "strict", document["configured"])
			require.Equal(t, "bound-secret", document["credential"])
			reasoning := document["reasoning"].(map[string]any)
			require.Equal(t, "high", reasoning["effort"])
			messages := document["messages"].([]any)
			first := messages[0].(map[string]any)
			require.Equal(t, "system", first["role"])
			require.Equal(t, "policy", first["content"])
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewBufferString(`{"result":"ok"}`)),
				Request:    request,
			}, nil
		}),
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPut, "https://ignored.invalid/original", bytes.NewBufferString(`{"messages":[{"role":"user","content":"hello"}]}`))
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	var document map[string]any
	require.NoError(t, json.Unmarshal(body, &document))
	require.Equal(t, "ok", document["result"])
	require.Equal(t, true, document["normalized"])
}

func TestPolicyTransportAppliesNativeResponsesPerCallControlsToHTTPBody(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		var document map[string]any
		if err := json.Unmarshal(body, &document); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- document
		response.Header().Set("Content-Type", "application/json")
		_, _ = response.Write([]byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"gpt-5.6","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
	}))
	defer server.Close()

	operation := &Operation{
		ID: "responses", Method: http.MethodPost, Path: "/v1/responses",
		Endpoint: manifest.Endpoint{BaseURL: server.URL},
		RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{
			Operation: "set", Path: "/vendor/mode", Value: &manifest.Template{Kind: "literal", Value: "transformed"},
		}}},
	}
	transport := &PolicyTransport{
		Operation: operation,
		RuntimeControls: map[string]any{
			"/vendor/mode":      "default",
			"/reasoning/effort": "low",
			"/output/detail":    "brief",
		},
	}
	httpClient := &http.Client{Transport: transport}
	provider, err := openai.New(
		openai.WithAPIKey("test-key"),
		openai.WithBaseURL(server.URL),
		openai.WithHTTPClient(httpClient),
		openai.WithUseResponsesAPI(),
		openai.WithResponsesAPIFunc(func(string) bool { return true }),
	)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), "gpt-5.6")
	require.NoError(t, err)

	_, err = model.Generate(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("hello")},
		ProviderOptions: fantasy.ProviderOptions{openai.Name: &openai.ResponsesProviderOptions{RuntimeControls: map[string]any{
			"/vendor/mode":      "selected",
			"/reasoning/effort": "high",
			"/output/detail":    "expanded",
		}}},
	})
	require.NoError(t, err)

	document := <-requests
	require.Equal(t, "selected", document["vendor"].(map[string]any)["mode"])
	require.Equal(t, "high", document["reasoning"].(map[string]any)["effort"])
	require.Equal(t, "expanded", document["output"].(map[string]any)["detail"])
}

func TestPolicyTransportPreservesNativeResponsesInputItems(t *testing.T) {
	operation := &Operation{
		ID: "responses", Method: http.MethodPost, Path: "/v1/responses",
		Endpoint: manifest.Endpoint{BaseURL: "https://provider.example.invalid"},
		PromptTransform: &manifest.PromptPipeline{Operations: []manifest.PromptOperation{{
			Operation: "prepend", Role: "developer", Text: &manifest.Template{Kind: "literal", Value: "policy"},
		}}},
		RoleMap: &manifest.RoleMap{System: "system", Developer: "system", User: "user", Assistant: "assistant", Tool: "tool", Unknown: "reject"},
	}
	var captured map[string]any
	transport := &PolicyTransport{Operation: operation, Base: policyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(body, &captured))
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{}`)), Request: request}, nil
	})}
	body := `{
		"model":"gpt-test",
		"input":[
			{"type":"message","id":"msg_user","role":"user","content":"hello","custom":"preserved"},
			{"type":"function_call","id":"call_item","call_id":"call_1","name":"lookup","arguments":"{\"id\":1}"},
			{"type":"function_call_output","id":"output_item","call_id":"call_1","output":"result"},
			{"type":"item_reference","id":"reference_item"},
			{"type":"message","id":"msg_assistant","role":"assistant","content":"done"}
		]
	}`
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://ignored.invalid", bytes.NewBufferString(body))
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())

	input := captured["input"].([]any)
	require.Len(t, input, 6)
	require.Equal(t, map[string]any{"role": "system", "content": "policy"}, input[0])
	require.Equal(t, "msg_user", input[1].(map[string]any)["id"])
	require.Equal(t, "preserved", input[1].(map[string]any)["custom"])
	require.Equal(t, "function_call", input[2].(map[string]any)["type"])
	require.Equal(t, "call_1", input[2].(map[string]any)["call_id"])
	require.Equal(t, "function_call_output", input[3].(map[string]any)["type"])
	require.Equal(t, "call_1", input[3].(map[string]any)["call_id"])
	require.Equal(t, "item_reference", input[4].(map[string]any)["type"])
	require.Equal(t, "reference_item", input[4].(map[string]any)["id"])
	require.Equal(t, "msg_assistant", input[5].(map[string]any)["id"])
	for _, raw := range input {
		require.NotContains(t, raw.(map[string]any), "__crux_responses_input_index")
	}
}

func TestPolicyTransportPreservesResponsesOpaqueBoundariesForAcceptedPromptTransforms(t *testing.T) {
	tests := []struct {
		name            string
		operation       manifest.PromptOperation
		wantOrder       []string
		wantUserContent []string
	}{
		{
			name: "prepend", operation: manifest.PromptOperation{Operation: "prepend", Role: "system", Text: &manifest.Template{Kind: "literal", Value: "prefix"}},
			wantOrder: []string{"system:prefix", "user_first", "call_between", "user_second"}, wantUserContent: []string{"first\nremove:first", "second\nremove:second"},
		},
		{
			name: "append", operation: manifest.PromptOperation{Operation: "append", Role: "system", Text: &manifest.Template{Kind: "literal", Value: "suffix"}},
			wantOrder: []string{"user_first", "call_between", "user_second", "system:suffix"}, wantUserContent: []string{"first\nremove:first", "second\nremove:second"},
		},
		{
			name: "insert after role", operation: manifest.PromptOperation{Operation: "insert-after-role", Role: "user", Text: &manifest.Template{Kind: "literal", Value: "inserted"}},
			wantOrder: []string{"user_first", "call_between", "user_second", "user:inserted"}, wantUserContent: []string{"first\nremove:first", "second\nremove:second", "inserted"},
		},
		{
			name: "remove lines", operation: manifest.PromptOperation{Operation: "remove-lines-with-prefix", Role: "user", Prefix: "remove:"},
			wantOrder: []string{"user_first", "call_between", "user_second"}, wantUserContent: []string{"first", "second"},
		},
		{
			name: "drop unrelated role", operation: manifest.PromptOperation{Operation: "drop-role", Role: "assistant"},
			wantOrder: []string{"user_first", "call_between", "user_second"}, wantUserContent: []string{"first\nremove:first", "second\nremove:second"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			operation := &Operation{
				ID: "responses", Method: http.MethodPost, Path: "/v1/responses",
				Endpoint:        manifest.Endpoint{BaseURL: "https://provider.example.invalid"},
				PromptTransform: &manifest.PromptPipeline{Operations: []manifest.PromptOperation{test.operation}},
			}
			var captured map[string]any
			transport := &PolicyTransport{Operation: operation, Base: policyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(request.Body)
				require.NoError(t, err)
				require.NoError(t, json.Unmarshal(body, &captured))
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{}`)), Request: request}, nil
			})}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://ignored.invalid", bytes.NewBufferString(`{
				"input":[
					{"type":"message","id":"user_first","role":"user","content":"first\nremove:first"},
					{"type":"function_call","id":"call_between","call_id":"call_1","name":"lookup","arguments":"{}"},
					{"type":"message","id":"user_second","role":"user","content":"second\nremove:second"}
				]
			}`))
			require.NoError(t, err)
			response, err := transport.RoundTrip(request)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())

			input := captured["input"].([]any)
			order := make([]string, 0, len(input))
			userContent := make([]string, 0, 3)
			for _, raw := range input {
				item := raw.(map[string]any)
				identity, _ := item["id"].(string)
				if identity == "" {
					identity = item["role"].(string) + ":" + item["content"].(string)
				}
				order = append(order, identity)
				if item["role"] == "user" {
					userContent = append(userContent, item["content"].(string))
				}
				require.NotContains(t, item, "__crux_responses_input_index")
			}
			require.Equal(t, test.wantOrder, order)
			require.Equal(t, test.wantUserContent, userContent)
		})
	}
}

func TestPolicyTransportRetriesTransformedRequestBeforeResponse(t *testing.T) {
	operation := &Operation{
		ID: "inference", Method: http.MethodPost, Path: "/v1/inference",
		Endpoint: manifest.Endpoint{BaseURL: "https://provider.example.invalid"},
		Retry: manifest.RetryPolicy{
			MaxAttempts: 2, Statuses: []int{http.StatusServiceUnavailable},
			Authentication: "never", ReplayRequirement: "before-first-event",
		},
		RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{
			Operation: "set", Path: "/policy", Value: &manifest.Template{Kind: "literal", Value: "active"},
		}}},
	}
	var bodies [][]byte
	transport := &PolicyTransport{Operation: operation, Base: policyRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		bodies = append(bodies, body)
		status := http.StatusServiceUnavailable
		if len(bodies) == 2 {
			status = http.StatusOK
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewBufferString(`{"text":"ok"}`)), Request: request}, nil
	})}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://ignored.invalid", bytes.NewBufferString(`{"input":"hello"}`))
	require.NoError(t, err)
	response, err := transport.RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Len(t, bodies, 2)
	require.Equal(t, bodies[0], bodies[1])
	require.JSONEq(t, `{"input":"hello","policy":"active"}`, string(bodies[1]))
}
