package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/foundation/schema"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

type LifecycleModel struct {
	inner          fantasy.LanguageModel
	retry          manifest.RetryPolicy
	errors         []manifest.ErrorMapping
	operationRetry bool
	continuation   *manifest.ContinuationPolicy
	toolCodec      *manifest.ToolCodec
	store          *ContinuationStore
	owner          string
}

type continuationChain struct {
	mu    sync.Mutex
	state ContinuationState
}

func NewLifecycleModel(inner fantasy.LanguageModel, retry manifest.RetryPolicy, continuation *manifest.ContinuationPolicy, codecs ...*manifest.ToolCodec) fantasy.LanguageModel {
	return NewLifecycleModelWithStore(inner, retry, continuation, NewContinuationStore(), "standalone", codecs...)
}

func NewLifecycleModelWithStore(inner fantasy.LanguageModel, retry manifest.RetryPolicy, continuation *manifest.ContinuationPolicy, store *ContinuationStore, owner string, codecs ...*manifest.ToolCodec) fantasy.LanguageModel {
	return newLifecycleModel(inner, retry, continuation, nil, false, store, owner, codecs...)
}

func NewLifecycleModelWithErrorMappingsAndStore(inner fantasy.LanguageModel, retry manifest.RetryPolicy, continuation *manifest.ContinuationPolicy, mappings []manifest.ErrorMapping, store *ContinuationStore, owner string, codecs ...*manifest.ToolCodec) fantasy.LanguageModel {
	return newLifecycleModel(inner, retry, continuation, mappings, true, store, owner, codecs...)
}

func newLifecycleModel(inner fantasy.LanguageModel, retry manifest.RetryPolicy, continuation *manifest.ContinuationPolicy, mappings []manifest.ErrorMapping, operationRetry bool, store *ContinuationStore, owner string, codecs ...*manifest.ToolCodec) fantasy.LanguageModel {
	if inner == nil {
		return nil
	}
	if store == nil {
		store = NewContinuationStore()
	}
	var policy *manifest.ContinuationPolicy
	if continuation != nil {
		value := *continuation
		policy = &value
	}
	var codec *manifest.ToolCodec
	if len(codecs) > 0 && codecs[0] != nil {
		value := *codecs[0]
		value.Aliases = append([]manifest.ToolAlias(nil), codecs[0].Aliases...)
		value.PrefixAliases = append([]manifest.ToolPrefixAlias(nil), codecs[0].PrefixAliases...)
		value.Parameters = append([]manifest.ParameterMap(nil), codecs[0].Parameters...)
		value.Surfaces = append([]string(nil), codecs[0].Surfaces...)
		codec = &value
	}
	errorMappings := make([]manifest.ErrorMapping, len(mappings))
	for index, mapping := range mappings {
		mapping.Statuses = slices.Clone(mapping.Statuses)
		mapping.Codes = slices.Clone(mapping.Codes)
		errorMappings[index] = mapping
	}
	return &LifecycleModel{inner: inner, retry: retry, errors: errorMappings, operationRetry: operationRetry, continuation: policy, toolCodec: codec, store: store, owner: owner}
}

func (m *LifecycleModel) Provider() string {
	return m.inner.Provider()
}

func (m *LifecycleModel) Model() string {
	return m.inner.Model()
}

func (m *LifecycleModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	prepared, request, chain, err := m.prepare(call)
	if err != nil {
		return nil, err
	}
	if chain != nil {
		defer chain.mu.Unlock()
	}
	prepared, err = encodeToolCodecCall(prepared, m.toolCodec)
	if err != nil {
		return nil, err
	}
	attempts := retryAttempts(m.retry)
	var response *fantasy.Response
	for attempt := 1; attempt <= attempts; attempt++ {
		response, err = m.inner.Generate(ctx, prepared)
		if err == nil || !m.retryable(err, false) || attempt == attempts {
			break
		}
		if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, RetryableError(m.retry, err, false)
	}
	if response == nil {
		return nil, fmt.Errorf("OpenAI Responses generation returned no response")
	}
	response.ProviderMetadata = m.continuationMetadata(response.ProviderMetadata)
	if err = decodeToolCodecResponse(response, m.toolCodec); err != nil {
		return nil, err
	}
	if chain != nil {
		responseID := responseID(response.ProviderMetadata)
		output, encodeErr := encodeMessages(responseMessages(response.Content))
		if encodeErr != nil {
			return nil, encodeErr
		}
		if err = chain.state.Commit(request, responseID, output); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (m *LifecycleModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	prepared, request, chain, err := m.prepare(call)
	if err != nil {
		return nil, err
	}
	prepared, err = encodeToolCodecCall(prepared, m.toolCodec)
	if err != nil {
		if chain != nil {
			chain.mu.Unlock()
		}
		return nil, err
	}
	attempts := retryAttempts(m.retry)
	attempt := 1
	stream, err := m.inner.Stream(ctx, prepared)
	for err != nil && m.retryable(err, false) && attempt < attempts {
		if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
			break
		}
		attempt++
		stream, err = m.inner.Stream(ctx, prepared)
	}
	if err != nil {
		if chain != nil {
			chain.mu.Unlock()
		}
		return nil, RetryableError(m.retry, err, false)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		if chain != nil {
			defer chain.mu.Unlock()
		}
		for {
			collector := newStreamCollector()
			finished := false
			emitted := false
			retry := false
			for part := range stream {
				if part.Type == fantasy.StreamPartTypeFinish {
					part.ProviderMetadata = m.continuationMetadata(part.ProviderMetadata)
				}
				part, err = decodeToolCodecStreamPart(part, m.toolCodec)
				if err != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
					return
				}
				if part.Type == fantasy.StreamPartTypeError {
					if attempt < attempts && m.retryable(part.Error, emitted) {
						err = part.Error
						retry = true
						break
					}
					part.Error = RetryableError(m.retry, part.Error, emitted)
				}
				collector.add(part)
				if !yield(part) {
					return
				}
				if part.Type == fantasy.StreamPartTypeError {
					return
				}
				if part.Type != fantasy.StreamPartTypeWarnings {
					emitted = true
				}
				if part.Type == fantasy.StreamPartTypeFinish {
					finished = true
				}
			}
			if retry {
				if waitErr := providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); waitErr != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: waitErr})
					return
				}
				attempt++
				stream, err = m.inner.Stream(ctx, prepared)
				for err != nil && m.retryable(err, false) && attempt < attempts {
					if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
						break
					}
					attempt++
					stream, err = m.inner.Stream(ctx, prepared)
				}
				if err != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: RetryableError(m.retry, err, false)})
					return
				}
				continue
			}
			if !finished || chain == nil {
				return
			}
			output, encodeErr := encodeMessages(collector.messages())
			if encodeErr != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: encodeErr})
				return
			}
			if commitErr := chain.state.Commit(request, collector.responseID, output); commitErr != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: commitErr})
			}
			return
		}
	}, nil
}

func (m *LifecycleModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	prepared, request, chain, err := m.prepareObject(call)
	if err != nil {
		return nil, err
	}
	if chain != nil {
		defer chain.mu.Unlock()
	}
	attempts := retryAttempts(m.retry)
	var response *fantasy.ObjectResponse
	for attempt := 1; attempt <= attempts; attempt++ {
		response, err = m.inner.GenerateObject(ctx, prepared)
		if err == nil || !m.retryable(err, false) || attempt == attempts {
			break
		}
		if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
			return nil, err
		}
	}
	if err != nil {
		return nil, RetryableError(m.retry, err, false)
	}
	if response == nil {
		return nil, fmt.Errorf("OpenAI Responses object generation returned no response")
	}
	response.ProviderMetadata = m.continuationMetadata(response.ProviderMetadata)
	if chain != nil {
		output, encodeErr := encodeObjectOutput(response.RawText, response.Object)
		if encodeErr != nil {
			return nil, encodeErr
		}
		if err = chain.state.Commit(request, responseID(response.ProviderMetadata), output); err != nil {
			return nil, err
		}
	}
	return response, nil
}

func (m *LifecycleModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	prepared, request, chain, err := m.prepareObject(call)
	if err != nil {
		return nil, err
	}
	attempts := retryAttempts(m.retry)
	attempt := 1
	stream, err := m.inner.StreamObject(ctx, prepared)
	for err != nil && m.retryable(err, false) && attempt < attempts {
		if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
			break
		}
		attempt++
		stream, err = m.inner.StreamObject(ctx, prepared)
	}
	if err != nil {
		if chain != nil {
			chain.mu.Unlock()
		}
		return nil, RetryableError(m.retry, err, false)
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		if chain != nil {
			defer chain.mu.Unlock()
		}
		for {
			var lastObject any
			var rawText string
			responseIDValue := ""
			finished := false
			emitted := false
			retry := false
			for part := range stream {
				if part.Type == fantasy.ObjectStreamPartTypeError {
					if attempt < attempts && m.retryable(part.Error, emitted) {
						err = part.Error
						retry = true
						break
					}
					part.Error = RetryableError(m.retry, part.Error, emitted)
				}
				switch part.Type {
				case fantasy.ObjectStreamPartTypeObject:
					if part.Object != nil {
						lastObject = part.Object
						emitted = true
					}
				case fantasy.ObjectStreamPartTypeTextDelta:
					rawText += part.Delta
					if part.Delta != "" {
						emitted = true
					}
				case fantasy.ObjectStreamPartTypeFinish:
					part.ProviderMetadata = m.continuationMetadata(part.ProviderMetadata)
					responseIDValue = responseID(part.ProviderMetadata)
					finished = true
					emitted = true
				}
				if !yield(part) {
					return
				}
				if part.Type == fantasy.ObjectStreamPartTypeError {
					return
				}
			}
			if retry {
				if waitErr := providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); waitErr != nil {
					yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: waitErr})
					return
				}
				attempt++
				stream, err = m.inner.StreamObject(ctx, prepared)
				for err != nil && m.retryable(err, false) && attempt < attempts {
					if err = providertransport.WaitForRetry(ctx, m.retryDelay(attempt, err)); err != nil {
						break
					}
					attempt++
					stream, err = m.inner.StreamObject(ctx, prepared)
				}
				if err != nil {
					yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: RetryableError(m.retry, err, false)})
					return
				}
				continue
			}
			if !finished || chain == nil {
				return
			}
			output, encodeErr := encodeObjectOutput(rawText, lastObject)
			if encodeErr != nil {
				yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: encodeErr})
				return
			}
			if commitErr := chain.state.Commit(request, responseIDValue, output); commitErr != nil {
				yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: commitErr})
			}
			return
		}
	}, nil
}

func (m *LifecycleModel) ResetConversationChain(conversationID string) {
	m.store.resetModelConversation(m.owner, m.Model(), conversationID)
	if resetter, ok := m.inner.(interface{ ResetConversationChain(string) }); ok {
		resetter.ResetConversationChain(conversationID)
	}
}

func (m *LifecycleModel) prepare(call fantasy.Call) (fantasy.Call, ContinuationRequest, *continuationChain, error) {
	if m.continuation == nil || m.continuation.Mode == "none" {
		return call, ContinuationRequest{}, nil, nil
	}
	policy := *m.continuation
	conversationID := call.Headers["x-session-id"]
	if conversationID == "" {
		return call, ContinuationRequest{}, nil, fmt.Errorf("previous-response continuation requires a session identity")
	}
	request, err := continuationRequest(call, m.Model(), policy)
	if err != nil {
		return call, ContinuationRequest{}, nil, err
	}
	chain := m.store.chain(continuationKey{
		owner:        m.owner,
		model:        m.Model(),
		conversation: conversationID,
		purpose:      call.Headers["x-request-purpose"],
	})
	chain.mu.Lock()
	plan, err := chain.state.Plan(request)
	if err != nil {
		chain.mu.Unlock()
		return call, ContinuationRequest{}, nil, err
	}
	if plan.FallbackReason != "" && policy.Fallback == "error" {
		chain.mu.Unlock()
		return call, ContinuationRequest{}, nil, fmt.Errorf("previous-response continuation unavailable: %s", plan.FallbackReason)
	}
	options, err := responsesOptions(call.ProviderOptions)
	if err != nil {
		chain.mu.Unlock()
		return call, ContinuationRequest{}, nil, err
	}
	options.Store = new(request.Store)
	if plan.Incremental {
		options.PreviousResponseID = new(plan.PreviousResponseID)
		call.Prompt = call.Prompt[len(call.Prompt)-len(plan.Input):]
	} else {
		options.PreviousResponseID = nil
	}
	call.ProviderOptions = cloneProviderOptions(call.ProviderOptions)
	call.ProviderOptions[openai.Name] = options
	return call, request, chain, nil
}

func (m *LifecycleModel) prepareObject(call fantasy.ObjectCall) (fantasy.ObjectCall, ContinuationRequest, *continuationChain, error) {
	schemaName := call.SchemaName
	if schemaName == "" {
		schemaName = "response"
	}
	lifecycleCall := fantasy.Call{
		Prompt:           call.Prompt,
		Tools:            []fantasy.Tool{fantasy.FunctionTool{Name: schemaName, Description: call.SchemaDescription, InputSchema: schema.ToMap(call.Schema)}},
		MaxOutputTokens:  call.MaxOutputTokens,
		Temperature:      call.Temperature,
		TopP:             call.TopP,
		TopK:             call.TopK,
		PresencePenalty:  call.PresencePenalty,
		FrequencyPenalty: call.FrequencyPenalty,
		UserAgent:        call.UserAgent,
		Headers:          call.Headers,
		ProviderOptions:  call.ProviderOptions,
	}
	prepared, request, chain, err := m.prepare(lifecycleCall)
	if err != nil {
		return fantasy.ObjectCall{}, ContinuationRequest{}, nil, err
	}
	prepared, err = encodeToolCodecCall(prepared, m.toolCodec)
	if err != nil {
		if chain != nil {
			chain.mu.Unlock()
		}
		return fantasy.ObjectCall{}, ContinuationRequest{}, nil, err
	}
	call.Prompt = prepared.Prompt
	call.ProviderOptions = prepared.ProviderOptions
	return call, request, chain, nil
}

func (m *LifecycleModel) continuationMetadata(metadata fantasy.ProviderMetadata) fantasy.ProviderMetadata {
	if m.continuation == nil || m.continuation.MetadataNamespace == "" {
		return metadata
	}
	responseID := responseID(metadata)
	if responseID == "" {
		return metadata
	}
	result := make(fantasy.ProviderMetadata, len(metadata)+1)
	for namespace, value := range metadata {
		result[namespace] = value
	}
	result[m.continuation.MetadataNamespace] = &openai.ResponsesProviderMetadata{ResponseID: responseID}
	return result
}

func continuationRequest(call fantasy.Call, model string, policy manifest.ContinuationPolicy) (ContinuationRequest, error) {
	options, err := responsesOptions(call.ProviderOptions)
	if err != nil {
		return ContinuationRequest{}, err
	}
	store := options.Store != nil && *options.Store
	switch policy.Store {
	case "required":
		store = true
	case "forbidden":
		store = false
	}
	stable := make(map[string]any, len(policy.RequiredStableFields))
	for _, field := range policy.RequiredStableFields {
		switch field {
		case "model":
			stable[field] = model
		case "instructions":
			stable[field] = struct {
				Prompt fantasy.Prompt `json:"prompt"`
				Value  *string        `json:"value"`
			}{Prompt: systemMessages(call.Prompt), Value: options.Instructions}
		case "tools":
			stable[field] = call.Tools
		default:
			return ContinuationRequest{}, fmt.Errorf("previous-response stable field %q is unavailable", field)
		}
	}
	stableJSON, err := json.Marshal(stable)
	if err != nil {
		return ContinuationRequest{}, fmt.Errorf("encode stable Responses fields: %w", err)
	}
	input, err := encodeMessages(call.Prompt)
	if err != nil {
		return ContinuationRequest{}, err
	}
	return ContinuationRequest{Stable: stableJSON, Input: input, Store: store}, nil
}

func systemMessages(prompt fantasy.Prompt) fantasy.Prompt {
	result := make(fantasy.Prompt, 0, 1)
	for _, message := range prompt {
		if message.Role == fantasy.MessageRoleSystem {
			result = append(result, message)
		}
	}
	return result
}

func responsesOptions(options fantasy.ProviderOptions) (*openai.ResponsesProviderOptions, error) {
	if value, ok := options[openai.Name]; ok {
		typed, ok := value.(*openai.ResponsesProviderOptions)
		if !ok {
			return nil, fmt.Errorf("OpenAI Responses provider options have type %T", value)
		}
		clone := *typed
		return &clone, nil
	}
	return &openai.ResponsesProviderOptions{}, nil
}

func cloneProviderOptions(options fantasy.ProviderOptions) fantasy.ProviderOptions {
	result := make(fantasy.ProviderOptions, len(options)+1)
	for key, value := range options {
		result[key] = value
	}
	return result
}

func encodeMessages(messages fantasy.Prompt) ([]json.RawMessage, error) {
	result := make([]json.RawMessage, len(messages))
	for index, message := range messages {
		value, err := json.Marshal(message)
		if err != nil {
			return nil, fmt.Errorf("encode Responses history item %d: %w", index, err)
		}
		result[index] = value
	}
	return result, nil
}

func encodeObjectOutput(rawText string, object any) ([]json.RawMessage, error) {
	if rawText == "" {
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil, fmt.Errorf("encode Responses object output: %w", err)
		}
		rawText = string(encoded)
	}
	return encodeMessages(fantasy.Prompt{{
		Role:    fantasy.MessageRoleAssistant,
		Content: []fantasy.MessagePart{fantasy.TextPart{Text: rawText}},
	}})
}

func responseID(metadata fantasy.ProviderMetadata) string {
	value, ok := metadata[openai.Name].(*openai.ResponsesProviderMetadata)
	if !ok {
		return ""
	}
	return value.ResponseID
}

type streamCollector struct {
	text       map[string]string
	reasoning  map[string]fantasy.ReasoningPart
	parts      []fantasy.MessagePart
	responseID string
}

func newStreamCollector() *streamCollector {
	return &streamCollector{text: make(map[string]string), reasoning: make(map[string]fantasy.ReasoningPart)}
}

func (c *streamCollector) add(part fantasy.StreamPart) {
	switch part.Type {
	case fantasy.StreamPartTypeTextStart:
		c.text[part.ID] = ""
	case fantasy.StreamPartTypeTextDelta:
		c.text[part.ID] += part.Delta
	case fantasy.StreamPartTypeTextEnd:
		c.parts = append(c.parts, fantasy.TextPart{Text: c.text[part.ID], ProviderOptions: fantasy.ProviderOptions(part.ProviderMetadata)})
		delete(c.text, part.ID)
	case fantasy.StreamPartTypeReasoningStart:
		c.reasoning[part.ID] = fantasy.ReasoningPart{Text: part.Delta, ProviderOptions: fantasy.ProviderOptions(part.ProviderMetadata)}
	case fantasy.StreamPartTypeReasoningDelta:
		value := c.reasoning[part.ID]
		value.Text += part.Delta
		if part.ProviderMetadata != nil {
			value.ProviderOptions = fantasy.ProviderOptions(part.ProviderMetadata)
		}
		c.reasoning[part.ID] = value
	case fantasy.StreamPartTypeReasoningEnd:
		value := c.reasoning[part.ID]
		if part.ProviderMetadata != nil {
			value.ProviderOptions = fantasy.ProviderOptions(part.ProviderMetadata)
		}
		c.parts = append(c.parts, value)
		delete(c.reasoning, part.ID)
	case fantasy.StreamPartTypeToolCall:
		c.parts = append(c.parts, fantasy.ToolCallPart{ToolCallID: part.ID, ToolName: part.ToolCallName, Input: part.ToolCallInput, ProviderExecuted: part.ProviderExecuted, ProviderOptions: fantasy.ProviderOptions(part.ProviderMetadata)})
	case fantasy.StreamPartTypeToolResult:
		if part.ProviderExecuted {
			c.parts = append(c.parts, fantasy.ToolResultPart{ToolCallID: part.ID, ProviderExecuted: true, ProviderOptions: fantasy.ProviderOptions(part.ProviderMetadata)})
		}
	case fantasy.StreamPartTypeFinish:
		c.responseID = responseID(part.ProviderMetadata)
	}
}

func (c *streamCollector) messages() fantasy.Prompt {
	if len(c.parts) == 0 {
		return nil
	}
	return fantasy.Prompt{{Role: fantasy.MessageRoleAssistant, Content: c.parts}}
}

func responseMessages(content fantasy.ResponseContent) fantasy.Prompt {
	collector := newStreamCollector()
	for _, item := range content {
		switch item.GetType() {
		case fantasy.ContentTypeText:
			if value, ok := fantasy.AsContentType[fantasy.TextContent](item); ok {
				collector.parts = append(collector.parts, fantasy.TextPart{Text: value.Text, ProviderOptions: fantasy.ProviderOptions(value.ProviderMetadata)})
			}
		case fantasy.ContentTypeReasoning:
			if value, ok := fantasy.AsContentType[fantasy.ReasoningContent](item); ok {
				collector.parts = append(collector.parts, fantasy.ReasoningPart{Text: value.Text, ProviderOptions: fantasy.ProviderOptions(value.ProviderMetadata)})
			}
		case fantasy.ContentTypeToolCall:
			if value, ok := fantasy.AsContentType[fantasy.ToolCallContent](item); ok {
				collector.parts = append(collector.parts, fantasy.ToolCallPart{ToolCallID: value.ToolCallID, ToolName: value.ToolName, Input: value.Input, ProviderExecuted: value.ProviderExecuted, ProviderOptions: fantasy.ProviderOptions(value.ProviderMetadata)})
			}
		case fantasy.ContentTypeToolResult:
			if value, ok := fantasy.AsContentType[fantasy.ToolResultContent](item); ok && value.ProviderExecuted {
				collector.parts = append(collector.parts, fantasy.ToolResultPart{ToolCallID: value.ToolCallID, Output: value.Result, ProviderExecuted: true, ProviderOptions: fantasy.ProviderOptions(value.ProviderMetadata), ClientMetadata: value.ClientMetadata})
			}
		}
	}
	return collector.messages()
}

func retryAttempts(policy manifest.RetryPolicy) int {
	if policy.MaxAttempts < 1 {
		return 1
	}
	return policy.MaxAttempts
}

func (m *LifecycleModel) retryable(err error, emitted bool) bool {
	providertransport.MapError(m.errors, err)
	if m.operationRetry {
		return providertransport.RetryOperationError(m.retry, m.errors, err, emitted)
	}
	return m.retry.UnexpectedEOF && errors.Is(err, io.ErrUnexpectedEOF) && (m.retry.ReplayRequirement != "before-first-event" || !emitted)
}

func (m *LifecycleModel) retryDelay(attempt int, err error) time.Duration {
	retryAfter := ""
	if m.retry.RetryAfter {
		retryAfter = providertransport.RetryAfterHeader(err)
	}
	return providertransport.RetryDelay(m.retry, attempt, retryAfter)
}

func RetryableError(policy manifest.RetryPolicy, err error, emitted bool) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) && (!policy.UnexpectedEOF || policy.ReplayRequirement == "before-first-event" && emitted) {
		return &fantasy.Error{Title: "provider request failed", Message: err.Error()}
	}
	return err
}

var _ fantasy.LanguageModel = (*LifecycleModel)(nil)
