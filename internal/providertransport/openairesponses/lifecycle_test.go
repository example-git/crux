package openairesponses

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

type lifecycleTestModel struct {
	mu                   sync.Mutex
	modelID              string
	calls                []fantasy.Call
	responses            []*fantasy.Response
	streams              []fantasy.StreamResponse
	objectCalls          []fantasy.ObjectCall
	objectResponses      []*fantasy.ObjectResponse
	objectStreams        []fantasy.ObjectStreamResponse
	objectGenerateErrors []error
	objectStreamErrors   []error
}

func (m *lifecycleTestModel) Provider() string { return openai.Name }
func (m *lifecycleTestModel) Model() string {
	if m.modelID != "" {
		return m.modelID
	}
	return "gpt-test"
}

func (m *lifecycleTestModel) Generate(_ context.Context, call fantasy.Call) (*fantasy.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	if len(m.responses) == 0 {
		return nil, errors.New("unexpected Generate call")
	}
	response := m.responses[0]
	m.responses = m.responses[1:]
	return response, nil
}

func (m *lifecycleTestModel) Stream(_ context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, call)
	stream := m.streams[0]
	m.streams = m.streams[1:]
	return stream, nil
}

func (m *lifecycleTestModel) GenerateObject(_ context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objectCalls = append(m.objectCalls, call)
	if len(m.objectGenerateErrors) > 0 {
		err := m.objectGenerateErrors[0]
		m.objectGenerateErrors = m.objectGenerateErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(m.objectResponses) == 0 {
		return nil, errors.New("unexpected GenerateObject call")
	}
	response := m.objectResponses[0]
	m.objectResponses = m.objectResponses[1:]
	return response, nil
}

func (m *lifecycleTestModel) StreamObject(_ context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objectCalls = append(m.objectCalls, call)
	if len(m.objectStreamErrors) > 0 {
		err := m.objectStreamErrors[0]
		m.objectStreamErrors = m.objectStreamErrors[1:]
		if err != nil {
			return nil, err
		}
	}
	if len(m.objectStreams) == 0 {
		return nil, errors.New("unexpected StreamObject call")
	}
	stream := m.objectStreams[0]
	m.objectStreams = m.objectStreams[1:]
	return stream, nil
}

func TestLifecycleModelExecutesPreviousResponsePerSessionAndReset(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		responseStream("resp_1", "answer one"),
		responseStream("resp_2", "answer two"),
		responseStream("resp_other", "other answer"),
		responseStream("resp_reset", "reset answer"),
	}}
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		ResponseIDPointer:    "/id",
		RequestField:         "previous_response_id",
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "before-first-event"}, policy)

	first := fantasy.Call{Prompt: testPrompt("one"), Headers: map[string]string{"x-session-id": "session-a"}}
	consumeStream(t, model, first)

	followup := fantasy.Call{
		Prompt: append(append(fantasy.Prompt{}, first.Prompt...),
			fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer one"}}},
			fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
		),
		Headers: map[string]string{"x-session-id": "session-a"},
	}
	consumeStream(t, model, followup)
	consumeStream(t, model, fantasy.Call{Prompt: testPrompt("other"), Headers: map[string]string{"x-session-id": "session-b"}})
	model.(interface{ ResetConversationChain(string) }).ResetConversationChain("session-a")
	consumeStream(t, model, followup)

	require.Len(t, inner.calls, 4)
	require.Len(t, inner.calls[0].Prompt, 2)
	firstOptions := inner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.NotNil(t, firstOptions.Store)
	require.True(t, *firstOptions.Store)
	require.Nil(t, firstOptions.PreviousResponseID)

	require.Len(t, inner.calls[1].Prompt, 1)
	require.Equal(t, fantasy.MessageRoleUser, inner.calls[1].Prompt[0].Role)
	secondOptions := inner.calls[1].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_1", *secondOptions.PreviousResponseID)

	otherOptions := inner.calls[2].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, otherOptions.PreviousResponseID)
	require.Len(t, inner.calls[2].Prompt, 2)

	resetOptions := inner.calls[3].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, resetOptions.PreviousResponseID)
	require.Len(t, inner.calls[3].Prompt, len(followup.Prompt))
}

func TestLifecycleModelSharesExactContinuationAcrossWrapperRebuilds(t *testing.T) {
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		ResponseIDPointer:    "/id",
		RequestField:         "previous_response_id",
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	store := NewContinuationStore()
	firstCall := fantasy.Call{
		Prompt:  testPrompt("one"),
		Headers: map[string]string{"x-session-id": "session-a", "x-request-purpose": "conversation"},
	}
	followup := fantasy.Call{
		Prompt: append(append(fantasy.Prompt{}, firstCall.Prompt...),
			fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer one"}}},
			fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
		),
		Headers: map[string]string{"x-session-id": "session-a", "x-request-purpose": "conversation"},
	}

	firstInner := &lifecycleTestModel{streams: []fantasy.StreamResponse{responseStream("resp_1", "answer one")}}
	consumeStream(t, NewLifecycleModelWithStore(firstInner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, "owner-a"), firstCall)

	rebuiltInner := &lifecycleTestModel{streams: []fantasy.StreamResponse{responseStream("resp_2", "answer two")}}
	rebuiltModel := NewLifecycleModelWithStore(rebuiltInner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, "owner-a")
	consumeStream(t, rebuiltModel, followup)
	rebuiltOptions := rebuiltInner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_1", *rebuiltOptions.PreviousResponseID)
	require.Len(t, rebuiltInner.calls[0].Prompt, 1)

	for _, test := range []struct {
		name    string
		owner   string
		modelID string
		purpose string
	}{
		{name: "owner", owner: "owner-b", modelID: "gpt-test", purpose: "conversation"},
		{name: "model", owner: "owner-a", modelID: "gpt-other", purpose: "conversation"},
		{name: "purpose", owner: "owner-a", modelID: "gpt-test", purpose: "summary"},
	} {
		t.Run(test.name, func(t *testing.T) {
			inner := &lifecycleTestModel{modelID: test.modelID, streams: []fantasy.StreamResponse{responseStream("resp_isolated", "isolated")}}
			call := followup
			call.Headers = map[string]string{"x-session-id": "session-a", "x-request-purpose": test.purpose}
			consumeStream(t, NewLifecycleModelWithStore(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, test.owner), call)
			options := inner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
			require.Nil(t, options.PreviousResponseID)
			require.Len(t, inner.calls[0].Prompt, len(followup.Prompt))
		})
	}

	rebuiltModel.(interface{ ResetConversationChain(string) }).ResetConversationChain("session-a")
	resetInner := &lifecycleTestModel{streams: []fantasy.StreamResponse{responseStream("resp_reset", "reset")}}
	consumeStream(t, NewLifecycleModelWithStore(resetInner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, "owner-a"), followup)
	resetOptions := resetInner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, resetOptions.PreviousResponseID)

	resetSummaryInner := &lifecycleTestModel{streams: []fantasy.StreamResponse{responseStream("resp_summary_reset", "summary reset")}}
	resetSummaryCall := followup
	resetSummaryCall.Headers = map[string]string{"x-session-id": "session-a", "x-request-purpose": "summary"}
	consumeStream(t, NewLifecycleModelWithStore(resetSummaryInner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, "owner-a"), resetSummaryCall)
	resetSummaryOptions := resetSummaryInner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, resetSummaryOptions.PreviousResponseID)

	store.ResetConversation("session-a")
	store.Close()
	closedInner := &lifecycleTestModel{streams: []fantasy.StreamResponse{responseStream("resp_closed", "closed")}}
	consumeStream(t, NewLifecycleModelWithStore(closedInner, manifest.RetryPolicy{MaxAttempts: 1}, policy, store, "owner-a"), followup)
	closedOptions := closedInner.calls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, closedOptions.PreviousResponseID)
}

func TestContinuationStoreResetDetachesInFlightChain(t *testing.T) {
	store := NewContinuationStore()
	key := continuationKey{owner: "owner", model: "model", conversation: "session", purpose: "conversation"}
	inFlight := store.chain(key)
	store.ResetConversation(key.conversation)
	fresh := store.chain(key)
	require.NotSame(t, inFlight, fresh)

	inFlight.state = ContinuationState{responseID: "stale"}
	require.Same(t, fresh, store.chain(key))
	require.Empty(t, fresh.state.responseID)

	store.Close()
	require.NotSame(t, store.chain(key), store.chain(key))
}

func TestLifecycleModelExecutesToolCodecAcrossStreamAndContinuation(t *testing.T) {
	codec := &manifest.ToolCodec{
		Aliases:    []manifest.ToolAlias{{Host: "view", Provider: "read_file"}},
		Parameters: []manifest.ParameterMap{{Tool: "view", Host: "file_path", Provider: "path"}},
		Surfaces:   []string{"definitions", "prompt-references", "history-calls", "stream-events"},
	}
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		ResponseIDPointer:    "/id",
		RequestField:         "previous_response_id",
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		toolCallStream("resp_tool", "read_file", `{"path":"one.txt"}`),
		responseStream("resp_followup", "done"),
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy, codec)
	choice := fantasy.SpecificToolChoice("view")
	tool := fantasy.FunctionTool{
		Name:        "view",
		Description: "Use `view`.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"file_path": map[string]any{"type": "string"}},
			"required":   []string{"file_path"},
		},
	}
	first := fantasy.Call{
		Prompt: fantasy.Prompt{
			{Role: fantasy.MessageRoleSystem, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Call `view`."}}},
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "old", ToolName: "view", Input: `{"file_path":"old.txt"}`}}},
			{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Open one.txt"}}},
		},
		Tools:      []fantasy.Tool{tool},
		ToolChoice: &choice,
		Headers:    map[string]string{"x-session-id": "codec-session"},
	}
	stream, err := model.Stream(t.Context(), first)
	require.NoError(t, err)
	var generated fantasy.StreamPart
	for part := range stream {
		require.NotEqual(t, fantasy.StreamPartTypeError, part.Type)
		if part.Type == fantasy.StreamPartTypeToolCall {
			generated = part
		}
	}
	require.Equal(t, "view", generated.ToolCallName)
	require.JSONEq(t, `{"file_path":"one.txt"}`, generated.ToolCallInput)

	require.Len(t, inner.calls, 1)
	providerCall := inner.calls[0]
	require.Equal(t, "Call `read_file`.", providerCall.Prompt[0].Content[0].(fantasy.TextPart).Text)
	providerHistory := providerCall.Prompt[1].Content[0].(fantasy.ToolCallPart)
	require.Equal(t, "read_file", providerHistory.ToolName)
	require.JSONEq(t, `{"path":"old.txt"}`, providerHistory.Input)
	providerTool := providerCall.Tools[0].(fantasy.FunctionTool)
	require.Equal(t, "read_file", providerTool.Name)
	require.Equal(t, "Use `read_file`.", providerTool.Description)
	require.Contains(t, providerTool.InputSchema["properties"], "path")
	require.Equal(t, []any{"path"}, providerTool.InputSchema["required"])
	require.Equal(t, fantasy.SpecificToolChoice("read_file"), *providerCall.ToolChoice)

	require.Equal(t, "Call `view`.", first.Prompt[0].Content[0].(fantasy.TextPart).Text)
	require.Equal(t, "view", first.Prompt[1].Content[0].(fantasy.ToolCallPart).ToolName)
	require.Contains(t, tool.InputSchema["properties"], "file_path")
	require.NotContains(t, tool.InputSchema["properties"], "path")
	require.Equal(t, fantasy.SpecificToolChoice("view"), *first.ToolChoice)

	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "call", ToolName: "view", Input: `{"file_path":"one.txt"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Continue"}}},
	)
	consumeStream(t, model, followup)
	require.Len(t, inner.calls, 2)
	options := inner.calls[1].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_tool", *options.PreviousResponseID)
	require.Len(t, inner.calls[1].Prompt, 1)
}

func TestLifecycleModelToolCodecGeneratedResponseAndSurfaceIsolation(t *testing.T) {
	codec := &manifest.ToolCodec{
		Aliases:         []manifest.ToolAlias{{Host: "view", Provider: "read_file"}},
		Parameters:      []manifest.ParameterMap{{Tool: "view", Host: "file_path", Provider: "path"}},
		Surfaces:        []string{"stream-events"},
		CaseFoldInbound: true,
	}
	inner := &lifecycleTestModel{responses: []*fantasy.Response{{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call", ToolName: "READ_FILE", Input: `{"path":"one.txt"}`},
	}}}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, nil, codec)
	tool := fantasy.FunctionTool{Name: "view", InputSchema: map[string]any{"type": "object"}}
	choice := fantasy.SpecificToolChoice("view")
	response, err := model.Generate(t.Context(), fantasy.Call{Tools: []fantasy.Tool{tool}, ToolChoice: &choice})
	require.NoError(t, err)
	generated := response.Content[0].(fantasy.ToolCallContent)
	require.Equal(t, "view", generated.ToolName)
	require.JSONEq(t, `{"file_path":"one.txt"}`, generated.Input)
	require.Equal(t, "view", inner.calls[0].Tools[0].(fantasy.FunctionTool).Name)
	require.Equal(t, fantasy.SpecificToolChoice("view"), *inner.calls[0].ToolChoice)

	inner = &lifecycleTestModel{responses: []*fantasy.Response{{Content: fantasy.ResponseContent{
		fantasy.ToolCallContent{ToolCallID: "call", ToolName: "read_file", Input: `not-json`},
	}}}}
	model = NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, nil, codec)
	_, err = model.Generate(t.Context(), fantasy.Call{})
	require.ErrorContains(t, err, `tool "view" input is not valid JSON`)
}

func TestLifecycleModelEmitsContinuationMetadataUnderDeclaredNamespace(t *testing.T) {
	const namespace = "synthetic.responses.continuation"
	policy := &manifest.ContinuationPolicy{
		Mode:              "previous-response",
		MetadataNamespace: namespace,
		Store:             "required",
		Fallback:          "full-replay",
	}
	inner := &lifecycleTestModel{
		responses: []*fantasy.Response{{
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_generate"},
			},
		}},
		streams: []fantasy.StreamResponse{responseStream("resp_stream", "done")},
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy)

	response, err := model.Generate(t.Context(), fantasy.Call{Prompt: testPrompt("generate"), Headers: map[string]string{"x-session-id": "generate-session"}})
	require.NoError(t, err)
	require.Equal(t, "resp_generate", metadataResponseID(t, response.ProviderMetadata, namespace))
	require.Equal(t, "resp_generate", metadataResponseID(t, response.ProviderMetadata, openai.Name))

	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: testPrompt("stream"), Headers: map[string]string{"x-session-id": "stream-session"}})
	require.NoError(t, err)
	var finish fantasy.StreamPart
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeFinish {
			finish = part
		}
	}
	require.Equal(t, "resp_stream", metadataResponseID(t, finish.ProviderMetadata, namespace))
	require.Equal(t, "resp_stream", metadataResponseID(t, finish.ProviderMetadata, openai.Name))
}

func TestLifecycleModelExecutesObjectContinuationCodecAndMetadata(t *testing.T) {
	const namespace = "synthetic.responses.continuation"
	codec := &manifest.ToolCodec{
		Aliases:    []manifest.ToolAlias{{Host: "view", Provider: "read_file"}},
		Parameters: []manifest.ParameterMap{{Tool: "view", Host: "file_path", Provider: "path"}},
		Surfaces:   []string{"definitions", "prompt-references", "history-calls", "stream-events"},
	}
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		MetadataNamespace:    namespace,
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	inner := &lifecycleTestModel{objectResponses: []*fantasy.ObjectResponse{
		{
			Object:  map[string]any{"value": "one"},
			RawText: `{"value":"one"}`,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_1"},
			},
		},
		{
			Object:  map[string]any{"value": "two"},
			RawText: `{"value":"two"}`,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_2"},
			},
		},
		{
			Object:  map[string]any{"value": 3},
			RawText: `{"value":3}`,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_3"},
			},
		},
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy, codec)
	schema := fantasy.Schema{
		Type:       "object",
		Properties: map[string]*fantasy.Schema{"value": {Type: "string"}},
		Required:   []string{"value"},
	}
	first := fantasy.ObjectCall{
		Prompt: fantasy.Prompt{
			{Role: fantasy.MessageRoleSystem, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Call `view`."}}},
			{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.ToolCallPart{ToolCallID: "old", ToolName: "view", Input: `{"file_path":"old.txt"}`}}},
			{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Generate one"}}},
		},
		Schema:     schema,
		SchemaName: "structured_response",
		Headers:    map[string]string{"x-session-id": "object-session"},
	}
	firstResponse, err := model.GenerateObject(t.Context(), first)
	require.NoError(t, err)
	require.Equal(t, "resp_object_1", metadataResponseID(t, firstResponse.ProviderMetadata, namespace))

	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: `{"value":"one"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Generate two"}}},
	)
	secondResponse, err := model.GenerateObject(t.Context(), followup)
	require.NoError(t, err)
	require.Equal(t, "resp_object_2", metadataResponseID(t, secondResponse.ProviderMetadata, namespace))

	changedSchema := followup
	changedSchema.Schema.Properties["value"] = &fantasy.Schema{Type: "integer"}
	changedSchema.Prompt = append(append(fantasy.Prompt{}, followup.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: `{"value":"two"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "Generate three"}}},
	)
	thirdResponse, err := model.GenerateObject(t.Context(), changedSchema)
	require.NoError(t, err)
	require.Equal(t, "resp_object_3", metadataResponseID(t, thirdResponse.ProviderMetadata, namespace))

	require.Len(t, inner.objectCalls, 3)
	require.Equal(t, "Call `read_file`.", inner.objectCalls[0].Prompt[0].Content[0].(fantasy.TextPart).Text)
	providerHistory := inner.objectCalls[0].Prompt[1].Content[0].(fantasy.ToolCallPart)
	require.Equal(t, "read_file", providerHistory.ToolName)
	require.JSONEq(t, `{"path":"old.txt"}`, providerHistory.Input)
	firstOptions := inner.objectCalls[0].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.True(t, *firstOptions.Store)
	require.Nil(t, firstOptions.PreviousResponseID)
	require.Len(t, inner.objectCalls[1].Prompt, 1)
	secondOptions := inner.objectCalls[1].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_object_1", *secondOptions.PreviousResponseID)
	thirdOptions := inner.objectCalls[2].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Nil(t, thirdOptions.PreviousResponseID)
	require.Len(t, inner.objectCalls[2].Prompt, len(changedSchema.Prompt))
}

func TestLifecycleModelSharesHostStateAcrossGenerateAndStreamVariants(t *testing.T) {
	const namespace = "synthetic.responses.continuation"
	usage := fantasy.Usage{InputTokens: 3, OutputTokens: 2, TotalTokens: 5, CacheReadTokens: 1}
	policy := &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		MetadataNamespace:    namespace,
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	inner := &lifecycleTestModel{
		responses: []*fantasy.Response{{
			Content: fantasy.ResponseContent{fantasy.TextContent{Text: "answer one"}},
			Usage:   usage,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_generate"},
			},
		}},
		streams: []fantasy.StreamResponse{func(yield func(fantasy.StreamPart) bool) {
			if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) ||
				!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: "answer two"}) ||
				!yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"}) {
				return
			}
			yield(fantasy.StreamPart{
				Type:  fantasy.StreamPartTypeFinish,
				Usage: usage,
				ProviderMetadata: fantasy.ProviderMetadata{
					openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_stream"},
				},
			})
		}},
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1}, policy)
	first := fantasy.Call{Prompt: testPrompt("one"), Headers: map[string]string{"x-session-id": "text-session", "x-request-purpose": "conversation"}}
	response, err := model.Generate(t.Context(), first)
	require.NoError(t, err)
	require.Equal(t, usage, response.Usage)
	require.Equal(t, "resp_generate", metadataResponseID(t, response.ProviderMetadata, namespace))

	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer one"}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
	)
	stream, err := model.Stream(t.Context(), followup)
	require.NoError(t, err)
	var finish fantasy.StreamPart
	for part := range stream {
		require.NotEqual(t, fantasy.StreamPartTypeError, part.Type)
		if part.Type == fantasy.StreamPartTypeFinish {
			finish = part
		}
	}
	require.Equal(t, usage, finish.Usage)
	require.Equal(t, "resp_stream", metadataResponseID(t, finish.ProviderMetadata, namespace))
	streamOptions := inner.calls[1].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_generate", *streamOptions.PreviousResponseID)
	require.Len(t, inner.calls[1].Prompt, 1)

	objectInner := &lifecycleTestModel{
		objectResponses: []*fantasy.ObjectResponse{{
			Object:  map[string]any{"value": "one"},
			RawText: `{"value":"one"}`,
			Usage:   usage,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_generate"},
			},
		}},
		objectStreams: []fantasy.ObjectStreamResponse{func(yield func(fantasy.ObjectStreamPart) bool) {
			if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeObject, Object: map[string]any{"value": "two"}}) {
				return
			}
			yield(fantasy.ObjectStreamPart{
				Type:  fantasy.ObjectStreamPartTypeFinish,
				Usage: usage,
				ProviderMetadata: fantasy.ProviderMetadata{
					openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_stream"},
				},
			})
		}},
	}
	objectModel := NewLifecycleModel(objectInner, manifest.RetryPolicy{MaxAttempts: 1}, policy)
	schema := fantasy.Schema{Type: "object", Properties: map[string]*fantasy.Schema{"value": {Type: "string"}}, Required: []string{"value"}}
	firstObject := fantasy.ObjectCall{Prompt: testPrompt("one"), Schema: schema, SchemaName: "response", Headers: map[string]string{"x-session-id": "object-session", "x-request-purpose": "conversation"}}
	objectResponse, err := objectModel.GenerateObject(t.Context(), firstObject)
	require.NoError(t, err)
	require.Equal(t, usage, objectResponse.Usage)

	followupObject := firstObject
	followupObject.Prompt = append(append(fantasy.Prompt{}, firstObject.Prompt...),
		fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: `{"value":"one"}`}}},
		fantasy.Message{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "two"}}},
	)
	objectStream, err := objectModel.StreamObject(t.Context(), followupObject)
	require.NoError(t, err)
	var objectFinish fantasy.ObjectStreamPart
	for part := range objectStream {
		require.NotEqual(t, fantasy.ObjectStreamPartTypeError, part.Type)
		if part.Type == fantasy.ObjectStreamPartTypeFinish {
			objectFinish = part
		}
	}
	require.Equal(t, usage, objectFinish.Usage)
	require.Equal(t, "resp_object_stream", metadataResponseID(t, objectFinish.ProviderMetadata, namespace))
	objectOptions := objectInner.objectCalls[1].ProviderOptions[openai.Name].(*openai.ResponsesProviderOptions)
	require.Equal(t, "resp_object_generate", *objectOptions.PreviousResponseID)
	require.Len(t, objectInner.objectCalls[1].Prompt, 1)
}

func TestLifecycleModelRetriesObjectCallsAndEmitsStreamMetadata(t *testing.T) {
	const namespace = "synthetic.responses.continuation"
	policy := &manifest.ContinuationPolicy{
		Mode:              "previous-response",
		MetadataNamespace: namespace,
		Store:             "required",
		Fallback:          "full-replay",
	}
	inner := &lifecycleTestModel{
		objectGenerateErrors: []error{fantasy.NewIncompleteStreamError(), nil},
		objectResponses: []*fantasy.ObjectResponse{{
			Object:  map[string]any{"value": "generated"},
			RawText: `{"value":"generated"}`,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: "resp_object_generate"},
			},
		}},
		objectStreams: []fantasy.ObjectStreamResponse{
			objectErrorStream(fantasy.NewIncompleteStreamError()),
			objectResponseStream("resp_object_stream", map[string]any{"value": "streamed"}),
		},
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       2,
		UnexpectedEOF:     true,
		ReplayRequirement: "before-first-event",
	}, policy)
	call := fantasy.ObjectCall{
		Prompt:  testPrompt("object"),
		Schema:  fantasy.Schema{Type: "object"},
		Headers: map[string]string{"x-session-id": "object-generate-session"},
	}
	response, err := model.GenerateObject(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, "resp_object_generate", metadataResponseID(t, response.ProviderMetadata, namespace))
	require.Len(t, inner.objectCalls, 2)

	call.Headers = map[string]string{"x-session-id": "object-stream-session"}
	stream, err := model.StreamObject(t.Context(), call)
	require.NoError(t, err)
	var finish fantasy.ObjectStreamPart
	for part := range stream {
		require.NotEqual(t, fantasy.ObjectStreamPartTypeError, part.Type)
		if part.Type == fantasy.ObjectStreamPartTypeFinish {
			finish = part
		}
	}
	require.Equal(t, "resp_object_stream", metadataResponseID(t, finish.ProviderMetadata, namespace))
	require.Len(t, inner.objectCalls, 4)
}

func TestLifecycleModelRetriesUnexpectedEOFOnlyBeforeFirstEvent(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fantasy.NewIncompleteStreamError()})
		},
		responseStream("resp_retry", "retried"),
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       2,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}, nil)
	agent := fantasy.NewAgent(model)
	maxRetries := 0
	result, err := agent.Stream(t.Context(), fantasy.AgentStreamCall{
		Prompt:     "retry",
		Headers:    map[string]string{"x-session-id": "retry-session"},
		MaxRetries: &maxRetries,
	})
	require.NoError(t, err)
	require.Equal(t, "retried", result.Response.Content.Text())
	require.Len(t, inner.calls, 2)

	inner = &lifecycleTestModel{streams: []fantasy.StreamResponse{
		func(yield func(fantasy.StreamPart) bool) {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"})
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: &fantasy.ProviderError{Cause: io.ErrUnexpectedEOF}})
		},
	}}
	model = NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       2,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}, nil)
	agent = fantasy.NewAgent(model)
	_, err = agent.Stream(t.Context(), fantasy.AgentStreamCall{Prompt: "do not replay", MaxRetries: &maxRetries})
	require.Error(t, err)
	require.Len(t, inner.calls, 1)
	require.False(t, errors.Is(err, io.ErrUnexpectedEOF))
}

func TestLifecycleModelBoundsUnexpectedEOFAttempts(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		errorStream(fantasy.NewIncompleteStreamError()),
		errorStream(fantasy.NewIncompleteStreamError()),
		errorStream(fantasy.NewIncompleteStreamError()),
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       3,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}, nil)
	maxRetries := 0
	_, err := fantasy.NewAgent(model).Stream(t.Context(), fantasy.AgentStreamCall{Prompt: "retry", MaxRetries: &maxRetries})
	require.Error(t, err)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.Len(t, inner.calls, 3)
}

func TestLifecycleModelDoesNotReplayExhaustedStatusErrors(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		errorStream(&fantasy.ProviderError{StatusCode: 503, Title: "unavailable", Message: "try later"}),
		responseStream("unexpected", "unexpected replay"),
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       3,
		Statuses:          []int{503},
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}, nil)
	maxRetries := 0
	_, err := fantasy.NewAgent(model).Stream(t.Context(), fantasy.AgentStreamCall{Prompt: "do not retry", MaxRetries: &maxRetries})
	require.Error(t, err)
	require.Len(t, inner.calls, 1)
}

func TestLifecycleModelPreservesProviderErrorForAuthRefresh(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{
		errorStream(&fantasy.ProviderError{StatusCode: 401, Title: "authentication", Message: "expired"}),
		responseStream("after_refresh", "refreshed"),
	}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       3,
		Authentication:    "refresh-once",
		ReplayRequirement: "before-first-event",
	}, nil)
	maxRetries := 0
	refreshCalls := 0
	result, err := fantasy.NewAgent(model).Stream(t.Context(), fantasy.AgentStreamCall{
		Prompt:     "refresh",
		MaxRetries: &maxRetries,
		OnAuthRefresh: func(_ context.Context, err *fantasy.ProviderError) error {
			refreshCalls++
			require.Equal(t, 401, err.StatusCode)
			return nil
		},
	})
	require.NoError(t, err)
	require.Equal(t, "refreshed", result.Response.Content.Text())
	require.Equal(t, 1, refreshCalls)
	require.Len(t, inner.calls, 2)
}

func TestLifecycleModelDoesNotRejectObjectCallsByRetryAuthentication(t *testing.T) {
	inner := &lifecycleTestModel{
		objectResponses: []*fantasy.ObjectResponse{{Object: map[string]any{"value": "generated"}}},
		objectStreams:   []fantasy.ObjectStreamResponse{objectResponseStream("resp_object", map[string]any{"value": "streamed"})},
	}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{MaxAttempts: 1, Authentication: "refresh-once"}, nil)

	_, err := model.GenerateObject(t.Context(), fantasy.ObjectCall{})
	require.NoError(t, err)
	stream, err := model.StreamObject(t.Context(), fantasy.ObjectCall{})
	require.NoError(t, err)
	for range stream {
	}
	require.Len(t, inner.objectCalls, 2)
}

func TestLifecycleModelEOFBackoffHonorsCancellation(t *testing.T) {
	inner := &lifecycleTestModel{streams: []fantasy.StreamResponse{errorStream(fantasy.NewIncompleteStreamError())}}
	model := NewLifecycleModel(inner, manifest.RetryPolicy{
		MaxAttempts:       2,
		InitialDelayMS:    60_000,
		UnexpectedEOF:     true,
		Authentication:    "never",
		ReplayRequirement: "before-first-event",
	}, nil)
	ctx, cancel := context.WithCancel(t.Context())
	stream, err := model.Stream(ctx, fantasy.Call{})
	require.NoError(t, err)
	cancel()
	var streamErr error
	for part := range stream {
		if part.Type == fantasy.StreamPartTypeError {
			streamErr = part.Error
		}
	}
	require.ErrorIs(t, streamErr, context.Canceled)
	require.Len(t, inner.calls, 1)
}

func testPrompt(text string) fantasy.Prompt {
	return fantasy.Prompt{
		{Role: fantasy.MessageRoleSystem, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "instructions"}}},
		{Role: fantasy.MessageRoleUser, Content: []fantasy.MessagePart{fantasy.TextPart{Text: text}}},
	}
}

func errorStream(err error) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
	}
}

func toolCallStream(responseID, toolName, input string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputStart, ID: "call", ToolCallName: toolName}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: "call", ToolCallName: toolName, ToolCallInput: input}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonToolCalls,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: responseID},
			},
		})
	}
}

func responseStream(responseID, text string) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: "text"}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: "text", Delta: text}) {
			return
		}
		if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: "text"}) {
			return
		}
		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			FinishReason: fantasy.FinishReasonStop,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: responseID},
			},
		})
	}
}

func metadataResponseID(t *testing.T, metadata fantasy.ProviderMetadata, namespace string) string {
	t.Helper()
	value, ok := metadata[namespace].(*openai.ResponsesProviderMetadata)
	require.True(t, ok)
	return value.ResponseID
}

func objectErrorStream(err error) fantasy.ObjectStreamResponse {
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: err})
	}
}

func objectResponseStream(responseID string, object any) fantasy.ObjectStreamResponse {
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeObject, Object: object}) {
			return
		}
		yield(fantasy.ObjectStreamPart{
			Type: fantasy.ObjectStreamPartTypeFinish,
			ProviderMetadata: fantasy.ProviderMetadata{
				openai.Name: &openai.ResponsesProviderMetadata{ResponseID: responseID},
			},
		})
	}
}

func consumeStream(t *testing.T, model fantasy.LanguageModel, call fantasy.Call) {
	t.Helper()
	stream, err := model.Stream(t.Context(), call)
	require.NoError(t, err)
	for part := range stream {
		require.NotEqual(t, fantasy.StreamPartTypeError, part.Type)
	}
}
