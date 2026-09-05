package declarative

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"math"
	"net/http"
	"net/url"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	providersse "github.com/example-git/crux/internal/providertransport/sse"
)

const (
	metadataType                  = "crux.declarative.metadata"
	optionsType                   = "crux.declarative.options"
	maxProviderErrorBodyBytes     = 1 << 20
	maxOpenDeclarativeStreamParts = 1024
)

type Metadata map[string]any

type Options struct {
	Values   map[string]any `json:"values,omitempty"`
	Controls map[string]any `json:"controls,omitempty"`
}

func init() {
	fantasy.RegisterProviderType(metadataType, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var value Metadata
		err := json.Unmarshal(data, &value)
		return &value, err
	})
	fantasy.RegisterProviderType(optionsType, func(data []byte) (fantasy.ProviderOptionsData, error) {
		var value Options
		err := json.Unmarshal(data, &value)
		return &value, err
	})
}

func (*Metadata) Options() {}

func (m Metadata) MarshalJSON() ([]byte, error) {
	type plain Metadata
	return fantasy.MarshalProviderType(metadataType, plain(m))
}

func (m *Metadata) UnmarshalJSON(data []byte) error {
	type plain Metadata
	var value plain
	if err := fantasy.UnmarshalProviderType(data, &value); err != nil {
		return err
	}
	*m = Metadata(value)
	return nil
}

func (*Options) Options() {}

func (o Options) MarshalJSON() ([]byte, error) {
	type plain Options
	return fantasy.MarshalProviderType(optionsType, plain(o))
}

func (o *Options) UnmarshalJSON(data []byte) error {
	type plain Options
	var value plain
	if err := fantasy.UnmarshalProviderType(data, &value); err != nil {
		return err
	}
	*o = Options(value)
	return nil
}

type Provider struct {
	ID              string
	Operation       *providertransport.Operation
	Usage           *manifest.UsagePolicy
	Errors          []manifest.ErrorMapping
	Metadata        []manifest.MetadataContract
	MetadataSchemas manifest.MetadataSchemas
	Headers         map[string]string
	Values          providertransport.TemplateValues
	HTTPClient      *http.Client
	RuntimeControl  map[string]any
}

func (p *Provider) Name() string { return p.ID }

func (p *Provider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	if p == nil || p.Operation == nil {
		return nil, fmt.Errorf("declarative provider has no operation contract")
	}
	switch p.Operation.Key.Transport {
	case "http-json":
		if p.Operation.Streaming != nil {
			return nil, fmt.Errorf("provider %s operation %q http-json transport must not declare streaming policy", p.ID, p.Operation.ID)
		}
	case "sse":
		if p.Operation.Streaming == nil || p.Operation.Streaming.EventSource != "sse-data-json" {
			return nil, fmt.Errorf("provider %s operation %q SSE transport requires a streaming policy using sse-data-json", p.ID, p.Operation.ID)
		}
		if err := manifest.ValidateEventMappings(p.Operation.Streaming.Mappings); err != nil {
			return nil, fmt.Errorf("provider %s operation %q SSE event mappings: %w", p.ID, p.Operation.ID, err)
		}
	case "websocket-json":
		return nil, fmt.Errorf("provider %s operation %q requires websocket-json; this protocol has no native declarative WebSocket adapter", p.ID, p.Operation.ID)
	default:
		return nil, fmt.Errorf("provider %s operation %q uses unsupported transport %q", p.ID, p.Operation.ID, p.Operation.Key.Transport)
	}
	provider := *p
	if len(provider.Metadata) > 0 && len(provider.MetadataSchemas) == 0 {
		schemas, err := manifest.CompileMetadataContracts(provider.Metadata)
		if err != nil {
			return nil, fmt.Errorf("provider %s metadata contracts: %w", p.ID, err)
		}
		provider.MetadataSchemas = schemas
	}
	return &languageModel{provider: &provider, modelID: modelID}, nil
}

type languageModel struct {
	provider *Provider
	modelID  string
}

type declarativeStreamState struct {
	usage    fantasy.Usage
	metadata fantasy.ProviderMetadata
	terminal bool
}

type streamContentKind uint8

const (
	streamContentText streamContentKind = iota
	streamContentReasoning
	streamContentToolInput
)

type streamContentKey struct {
	kind streamContentKind
	id   string
}

type streamLifecycle struct {
	providerID       string
	active           map[streamContentKey]struct{}
	order            []streamContentKey
	pendingToolCalls map[string]fantasy.ProviderMetadata
}

func newStreamLifecycle(providerID string) *streamLifecycle {
	return &streamLifecycle{
		providerID:       providerID,
		active:           make(map[streamContentKey]struct{}),
		pendingToolCalls: make(map[string]fantasy.ProviderMetadata),
	}
}

func (s *streamLifecycle) open(key streamContentKey) error {
	if _, exists := s.active[key]; exists {
		return fmt.Errorf("provider %s stream opened the same content frame more than once", s.providerID)
	}
	if key.kind == streamContentToolInput {
		if _, exists := s.pendingToolCalls[key.id]; exists {
			return fmt.Errorf("provider %s stream reopened tool input before emitting its tool call", s.providerID)
		}
	}
	if len(s.active)+len(s.pendingToolCalls) >= maxOpenDeclarativeStreamParts {
		return fmt.Errorf("provider %s stream exceeded the open content frame limit", s.providerID)
	}
	s.active[key] = struct{}{}
	s.order = append(s.order, key)
	return nil
}

func (s *streamLifecycle) close(key streamContentKey) bool {
	if _, exists := s.active[key]; !exists {
		return false
	}
	delete(s.active, key)
	for index, current := range s.order {
		if current != key {
			continue
		}
		copy(s.order[index:], s.order[index+1:])
		s.order = s.order[:len(s.order)-1]
		break
	}
	return true
}

func (s *streamLifecycle) normalize(parts []fantasy.StreamPart) ([]fantasy.StreamPart, error) {
	result := make([]fantasy.StreamPart, 0, len(parts)+len(s.active)+1)
	for _, part := range parts {
		switch part.Type {
		case fantasy.StreamPartTypeTextStart:
			if err := s.open(streamContentKey{kind: streamContentText, id: part.ID}); err != nil {
				return nil, err
			}
		case fantasy.StreamPartTypeTextDelta:
			key := streamContentKey{kind: streamContentText, id: part.ID}
			if _, exists := s.active[key]; !exists {
				if err := s.open(key); err != nil {
					return nil, err
				}
				result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: part.ID})
			}
		case fantasy.StreamPartTypeTextEnd:
			if !s.close(streamContentKey{kind: streamContentText, id: part.ID}) {
				return nil, fmt.Errorf("provider %s stream ended text without a matching start", s.providerID)
			}
		case fantasy.StreamPartTypeReasoningStart:
			if err := s.open(streamContentKey{kind: streamContentReasoning, id: part.ID}); err != nil {
				return nil, err
			}
		case fantasy.StreamPartTypeReasoningDelta:
			key := streamContentKey{kind: streamContentReasoning, id: part.ID}
			if _, exists := s.active[key]; !exists {
				if err := s.open(key); err != nil {
					return nil, err
				}
				result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: part.ID})
			}
		case fantasy.StreamPartTypeReasoningEnd:
			if !s.close(streamContentKey{kind: streamContentReasoning, id: part.ID}) {
				return nil, fmt.Errorf("provider %s stream ended reasoning without a matching start", s.providerID)
			}
		case fantasy.StreamPartTypeToolInputStart:
			if err := s.open(streamContentKey{kind: streamContentToolInput, id: part.ID}); err != nil {
				return nil, err
			}
		case fantasy.StreamPartTypeToolInputDelta:
			if _, exists := s.active[streamContentKey{kind: streamContentToolInput, id: part.ID}]; !exists {
				return nil, fmt.Errorf("provider %s stream emitted tool input without a matching start", s.providerID)
			}
		case fantasy.StreamPartTypeToolInputEnd:
			if !s.close(streamContentKey{kind: streamContentToolInput, id: part.ID}) {
				return nil, fmt.Errorf("provider %s stream ended tool input without a matching start", s.providerID)
			}
			s.pendingToolCalls[part.ID] = part.ProviderMetadata
		case fantasy.StreamPartTypeToolCall:
			key := streamContentKey{kind: streamContentToolInput, id: part.ID}
			if s.close(key) {
				result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeToolInputEnd, ID: part.ID})
			}
			if metadata, exists := s.pendingToolCalls[part.ID]; exists {
				merged, err := mergeProviderMetadata(metadata, part.ProviderMetadata)
				if err != nil {
					return nil, fmt.Errorf("provider %s %w", s.providerID, err)
				}
				part.ProviderMetadata = merged
				delete(s.pendingToolCalls, part.ID)
			}
		case fantasy.StreamPartTypeFinish:
			for key := range s.active {
				if key.kind == streamContentToolInput {
					return nil, fmt.Errorf("provider %s stream finished with incomplete tool input", s.providerID)
				}
			}
			if len(s.pendingToolCalls) > 0 {
				return nil, fmt.Errorf("provider %s stream finished without emitting a completed tool call", s.providerID)
			}
			for _, key := range append([]streamContentKey(nil), s.order...) {
				switch key.kind {
				case streamContentText:
					result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: key.id})
				case streamContentReasoning:
					result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: key.id})
				}
				s.close(key)
			}
		}
		result = append(result, part)
	}
	return result, nil
}

func (m *languageModel) Provider() string { return m.provider.ID }
func (m *languageModel) Model() string    { return m.modelID }

func (m *languageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	switch m.provider.Operation.Key.Transport {
	case "http-json":
		return m.generateHTTPJSON(ctx, call)
	case "sse":
		return m.generateSSE(ctx, call)
	default:
		return nil, fmt.Errorf("provider %s operation %q uses unsupported transport %q", m.provider.ID, m.provider.Operation.ID, m.provider.Operation.Key.Transport)
	}
}

func (m *languageModel) generateHTTPJSON(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	document, err := m.request(call)
	if err != nil {
		return nil, err
	}
	response, err := m.do(ctx, document, false, call.Headers)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, err := readBounded(response.Body, 16<<20)
	if err != nil {
		return nil, err
	}
	result, err := decodeJSONDocument(data)
	if err != nil {
		return nil, fmt.Errorf("provider %s response is not valid JSON: %w", m.provider.ID, err)
	}
	if err := providertransport.ApplyJSONPipeline(result, m.provider.Operation.ResponseTransform, m.provider.Values); err != nil {
		return nil, fmt.Errorf("provider %s response transform: %w", m.provider.ID, err)
	}
	parts, err := m.mapDocumentWithUsage(result, usagePolicyForSource(m.provider.Usage, "response"))
	if err != nil {
		return nil, err
	}
	collector := newResponseCollector(m.provider.ID)
	for _, part := range parts {
		if err := collector.consume(part); err != nil {
			return nil, err
		}
	}
	responseValue := collector.response
	if len(parts) == 0 {
		if object, ok := result.(map[string]any); ok {
			text, _ := object["text"].(string)
			if text == "" {
				text, _ = object["output_text"].(string)
			}
			if text != "" {
				responseValue.Content = append(responseValue.Content, fantasy.TextContent{Text: text})
			}
		}
	}
	responseValue.Usage, err = mapUsage(result, usagePolicyForSource(m.provider.Usage, "response"), responseValue.Usage)
	if err != nil {
		return nil, fmt.Errorf("provider %s usage: %w", m.provider.ID, err)
	}
	if responseValue.FinishReason == fantasy.FinishReasonUnknown {
		responseValue.FinishReason = fantasy.FinishReasonStop
	}
	return &responseValue, nil
}

func (m *languageModel) generateSSE(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	stream, err := m.streamSSE(ctx, call)
	if err != nil {
		return nil, err
	}
	collector := newResponseCollector(m.provider.ID)
	for part := range stream {
		if err := collector.consume(part); err != nil {
			return nil, err
		}
	}
	if !collector.finished {
		return nil, fmt.Errorf("provider %s stream ended without a mapped finish event", m.provider.ID)
	}
	if collector.response.FinishReason == fantasy.FinishReasonUnknown {
		collector.response.FinishReason = fantasy.FinishReasonStop
	}
	return &collector.response, nil
}

func (m *languageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	switch m.provider.Operation.Key.Transport {
	case "http-json":
		return m.streamHTTPJSON(ctx, call)
	case "sse":
		return m.streamSSE(ctx, call)
	default:
		return nil, fmt.Errorf("provider %s operation %q uses unsupported transport %q", m.provider.ID, m.provider.Operation.ID, m.provider.Operation.Key.Transport)
	}
}

func (m *languageModel) streamHTTPJSON(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	response, err := m.generateHTTPJSON(ctx, call)
	if err != nil {
		return nil, err
	}
	return streamResponse(response), nil
}

func (m *languageModel) streamSSE(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	document, err := m.request(call)
	if err != nil {
		return nil, err
	}
	streaming := m.provider.Operation.Streaming
	if streaming == nil {
		return nil, fmt.Errorf("provider %s operation %q has no streaming policy", m.provider.ID, m.provider.Operation.ID)
	}
	policy := m.provider.Operation.Retry
	attempts := policy.MaxAttempts
	if attempts < 1 {
		attempts = 1
	}
	attempt := 1
	open := func() (io.ReadCloser, error) {
		singleAttempt := policy
		singleAttempt.MaxAttempts = 1
		response, err := m.doWithRetry(ctx, document, true, call.Headers, singleAttempt)
		if err != nil {
			return nil, err
		}
		return response.Body, nil
	}
	body, err := open()
	for err != nil && attempt < attempts && providertransport.RetryOperationError(policy, m.provider.Errors, err, false) {
		retryAfter := ""
		if policy.RetryAfter {
			retryAfter = providertransport.RetryAfterHeader(err)
		}
		if err = providertransport.WaitForRetry(ctx, providertransport.RetryDelay(policy, attempt, retryAfter)); err != nil {
			return nil, err
		}
		attempt++
		body, err = open()
	}
	if err != nil {
		return nil, providertransport.MapError(m.provider.Errors, err)
	}
	return func(yield func(fantasy.StreamPart) bool) {
		for {
			state := &declarativeStreamState{}
			lifecycle := newStreamLifecycle(m.provider.ID)
			consumerStopped := false
			emitted := false
			var retryErr error
			options := providersse.Options{DoneMarker: streaming.DoneMarker, RequireTerminal: streaming.RequireTerminal, MaxEventBytes: streaming.MaxEventBytes}
			parseErr := providersse.Parse(ctx, body, options, func(raw json.RawMessage) error {
				document, decodeErr := decodeJSONDocument(raw)
				if decodeErr != nil {
					return decodeErr
				}
				if transformErr := providertransport.ApplyJSONPipeline(document, m.provider.Operation.ResponseTransform, m.provider.Values); transformErr != nil {
					return transformErr
				}
				parts, terminal, mapErr := m.mapDocumentEventWithState(document, usagePolicyForSource(m.provider.Usage, "stream"), state)
				if mapErr != nil {
					return mapErr
				}
				parts, mapErr = lifecycle.normalize(parts)
				if mapErr != nil {
					return mapErr
				}
				for _, part := range parts {
					if part.Type == fantasy.StreamPartTypeError {
						providertransport.MapError(m.provider.Errors, part.Error)
						if attempt < attempts && providertransport.RetryOperationError(policy, m.provider.Errors, part.Error, emitted) {
							retryErr = part.Error
							return retryErr
						}
					}
					if !yield(part) {
						consumerStopped = true
						return io.EOF
					}
					if part.Type != fantasy.StreamPartTypeWarnings {
						emitted = true
					}
				}
				if terminal {
					return providersse.ErrTerminal
				}
				return nil
			})
			_ = body.Close()
			if consumerStopped {
				return
			}
			if retryErr == nil && parseErr != nil && parseErr != io.EOF && attempt < attempts && providertransport.RetryOperationError(policy, m.provider.Errors, parseErr, emitted) {
				retryErr = parseErr
			}
			if retryErr != nil {
				for {
					retryAfter := ""
					if policy.RetryAfter {
						retryAfter = providertransport.RetryAfterHeader(retryErr)
					}
					if waitErr := providertransport.WaitForRetry(ctx, providertransport.RetryDelay(policy, attempt, retryAfter)); waitErr != nil {
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: waitErr})
						return
					}
					attempt++
					body, err = open()
					if err == nil {
						break
					}
					providertransport.MapError(m.provider.Errors, err)
					if attempt >= attempts || !providertransport.RetryOperationError(policy, m.provider.Errors, err, false) {
						yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
						return
					}
					retryErr = err
				}
				continue
			}
			if parseErr != nil && parseErr != io.EOF {
				providertransport.MapError(m.provider.Errors, parseErr)
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: parseErr})
				return
			}
			if !state.terminal {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider %s stream ended without a mapped finish or error event", m.provider.ID)})
			}
			return
		}
	}, nil
}

func (m *languageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	response, err := m.Generate(ctx, objectCall(call))
	if err != nil {
		return nil, err
	}
	text := response.Content.Text()
	var object any
	if err := json.Unmarshal([]byte(text), &object); err != nil {
		return nil, fmt.Errorf("provider %s structured response is not valid JSON: %w", m.provider.ID, err)
	}
	return &fantasy.ObjectResponse{Object: object, RawText: text, Usage: response.Usage, FinishReason: response.FinishReason, Warnings: response.Warnings, ProviderMetadata: response.ProviderMetadata}, nil
}

func (m *languageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	stream, err := m.Stream(ctx, objectCall(call))
	if err != nil {
		return nil, err
	}
	return func(yield func(fantasy.ObjectStreamPart) bool) {
		var text strings.Builder
		for part := range stream {
			switch part.Type {
			case fantasy.StreamPartTypeTextDelta:
				text.WriteString(part.Delta)
				if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeTextDelta, Delta: part.Delta}) {
					return
				}
			case fantasy.StreamPartTypeError:
				yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: part.Error})
				return
			case fantasy.StreamPartTypeFinish:
				var object any
				if err := json.Unmarshal([]byte(text.String()), &object); err != nil {
					yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeError, Error: err})
					return
				}
				if !yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeObject, Object: object}) {
					return
				}
				yield(fantasy.ObjectStreamPart{Type: fantasy.ObjectStreamPartTypeFinish, Usage: part.Usage, FinishReason: part.FinishReason, ProviderMetadata: part.ProviderMetadata})
				return
			}
		}
	}, nil
}

func (m *languageModel) request(call fantasy.Call) (map[string]any, error) {
	document, err := canonicalRequest(m.modelID, call)
	if err != nil {
		return nil, err
	}
	values := m.provider.Values
	if values.Context == nil {
		values.Context = map[string]string{}
	}
	values.Context["model.id"] = m.modelID
	if call.UserAgent != "" {
		values.Context["client.user_agent"] = call.UserAgent
	}
	if err := providertransport.ApplyPromptPipeline(document, m.provider.Operation.PromptTransform, values); err != nil {
		return nil, fmt.Errorf("provider %s prompt transform: %w", m.provider.ID, err)
	}
	if m.provider.Operation.Key.Protocol == "gemini-generate-content" {
		document, err = geminiRequest(document, m.provider.Operation.RoleMap)
		if err != nil {
			return nil, err
		}
	} else if err := providertransport.ApplyRoleMap(document, m.provider.Operation.RoleMap); err != nil {
		return nil, fmt.Errorf("provider %s role map: %w", m.provider.ID, err)
	}
	for path, value := range m.provider.RuntimeControl {
		if err := providertransport.SetJSONPointer(document, path, value, false); err != nil {
			return nil, fmt.Errorf("provider %s runtime control default %q: %w", m.provider.ID, path, err)
		}
	}
	if err := providertransport.ApplyJSONPipeline(document, m.provider.Operation.RequestTransform, values); err != nil {
		return nil, fmt.Errorf("provider %s request transform: %w", m.provider.ID, err)
	}
	if options, ok := call.ProviderOptions[m.provider.ID].(*Options); ok {
		for name, value := range options.Values {
			document[name] = value
		}
		for path, value := range options.Controls {
			if err := providertransport.SetJSONPointer(document, path, value, false); err != nil {
				return nil, fmt.Errorf("provider %s runtime control %q: %w", m.provider.ID, path, err)
			}
		}
	}
	return document, nil
}

func (m *languageModel) do(ctx context.Context, document map[string]any, stream bool, callHeaders map[string]string) (*http.Response, error) {
	return m.doWithRetry(ctx, document, stream, callHeaders, m.provider.Operation.Retry)
}

func (m *languageModel) doWithRetry(ctx context.Context, document map[string]any, stream bool, callHeaders map[string]string, retry manifest.RetryPolicy) (*http.Response, error) {
	body, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(m.provider.Operation.Endpoint.BaseURL)
	if err != nil {
		return nil, err
	}
	path := strings.ReplaceAll(m.provider.Operation.Path, "{model}", url.PathEscape(m.modelID))
	reference, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	target := base.ResolveReference(reference)
	request, err := http.NewRequestWithContext(ctx, m.provider.Operation.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	headers := cloneHeaders(callHeaders)
	for key, value := range m.provider.Headers {
		headers[key] = value
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	} else {
		request.Header.Set("Accept", "application/json")
	}
	client := m.provider.Operation.HTTPClient(m.provider.HTTPClient)
	response, err := providertransport.DoWithRetry(request, client, retry, m.provider.Operation.Errors)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		responseBody, readErr := readProviderErrorBody(response.Body)
		return nil, &fantasy.ProviderError{
			Message:         fmt.Sprintf("provider %s request failed with HTTP %d", m.provider.ID, response.StatusCode),
			Cause:           readErr,
			URL:             target.String(),
			StatusCode:      response.StatusCode,
			ResponseHeaders: responseHeaders(response.Header),
			ResponseBody:    responseBody,
		}
	}
	return response, nil
}

func (m *languageModel) mapDocument(document any) ([]fantasy.StreamPart, error) {
	return m.mapDocumentWithUsage(document, m.provider.Usage)
}

func (m *languageModel) mapDocumentWithUsage(document any, usage *manifest.UsagePolicy) ([]fantasy.StreamPart, error) {
	parts, _, err := m.mapDocumentEvent(document, usage)
	return parts, err
}

func (m *languageModel) mapDocumentEvent(document any, usage *manifest.UsagePolicy) ([]fantasy.StreamPart, bool, error) {
	return m.mapDocumentEventWithState(document, usage, &declarativeStreamState{})
}

func (m *languageModel) mapDocumentEventWithState(document any, usage *manifest.UsagePolicy, state *declarativeStreamState) ([]fantasy.StreamPart, bool, error) {
	policy := m.provider.Operation.Streaming
	metadataSchemas := m.provider.MetadataSchemas
	if len(m.provider.Metadata) > 0 && len(metadataSchemas) == 0 {
		var err error
		metadataSchemas, err = manifest.CompileMetadataContracts(m.provider.Metadata)
		if err != nil {
			return nil, false, fmt.Errorf("provider %s metadata contracts: %w", m.provider.ID, err)
		}
	}
	if policy == nil {
		return nil, false, nil
	}
	eventType, _ := providertransport.JSONPointer(document, policy.EventTypePointer).(string)
	sourceKnown := false
	var result []fantasy.StreamPart
	var terminalPart *fantasy.StreamPart
	for _, mapping := range policy.Mappings {
		if mapping.Source != eventType {
			continue
		}
		sourceKnown = true
		if mapping.Condition != nil {
			matched, err := providertransport.EvaluatePredicate(document, *mapping.Condition, m.provider.Values)
			if err != nil {
				return nil, false, fmt.Errorf("provider %s event %q condition: %w", m.provider.ID, eventType, err)
			}
			if !matched {
				continue
			}
		}
		part, err := mappedPart(document, mapping, m.provider.ID, usage, state.usage, m.provider.Errors, m.provider.Metadata, metadataSchemas)
		if err != nil {
			return nil, false, err
		}
		switch mapping.Event {
		case "usage":
			state.usage = part.Usage
			state.metadata, err = mergeProviderMetadata(state.metadata, part.ProviderMetadata)
			if err != nil {
				return nil, false, fmt.Errorf("provider %s %w", m.provider.ID, err)
			}
		case "finish":
			if terminalPart != nil {
				return nil, false, fmt.Errorf("provider %s event %q matched multiple terminal mappings", m.provider.ID, eventType)
			}
			state.usage = part.Usage
			terminalPart = &part
		case "error":
			if terminalPart != nil {
				return nil, false, fmt.Errorf("provider %s event %q matched multiple terminal mappings", m.provider.ID, eventType)
			}
			terminalPart = &part
		default:
			result = append(result, part)
		}
	}
	if terminalPart != nil {
		state.terminal = true
		if terminalPart.Type == fantasy.StreamPartTypeFinish {
			terminalPart.Usage = state.usage
			metadata, err := mergeProviderMetadata(state.metadata, terminalPart.ProviderMetadata)
			if err != nil {
				return nil, false, fmt.Errorf("provider %s %w", m.provider.ID, err)
			}
			terminalPart.ProviderMetadata = metadata
		}
		result = append(result, *terminalPart)
		return result, true, nil
	}
	if !sourceKnown && eventType != "" {
		message := fmt.Sprintf("provider %s returned unknown event %q", m.provider.ID, eventType)
		switch policy.UnknownEvent {
		case "warn":
			result = append(result, fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings, Warnings: []fantasy.CallWarning{{Type: fantasy.CallWarningTypeOther, Message: message}}})
		case "error":
			return nil, false, fmt.Errorf("%s", message)
		}
	}
	return result, false, nil
}

func mergeProviderMetadata(base, additional fantasy.ProviderMetadata) (fantasy.ProviderMetadata, error) {
	if len(base) == 0 && len(additional) == 0 {
		return nil, nil
	}
	result := make(fantasy.ProviderMetadata, len(base)+len(additional))
	for namespace, value := range base {
		result[namespace] = value
	}
	for namespace, value := range additional {
		current, exists := result[namespace]
		if !exists {
			result[namespace] = value
			continue
		}
		existing, existingOK := current.(*Metadata)
		next, nextOK := value.(*Metadata)
		if !existingOK || !nextOK {
			return nil, fmt.Errorf("metadata namespace %q conflicts with an earlier event", namespace)
		}
		merged := make(Metadata, len(*existing)+len(*next))
		for name, field := range *existing {
			merged[name] = field
		}
		for name, field := range *next {
			if previous, duplicate := merged[name]; duplicate {
				if !providertransport.JSONValuesEqual(previous, field) {
					return nil, fmt.Errorf("metadata namespace %q field %q conflicts with an earlier event", namespace, name)
				}
				continue
			}
			merged[name] = field
		}
		result[namespace] = &merged
	}
	return result, nil
}

func canonicalRequest(model string, call fantasy.Call) (map[string]any, error) {
	messages := make([]any, 0, len(call.Prompt))
	for _, message := range call.Prompt {
		parts := make([]any, 0, len(message.Content))
		for _, raw := range message.Content {
			switch part := raw.(type) {
			case fantasy.TextPart:
				parts = append(parts, map[string]any{"type": "text", "text": part.Text})
			case fantasy.ReasoningPart:
				parts = append(parts, map[string]any{"type": "reasoning", "text": part.Text})
			case fantasy.FilePart:
				parts = append(parts, map[string]any{"type": "file", "media_type": part.MediaType, "data": base64.StdEncoding.EncodeToString(part.Data)})
			case fantasy.ToolCallPart:
				var input any
				if err := json.Unmarshal([]byte(part.Input), &input); err != nil {
					return nil, fmt.Errorf("tool call %q input is not valid JSON", part.ToolName)
				}
				parts = append(parts, map[string]any{"type": "tool-call", "id": part.ToolCallID, "name": part.ToolName, "input": input})
			case fantasy.ToolResultPart:
				parts = append(parts, map[string]any{"type": "tool-result", "id": part.ToolCallID, "output": toolOutput(part.Output)})
			default:
				return nil, fmt.Errorf("unsupported prompt part %T", raw)
			}
		}
		content := any(parts)
		if len(parts) == 1 {
			if text, ok := parts[0].(map[string]any)["text"].(string); ok {
				content = text
			}
		}
		messages = append(messages, map[string]any{"role": string(message.Role), "content": content})
	}
	document := map[string]any{"model": model, "messages": messages}
	setOptional(document, "max_output_tokens", call.MaxOutputTokens)
	setOptional(document, "temperature", call.Temperature)
	setOptional(document, "top_p", call.TopP)
	setOptional(document, "top_k", call.TopK)
	setOptional(document, "presence_penalty", call.PresencePenalty)
	setOptional(document, "frequency_penalty", call.FrequencyPenalty)
	if len(call.Tools) > 0 {
		tools := make([]any, 0, len(call.Tools))
		for _, tool := range call.Tools {
			function, ok := tool.(fantasy.FunctionTool)
			if !ok {
				return nil, fmt.Errorf("provider-defined tool %q is unsupported by declarative transport", tool.GetName())
			}
			tools = append(tools, map[string]any{"name": function.Name, "description": function.Description, "input_schema": function.InputSchema})
		}
		document["tools"] = tools
	}
	if call.ToolChoice != nil {
		document["tool_choice"] = string(*call.ToolChoice)
	}
	return document, nil
}

func geminiRequest(document map[string]any, roles *manifest.RoleMap) (map[string]any, error) {
	result := map[string]any{}
	messages, _ := document["messages"].([]any)
	for _, raw := range messages {
		message := raw.(map[string]any)
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			if _, exists := result["systemInstruction"]; exists {
				return nil, fmt.Errorf("Gemini request has multiple system messages after prompt transforms")
			}
			result["systemInstruction"] = map[string]any{"parts": geminiParts(message["content"])}
			continue
		}
		if roles != nil {
			mapped := map[string]string{"user": roles.User, "assistant": roles.Assistant, "tool": roles.Tool}[role]
			if mapped == "" {
				switch roles.Unknown {
				case "drop", "warn-drop":
					continue
				default:
					return nil, fmt.Errorf("role %q has no provider mapping", role)
				}
			}
			role = mapped
		}
		result["contents"] = append(array(result["contents"]), map[string]any{"role": role, "parts": geminiParts(message["content"])})
	}
	generation := map[string]any{}
	for source, target := range map[string]string{"max_output_tokens": "maxOutputTokens", "temperature": "temperature", "top_p": "topP", "top_k": "topK", "presence_penalty": "presencePenalty", "frequency_penalty": "frequencyPenalty"} {
		if value, ok := document[source]; ok {
			generation[target] = value
		}
	}
	if len(generation) > 0 {
		result["generationConfig"] = generation
	}
	if tools, ok := document["tools"].([]any); ok {
		declarations := make([]any, 0, len(tools))
		for _, raw := range tools {
			tool := raw.(map[string]any)
			declarations = append(declarations, map[string]any{"name": tool["name"], "description": tool["description"], "parameters": tool["input_schema"]})
		}
		result["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	return result, nil
}

func geminiParts(content any) []any {
	if text, ok := content.(string); ok {
		return []any{map[string]any{"text": text}}
	}
	parts, _ := content.([]any)
	result := make([]any, 0, len(parts))
	for _, raw := range parts {
		part := raw.(map[string]any)
		switch part["type"] {
		case "text", "reasoning":
			result = append(result, map[string]any{"text": part["text"], "thought": part["type"] == "reasoning"})
		case "file":
			result = append(result, map[string]any{"inlineData": map[string]any{"mimeType": part["media_type"], "data": part["data"]}})
		case "tool-call":
			result = append(result, map[string]any{"functionCall": map[string]any{"id": part["id"], "name": part["name"], "args": part["input"]}})
		case "tool-result":
			result = append(result, map[string]any{"functionResponse": map[string]any{"id": part["id"], "response": part["output"]}})
		}
	}
	return result
}

func mappedPart(document any, mapping manifest.EventMapping, providerID string, usagePolicy *manifest.UsagePolicy, currentUsage fantasy.Usage, errorMappings []manifest.ErrorMapping, contracts []manifest.MetadataContract, schemas manifest.MetadataSchemas) (fantasy.StreamPart, error) {
	field := func(name string) any {
		pointer, ok := mapping.Fields[name]
		if !ok {
			return nil
		}
		return providertransport.JSONPointer(document, pointer)
	}
	part := fantasy.StreamPart{ID: stringField(field("id")), Delta: stringField(field("delta")), ToolCallName: stringField(field("name")), ToolCallInput: stringField(field("input")), FinishReason: finishReason(stringField(field("finish_reason")))}
	if pointer, ok := mapping.Fields["provider_executed"]; ok {
		value, present, err := providertransport.LookupJSONPointer(document, pointer)
		if err != nil || !present {
			return fantasy.StreamPart{}, fmt.Errorf("provider %s event field %q is unavailable", providerID, "provider_executed")
		}
		providerExecuted, ok := value.(bool)
		if !ok {
			return fantasy.StreamPart{}, fmt.Errorf("provider %s event field %q is not boolean", providerID, "provider_executed")
		}
		part.ProviderExecuted = providerExecuted
	}
	switch mapping.Event {
	case "warning":
		part.Type = fantasy.StreamPartTypeWarnings
		part.Warnings = []fantasy.CallWarning{{Type: fantasy.CallWarningTypeOther, Message: stringField(field("message"))}}
	case "text-start":
		part.Type = fantasy.StreamPartTypeTextStart
	case "text-delta":
		part.Type = fantasy.StreamPartTypeTextDelta
	case "text-end":
		part.Type = fantasy.StreamPartTypeTextEnd
	case "reasoning-start":
		part.Type = fantasy.StreamPartTypeReasoningStart
	case "reasoning-delta":
		part.Type = fantasy.StreamPartTypeReasoningDelta
	case "reasoning-end":
		part.Type = fantasy.StreamPartTypeReasoningEnd
	case "tool-input-start":
		part.Type = fantasy.StreamPartTypeToolInputStart
	case "tool-input-delta":
		part.Type = fantasy.StreamPartTypeToolInputDelta
	case "tool-input-end":
		part.Type = fantasy.StreamPartTypeToolInputEnd
	case "tool-call":
		part.Type = fantasy.StreamPartTypeToolCall
	case "tool-result":
		part.Type = fantasy.StreamPartTypeToolResult
		part.ProviderExecuted = true
	case "source":
		part.Type = fantasy.StreamPartTypeSource
		part.URL, part.Title = stringField(field("url")), stringField(field("title"))
		sourceType, ok := field("source_type").(string)
		if !ok {
			return fantasy.StreamPart{}, fmt.Errorf("provider %s event field %q is unavailable", providerID, "source_type")
		}
		switch fantasy.SourceType(sourceType) {
		case fantasy.SourceTypeURL, fantasy.SourceTypeDocument:
			part.SourceType = fantasy.SourceType(sourceType)
		default:
			return fantasy.StreamPart{}, fmt.Errorf("provider %s event field %q is unsupported", providerID, "source_type")
		}
	case "usage", "finish":
		var err error
		part.Usage, err = mapUsage(document, usagePolicy, currentUsage)
		if err != nil {
			return fantasy.StreamPart{}, fmt.Errorf("provider %s usage: %w", providerID, err)
		}
		if mapping.Event == "finish" {
			part.Type = fantasy.StreamPartTypeFinish
		}
	case "error":
		part.Type = fantasy.StreamPartTypeError
		part.Error = newProviderEventError(document, errorMappings, fmt.Sprintf("provider %s: %s", providerID, boundedProviderErrorMessage(stringField(field("message")))))
	default:
		return fantasy.StreamPart{}, fmt.Errorf("provider %s mapping uses unsupported event %q", providerID, mapping.Event)
	}
	if mapping.MetadataNamespace != "" {
		metadata := Metadata{}
		for name, pointer := range mapping.Fields {
			fieldName, ok := strings.CutPrefix(name, "metadata.")
			if !ok {
				continue
			}
			value, present, err := providertransport.LookupJSONPointer(document, pointer)
			if err != nil {
				return fantasy.StreamPart{}, fmt.Errorf("provider %s metadata field %q is unavailable", providerID, fieldName)
			}
			if present {
				metadata[fieldName] = value
			}
		}
		if len(metadata) > 0 {
			_, ok := metadataContract(contracts, mapping.MetadataNamespace)
			if !ok {
				return fantasy.StreamPart{}, fmt.Errorf("provider %s event references undeclared metadata namespace %q", providerID, mapping.MetadataNamespace)
			}
			if err := manifest.ValidateMetadataValue(mapping.MetadataNamespace, schemas[mapping.MetadataNamespace], metadata); err != nil {
				return fantasy.StreamPart{}, fmt.Errorf("provider %s metadata %q: %w", providerID, mapping.MetadataNamespace, err)
			}
			part.ProviderMetadata = fantasy.ProviderMetadata{mapping.MetadataNamespace: &metadata}
		}
	}
	return part, nil
}

type providerEventError struct {
	providerError *fantasy.ProviderError
	fields        map[string]string
}

func (e *providerEventError) Error() string {
	return e.providerError.Error()
}

func (e *providerEventError) Unwrap() error {
	return e.providerError
}

func (e *providerEventError) ProviderErrorField(pointer string) (string, bool) {
	value, ok := e.fields[pointer]
	return value, ok
}

func newProviderEventError(document any, mappings []manifest.ErrorMapping, message string) error {
	body, err := json.Marshal(document)
	if err != nil || len(body) > maxProviderErrorBodyBytes {
		body = []byte(`{"truncated":true}`)
	}
	fields := make(map[string]string)
	for _, mapping := range mappings {
		for _, pointer := range []string{mapping.CodePointer, mapping.MessagePointer} {
			if pointer == "" {
				continue
			}
			if value, ok := providerErrorField(providertransport.JSONPointer(document, pointer)); ok {
				fields[pointer] = value
			}
		}
	}
	return &providerEventError{
		providerError: &fantasy.ProviderError{Message: message, ResponseBody: body},
		fields:        fields,
	}
}

func providerErrorField(value any) (string, bool) {
	switch value := value.(type) {
	case string:
		return boundedProviderErrorMessage(value), true
	case json.Number:
		return value.String(), true
	case float64:
		return fmt.Sprintf("%v", value), true
	default:
		return "", false
	}
}

func boundedProviderErrorMessage(value string) string {
	runes := []rune(value)
	if len(runes) > 4096 {
		runes = runes[:4096]
	}
	return string(runes)
}

func metadataContract(contracts []manifest.MetadataContract, namespace string) (manifest.MetadataContract, bool) {
	for _, contract := range contracts {
		if contract.Namespace == namespace {
			return contract, true
		}
	}
	return manifest.MetadataContract{}, false
}

func usagePolicyForSource(policy *manifest.UsagePolicy, source string) *manifest.UsagePolicy {
	if policy == nil || policy.Source != source {
		return nil
	}
	return policy
}

func mapUsage(document any, policy *manifest.UsagePolicy, current fantasy.Usage) (fantasy.Usage, error) {
	if policy == nil {
		return current, nil
	}
	mapped := make(map[string]int64, len(policy.Mappings))
	for _, mapping := range policy.Mappings {
		raw, present, err := providertransport.LookupJSONPointer(document, mapping.Pointer)
		if err != nil {
			return current, fmt.Errorf("usage target %q cannot be read", mapping.Target)
		}
		if !present {
			continue
		}
		value, ok := usageInteger(raw)
		if !ok {
			return current, fmt.Errorf("usage target %q is not a non-negative integer", mapping.Target)
		}
		switch mapping.Operation {
		case "", "copy", "replace", "subtract-cache-read":
		case "accumulate":
			base := usageTargetValue(current, mapping.Target)
			var ok bool
			value, ok = addUsageValues(value, base)
			if !ok {
				return current, fmt.Errorf("usage target %q exceeds the supported range", mapping.Target)
			}
		default:
			return current, fmt.Errorf("usage target %q uses an unsupported operation", mapping.Target)
		}
		mapped[mapping.Target] = value
	}
	if value, ok := mapped["cache_read_tokens"]; ok {
		current.CacheReadTokens = value
	}
	if value, ok := mapped["cache_write_tokens"]; ok {
		current.CacheCreationTokens = value
	}
	if value, ok := mapped["input_tokens"]; ok {
		for _, mapping := range policy.Mappings {
			if mapping.Target == "input_tokens" && mapping.Operation == "subtract-cache-read" {
				value -= current.CacheReadTokens
				if value < 0 {
					value = 0
				}
				break
			}
		}
		current.InputTokens = value
	}
	if value, ok := mapped["output_tokens"]; ok {
		current.OutputTokens = value
	}
	if value, ok := mapped["reasoning_tokens"]; ok {
		current.ReasoningTokens = value
	}
	if value, ok := mapped["total_tokens"]; ok {
		current.TotalTokens = value
	} else {
		_, inputMapped := mapped["input_tokens"]
		_, outputMapped := mapped["output_tokens"]
		if inputMapped || outputMapped || current.TotalTokens == 0 {
			total, ok := addUsageValues(current.InputTokens, current.OutputTokens)
			if !ok {
				return current, fmt.Errorf("usage total exceeds the supported range")
			}
			current.TotalTokens = total
		}
	}
	return current, nil
}

func addUsageValues(first, second int64) (int64, bool) {
	const maximum = int64(^uint64(0) >> 1)
	if first < 0 || second < 0 || first > maximum-second {
		return 0, false
	}
	return first + second, true
}

func usageTargetValue(usage fantasy.Usage, target string) int64 {
	switch target {
	case "input_tokens":
		return usage.InputTokens
	case "output_tokens":
		return usage.OutputTokens
	case "reasoning_tokens":
		return usage.ReasoningTokens
	case "cache_read_tokens":
		return usage.CacheReadTokens
	case "cache_write_tokens":
		return usage.CacheCreationTokens
	case "total_tokens":
		return usage.TotalTokens
	default:
		return 0
	}
}

func usageInteger(value any) (int64, bool) {
	switch value := value.(type) {
	case int:
		return int64(value), value >= 0
	case int8:
		return int64(value), value >= 0
	case int16:
		return int64(value), value >= 0
	case int32:
		return int64(value), value >= 0
	case int64:
		return value, value >= 0
	case uint:
		if uint64(value) > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		if value > uint64(^uint64(0)>>1) {
			return 0, false
		}
		return int64(value), true
	case float32:
		return usageFloat(float64(value))
	case float64:
		return usageFloat(value)
	case json.Number:
		if integer, err := value.Int64(); err == nil {
			return integer, integer >= 0
		}
		decimal, err := value.Float64()
		if err != nil {
			return 0, false
		}
		return usageFloat(decimal)
	default:
		return 0, false
	}
}

func usageFloat(value float64) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value >= float64(uint64(1)<<63) || math.Trunc(value) != value {
		return 0, false
	}
	return int64(value), true
}

type responseCollector struct {
	providerID string
	response   fantasy.Response
	text       map[string]int
	reasoning  map[string]int
	finished   bool
}

func newResponseCollector(providerID string) *responseCollector {
	return &responseCollector{
		providerID: providerID,
		response:   fantasy.Response{FinishReason: fantasy.FinishReasonUnknown},
		text:       make(map[string]int),
		reasoning:  make(map[string]int),
	}
}

func (c *responseCollector) consume(part fantasy.StreamPart) error {
	switch part.Type {
	case fantasy.StreamPartTypeWarnings:
		c.response.Warnings = append(c.response.Warnings, part.Warnings...)
	case fantasy.StreamPartTypeTextStart:
		if _, exists := c.text[part.ID]; exists {
			return fmt.Errorf("provider %s response opened the same text frame more than once", c.providerID)
		}
		c.text[part.ID] = len(c.response.Content)
		c.response.Content = append(c.response.Content, fantasy.TextContent{ProviderMetadata: part.ProviderMetadata})
	case fantasy.StreamPartTypeTextDelta:
		index, exists := c.text[part.ID]
		if !exists {
			return fmt.Errorf("provider %s response emitted text without a matching start", c.providerID)
		}
		content, ok := fantasy.AsContentType[fantasy.TextContent](c.response.Content[index])
		if !ok {
			return fmt.Errorf("provider %s response text state is invalid", c.providerID)
		}
		content.Text += part.Delta
		metadata, err := mergeProviderMetadata(content.ProviderMetadata, part.ProviderMetadata)
		if err != nil {
			return fmt.Errorf("provider %s %w", c.providerID, err)
		}
		content.ProviderMetadata = metadata
		c.response.Content[index] = content
	case fantasy.StreamPartTypeTextEnd:
		index, exists := c.text[part.ID]
		if !exists {
			return fmt.Errorf("provider %s response ended text without a matching start", c.providerID)
		}
		content, ok := fantasy.AsContentType[fantasy.TextContent](c.response.Content[index])
		if !ok {
			return fmt.Errorf("provider %s response text state is invalid", c.providerID)
		}
		metadata, err := mergeProviderMetadata(content.ProviderMetadata, part.ProviderMetadata)
		if err != nil {
			return fmt.Errorf("provider %s %w", c.providerID, err)
		}
		content.ProviderMetadata = metadata
		c.response.Content[index] = content
		delete(c.text, part.ID)
	case fantasy.StreamPartTypeReasoningStart:
		if _, exists := c.reasoning[part.ID]; exists {
			return fmt.Errorf("provider %s response opened the same reasoning frame more than once", c.providerID)
		}
		c.reasoning[part.ID] = len(c.response.Content)
		c.response.Content = append(c.response.Content, fantasy.ReasoningContent{Text: part.Delta, ProviderMetadata: part.ProviderMetadata})
	case fantasy.StreamPartTypeReasoningDelta:
		index, exists := c.reasoning[part.ID]
		if !exists {
			return fmt.Errorf("provider %s response emitted reasoning without a matching start", c.providerID)
		}
		content, ok := fantasy.AsContentType[fantasy.ReasoningContent](c.response.Content[index])
		if !ok {
			return fmt.Errorf("provider %s response reasoning state is invalid", c.providerID)
		}
		content.Text += part.Delta
		metadata, err := mergeProviderMetadata(content.ProviderMetadata, part.ProviderMetadata)
		if err != nil {
			return fmt.Errorf("provider %s %w", c.providerID, err)
		}
		content.ProviderMetadata = metadata
		c.response.Content[index] = content
	case fantasy.StreamPartTypeReasoningEnd:
		index, exists := c.reasoning[part.ID]
		if !exists {
			return fmt.Errorf("provider %s response ended reasoning without a matching start", c.providerID)
		}
		content, ok := fantasy.AsContentType[fantasy.ReasoningContent](c.response.Content[index])
		if !ok {
			return fmt.Errorf("provider %s response reasoning state is invalid", c.providerID)
		}
		metadata, err := mergeProviderMetadata(content.ProviderMetadata, part.ProviderMetadata)
		if err != nil {
			return fmt.Errorf("provider %s %w", c.providerID, err)
		}
		content.ProviderMetadata = metadata
		c.response.Content[index] = content
		delete(c.reasoning, part.ID)
	case fantasy.StreamPartTypeToolInputStart, fantasy.StreamPartTypeToolInputDelta, fantasy.StreamPartTypeToolInputEnd:
	case fantasy.StreamPartTypeToolCall:
		c.response.Content = append(c.response.Content, fantasy.ToolCallContent{
			ToolCallID:       part.ID,
			ToolName:         part.ToolCallName,
			Input:            part.ToolCallInput,
			ProviderExecuted: part.ProviderExecuted,
			ProviderMetadata: part.ProviderMetadata,
		})
	case fantasy.StreamPartTypeToolResult:
		c.response.Content = append(c.response.Content, fantasy.ToolResultContent{
			ToolCallID:       part.ID,
			ToolName:         part.ToolCallName,
			ProviderExecuted: part.ProviderExecuted,
			ProviderMetadata: part.ProviderMetadata,
		})
	case fantasy.StreamPartTypeSource:
		c.response.Content = append(c.response.Content, fantasy.SourceContent{
			SourceType:       part.SourceType,
			ID:               part.ID,
			URL:              part.URL,
			Title:            part.Title,
			ProviderMetadata: part.ProviderMetadata,
		})
	case fantasy.StreamPartTypeFinish:
		if c.finished {
			return fmt.Errorf("provider %s response emitted more than one finish event", c.providerID)
		}
		if len(c.text) > 0 || len(c.reasoning) > 0 {
			return fmt.Errorf("provider %s response finished with incomplete content", c.providerID)
		}
		c.response.FinishReason = part.FinishReason
		c.response.Usage = part.Usage
		c.response.ProviderMetadata = part.ProviderMetadata
		c.finished = true
	case fantasy.StreamPartTypeError:
		if part.Error != nil {
			return part.Error
		}
		return fmt.Errorf("provider %s returned an unspecified stream error", c.providerID)
	default:
		return fmt.Errorf("provider %s returned unsupported stream part type %q", c.providerID, part.Type)
	}
	return nil
}

func streamResponse(response *fantasy.Response) fantasy.StreamResponse {
	return func(yield func(fantasy.StreamPart) bool) {
		if response == nil {
			yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response is unavailable")})
			return
		}
		if len(response.Warnings) > 0 && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeWarnings, Warnings: response.Warnings}) {
			return
		}
		for index, raw := range response.Content {
			if raw == nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains unavailable content")})
				return
			}
			switch raw.GetType() {
			case fantasy.ContentTypeText:
				content, ok := fantasy.AsContentType[fantasy.TextContent](raw)
				if !ok {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains invalid text content")})
					return
				}
				id := fmt.Sprintf("declarative-text-%d", index)
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: id}) {
					return
				}
				if content.Text != "" && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextDelta, ID: id, Delta: content.Text}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: id, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
			case fantasy.ContentTypeReasoning:
				content, ok := fantasy.AsContentType[fantasy.ReasoningContent](raw)
				if !ok {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains invalid reasoning content")})
					return
				}
				id := fmt.Sprintf("declarative-reasoning-%d", index)
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: id, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
				if content.Text != "" && !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningDelta, ID: id, Delta: content.Text}) {
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: id, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
			case fantasy.ContentTypeToolCall:
				content, ok := fantasy.AsContentType[fantasy.ToolCallContent](raw)
				if !ok {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains invalid tool-call content")})
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolCall, ID: content.ToolCallID, ToolCallName: content.ToolName, ToolCallInput: content.Input, ProviderExecuted: content.ProviderExecuted, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
			case fantasy.ContentTypeToolResult:
				content, ok := fantasy.AsContentType[fantasy.ToolResultContent](raw)
				if !ok {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains invalid tool-result content")})
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeToolResult, ID: content.ToolCallID, ToolCallName: content.ToolName, ProviderExecuted: content.ProviderExecuted, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
			case fantasy.ContentTypeSource:
				content, ok := fantasy.AsContentType[fantasy.SourceContent](raw)
				if !ok {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains invalid source content")})
					return
				}
				if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeSource, ID: content.ID, SourceType: content.SourceType, URL: content.URL, Title: content.Title, ProviderMetadata: content.ProviderMetadata}) {
					return
				}
			case fantasy.ContentTypeFile:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response file content cannot be represented by declarative streaming")})
				return
			default:
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("provider response contains unsupported content type %q", raw.GetType())})
				return
			}
		}
		yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeFinish, Usage: response.Usage, FinishReason: response.FinishReason, ProviderMetadata: response.ProviderMetadata})
	}
}

func objectCall(call fantasy.ObjectCall) fantasy.Call {
	return fantasy.Call{Prompt: call.Prompt, MaxOutputTokens: call.MaxOutputTokens, Temperature: call.Temperature, TopP: call.TopP, TopK: call.TopK, PresencePenalty: call.PresencePenalty, FrequencyPenalty: call.FrequencyPenalty, UserAgent: call.UserAgent, Headers: call.Headers, ProviderOptions: call.ProviderOptions}
}

func toolOutput(output fantasy.ToolResultOutputContent) any {
	switch value := output.(type) {
	case fantasy.ToolResultOutputContentText:
		return value.Text
	case fantasy.ToolResultOutputContentError:
		return map[string]any{"error": value.Error.Error()}
	case fantasy.ToolResultOutputContentMedia:
		return map[string]any{"text": value.Text, "media_type": value.MediaType, "data": value.Data}
	default:
		return nil
	}
}

func finishReason(value string) fantasy.FinishReason {
	switch strings.ToLower(value) {
	case "stop", "completed", "end_turn":
		return fantasy.FinishReasonStop
	case "length", "max_tokens":
		return fantasy.FinishReasonLength
	case "content-filter", "safety":
		return fantasy.FinishReasonContentFilter
	case "tool-calls", "tool_use":
		return fantasy.FinishReasonToolCalls
	case "error", "failed":
		return fantasy.FinishReasonError
	case "":
		return fantasy.FinishReasonUnknown
	default:
		return fantasy.FinishReasonOther
	}
}

func stringField(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func setOptional[T any](document map[string]any, name string, value *T) {
	if value != nil {
		document[name] = *value
	}
}
func array(value any) []any { result, _ := value.([]any); return result }

func decodeJSONDocument(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("response contains multiple JSON values")
		}
		return nil, err
	}
	return document, nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return data, nil
}

func readProviderErrorBody(reader io.Reader) ([]byte, error) {
	return io.ReadAll(io.LimitReader(reader, maxProviderErrorBodyBytes))
}

func cloneHeaders(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}

func responseHeaders(values http.Header) map[string]string {
	result := make(map[string]string, len(values))
	for name, entries := range values {
		result[name] = strings.Join(entries, ", ")
	}
	return result
}

var _ iter.Seq[fantasy.StreamPart]
