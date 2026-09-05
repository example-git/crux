package antigravity

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func TestRetryOptionProjectsImmutablePolicy(t *testing.T) {
	retry := manifest.RetryPolicy{
		MaxAttempts:       4,
		Statuses:          []int{503},
		Codes:             []string{"temporary"},
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}
	providerValue, err := New(WithRetryPolicy(retry))
	if err != nil {
		t.Fatal(err)
	}
	modelValue, err := providerValue.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatal(err)
	}
	model := modelValue.(*languageModel)
	if model.client.retry.MaxAttempts != 4 || len(model.client.retry.Statuses) != 1 || model.client.retry.Statuses[0] != 503 || len(model.client.retry.Codes) != 1 || model.client.retry.Codes[0] != "temporary" {
		t.Fatalf("retry policy = %#v", model.client.retry)
	}
	retry.Statuses[0] = 429
	retry.Codes[0] = "changed"
	if model.client.retry.Statuses[0] != 503 || model.client.retry.Codes[0] != "temporary" {
		t.Fatalf("retry policy aliases source: %#v", model.client.retry)
	}
}

func TestErrorMappingOptionProjectsImmutablePolicy(t *testing.T) {
	mappings := []manifest.ErrorMapping{{
		Class:       "capacity",
		Statuses:    []int{429},
		Codes:       []string{"capacity"},
		CodePointer: "/error/code",
		Retryable:   true,
	}}
	providerValue, err := New(WithErrorMappings(mappings))
	if err != nil {
		t.Fatal(err)
	}
	modelValue, err := providerValue.LanguageModel(t.Context(), "model")
	if err != nil {
		t.Fatal(err)
	}
	model := modelValue.(*languageModel)
	mappings[0].Class = "authentication"
	mappings[0].Statuses[0] = 401
	mappings[0].Codes[0] = "changed"
	if len(model.client.errors) != 1 || model.client.errors[0].Class != "capacity" || model.client.errors[0].Statuses[0] != 429 || model.client.errors[0].Codes[0] != "capacity" {
		t.Fatalf("error mappings alias source: %#v", model.client.errors)
	}
}

func TestGPTOSSRequestOmitsUnsupportedGenerationConfig(t *testing.T) {
	maxOutputTokens := int64(32_768)
	thinkingBudget := int64(2_000)
	includeThoughts := true
	model := &languageModel{modelID: "gpt-oss-120b-medium"}

	req, _, err := model.prepareRequest(fantasy.Call{
		Prompt: fantasy.Prompt{
			fantasy.Message{
				Role: fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{
					fantasy.TextPart{Text: "hello"},
				},
			},
		},
		MaxOutputTokens: &maxOutputTokens,
		ProviderOptions: fantasy.ProviderOptions{
			Name: &ProviderOptions{
				ThinkingConfig: &ThinkingConfig{
					ThinkingBudget:  &thinkingBudget,
					IncludeThoughts: &includeThoughts,
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if req.GenerationConfig.MaxOutputTokens != 0 {
		t.Errorf("max output tokens = %d, want omitted", req.GenerationConfig.MaxOutputTokens)
	}
	if req.GenerationConfig.TopP != nil || req.GenerationConfig.TopK != nil {
		t.Errorf("sampling config = %+v, want omitted", req.GenerationConfig)
	}
	if req.GenerationConfig.ThinkingConfig != nil {
		t.Errorf("thinking config = %+v, want omitted", req.GenerationConfig.ThinkingConfig)
	}
}

// A functionCall part carrying thoughtSignature with no preceding thought
// block must still surface the signature via a reasoning-end part, otherwise
// it is never persisted and the next turn echoes empty signatures.
func TestStreamWarnsForUnknownEvent(t *testing.T) {
	body := "data: {}\n\ndata: {\"response\":{\"candidates\":[{\"finishReason\":\"STOP\"}]}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	model := &languageModel{
		modelID:         "model",
		providerOptions: options{toolCallIDFunc: func() string { return "generated-id" }},
		client: &client{
			httpClient: srv.Client(),
			baseURL:    srv.URL,
			userAgent:  "test-ua",
		},
	}
	stream, err := model.Stream(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{fantasy.NewUserMessage("test")},
	})
	if err != nil {
		t.Fatal(err)
	}
	var warnings []fantasy.CallWarning
	var finished bool
	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeWarnings:
			warnings = append(warnings, part.Warnings...)
		case fantasy.StreamPartTypeFinish:
			finished = true
		case fantasy.StreamPartTypeError:
			t.Fatalf("stream error: %v", part.Error)
		}
	}
	if len(warnings) != 1 || warnings[0].Type != fantasy.CallWarningTypeOther || warnings[0].Message != "unrecognized Antigravity stream event" {
		t.Fatalf("warnings = %#v", warnings)
	}
	if !finished {
		t.Fatal("stream did not finish")
	}
}

func TestStreamPreservesFunctionCallOnlySignature(t *testing.T) {
	t.Parallel()

	body := "data: {\"response\":{\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[" +
		"{\"functionCall\":{\"id\":\"call-1\",\"name\":\"view\",\"args\":{}},\"thoughtSignature\":\"sig-fc\"}" +
		"]},\"finishReason\":\"STOP\"}]}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	model := &languageModel{
		modelID:         "gemini-pro-agent",
		providerOptions: options{toolCallIDFunc: func() string { return "generated-id" }},
		client: &client{
			httpClient: srv.Client(),
			baseURL:    srv.URL,
			token:      func() string { return "tok" },
			project:    func(_ context.Context, _ string) string { return "proj" },
			userAgent:  "test-ua",
		},
	}

	stream, err := model.Stream(t.Context(), fantasy.Call{
		Prompt: fantasy.Prompt{
			fantasy.Message{
				Role:    fantasy.MessageRoleUser,
				Content: []fantasy.MessagePart{fantasy.TextPart{Text: "hi"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var gotSig, gotToolID string
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			t.Fatalf("stream error: %v", part.Error)
		}
		if part.Type != fantasy.StreamPartTypeReasoningEnd {
			continue
		}
		meta, ok := part.ProviderMetadata[Name].(*ReasoningMetadata)
		if !ok {
			t.Fatalf("reasoning end missing metadata: %+v", part.ProviderMetadata)
		}
		gotSig = meta.Signature
		gotToolID = meta.ToolID
	}
	if gotSig != "sig-fc" {
		t.Errorf("signature = %q, want %q", gotSig, "sig-fc")
	}
	if gotToolID != "call-1" {
		t.Errorf("tool id = %q, want %q", gotToolID, "call-1")
	}
}
