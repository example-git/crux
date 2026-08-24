// Package antigravity implements a fantasy provider for the Antigravity
// Cloud Code endpoint, which serves its whole model lineup (Gemini, GPT-OSS,
// and Claude ids) over the Gemini generateContent protocol.
//
// Unlike a stock Gemini SDK client, this provider speaks the Antigravity
// dialect natively: it builds the request envelope itself, sends tool
// results as model-role turns, echoes thought signatures on every part that
// needs one, and parses the endpoint's wrapped SSE stream directly. See
// wire.go for the request/response shapes and client.go for transport and
// retry behavior.
package antigravity

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/object"
	"github.com/example-git/crux/foundation/schema"
	"github.com/google/uuid"
)

// Name is the name of the Antigravity provider.
const Name = "antigravity"

type provider struct {
	options options
}

// ToolCallIDFunc defines a function that generates a tool call ID.
type ToolCallIDFunc = func() string

type options struct {
	name           string
	baseURL        string
	headers        map[string]string
	userAgent      string
	client         *http.Client
	token          TokenSource
	project        ProjectLoader
	toolCallIDFunc ToolCallIDFunc
	objectMode     fantasy.ObjectMode
}

// Option defines a function that configures Antigravity provider options.
type Option = func(*options)

// New creates a new Antigravity provider with the given options.
func New(opts ...Option) (fantasy.Provider, error) {
	options := options{
		headers: map[string]string{},
		toolCallIDFunc: func() string {
			return uuid.NewString()
		},
	}
	for _, o := range opts {
		o(&options)
	}

	options.name = cmp.Or(options.name, Name)

	return &provider{
		options: options,
	}, nil
}

// WithBaseURL sets the base URL for the Antigravity provider.
func WithBaseURL(baseURL string) Option {
	return func(o *options) {
		o.baseURL = baseURL
	}
}

// WithName sets the name for the Antigravity provider.
func WithName(name string) Option {
	return func(o *options) {
		o.name = name
	}
}

// WithHeaders sets the headers for the Antigravity provider.
func WithHeaders(headers map[string]string) Option {
	return func(o *options) {
		maps.Copy(o.headers, headers)
	}
}

// WithHTTPClient sets the HTTP client for the Antigravity provider.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) {
		o.client = client
	}
}

// WithTokenSource sets the OAuth token source; the token is read per request
// so refreshed credentials are picked up automatically.
func WithTokenSource(token TokenSource) Option {
	return func(o *options) {
		o.token = token
	}
}

// WithProjectLoader sets the loader that resolves the Cloud AI Companion
// project carried in the request envelope.
func WithProjectLoader(project ProjectLoader) Option {
	return func(o *options) {
		o.project = project
	}
}

// WithToolCallIDFunc sets the function that generates a tool call ID.
func WithToolCallIDFunc(f ToolCallIDFunc) Option {
	return func(o *options) {
		o.toolCallIDFunc = f
	}
}

// WithUserAgent sets an explicit User-Agent header, overriding the default
// and any value set via WithHeaders.
func WithUserAgent(ua string) Option {
	return func(o *options) {
		o.userAgent = ua
	}
}

// WithObjectMode sets the object generation mode for the provider.
func WithObjectMode(om fantasy.ObjectMode) Option {
	return func(o *options) {
		o.objectMode = om
	}
}

func (*provider) Name() string {
	return Name
}

type languageModel struct {
	provider        string
	modelID         string
	client          *client
	providerOptions options
	objectMode      fantasy.ObjectMode
}

// LanguageModel implements fantasy.Provider.
func (a *provider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	resolved := resolveHeaders(a.options.headers, a.options.userAgent, defaultUserAgent(fantasy.Version))
	userAgent := resolved["User-Agent"]
	delete(resolved, "User-Agent")

	objectMode := a.options.objectMode
	if objectMode == "" {
		objectMode = fantasy.ObjectModeAuto
	}

	return &languageModel{
		modelID:         modelID,
		provider:        a.options.name,
		providerOptions: a.options,
		objectMode:      objectMode,
		client: &client{
			httpClient: a.options.client,
			baseURL:    a.options.baseURL,
			token:      a.options.token,
			project:    a.options.project,
			userAgent:  userAgent,
			headers:    resolved,
		},
	}, nil
}

// callClient returns the client to use for a single call, applying per-call
// header overrides when present. The User-Agent is never overridable: the
// Antigravity endpoint licenses by client identity and rejects requests with
// any other UA (403 #3501 SUBSCRIPTION_REQUIRED).
func (g *languageModel) callClient(call fantasy.Call) *client {
	if len(call.Headers) == 0 {
		return g.client
	}
	c := *g.client
	headers := maps.Clone(c.headers)
	if headers == nil {
		headers = map[string]string{}
	}
	maps.Copy(headers, call.Headers)
	delete(headers, "User-Agent")
	c.headers = headers
	return &c
}

// OAuth-path sampling defaults matching the Antigravity client; the endpoint
// misbehaves without them.
const (
	defaultMaxOutputTokens int64   = 8192
	defaultTemperature     float64 = 1
	defaultTopP            float64 = 0.95
	defaultTopK            int64   = 64
)

func (g *languageModel) prepareRequest(call fantasy.Call) (*wireRequest, []fantasy.CallWarning, error) {
	providerOptions := &ProviderOptions{}
	if v, ok := call.ProviderOptions[Name]; ok {
		providerOptions, ok = v.(*ProviderOptions)
		if !ok {
			return nil, nil, &fantasy.Error{Title: "invalid argument", Message: "antigravity provider options should be *antigravity.ProviderOptions"}
		}
	}

	system, contents, warnings := toWirePrompt(call.Prompt)
	if len(contents) == 0 {
		return nil, nil, errors.New("no messages to send")
	}

	isGPTOSS := strings.HasPrefix(g.modelID, "gpt-oss-")
	genCfg := &wireGenerationConfig{
		Temperature: ptr(defaultTemperature),
	}
	if !isGPTOSS {
		genCfg.MaxOutputTokens = defaultMaxOutputTokens
		genCfg.TopP = ptr(defaultTopP)
		genCfg.TopK = ptr(defaultTopK)
	}
	if !isGPTOSS && call.MaxOutputTokens != nil {
		genCfg.MaxOutputTokens = *call.MaxOutputTokens
	}
	if call.Temperature != nil {
		genCfg.Temperature = call.Temperature
	}
	if !isGPTOSS && call.TopP != nil {
		genCfg.TopP = call.TopP
	}
	if !isGPTOSS && call.TopK != nil {
		genCfg.TopK = call.TopK
	}
	genCfg.PresencePenalty = call.PresencePenalty
	genCfg.FrequencyPenalty = call.FrequencyPenalty

	if tc := providerOptions.ThinkingConfig; tc != nil {
		if tc.ThinkingLevel != nil && tc.ThinkingBudget != nil {
			return nil, nil, &fantasy.Error{
				Title:   "invalid argument",
				Message: "thinking_level and thinking_budget are mutually exclusive",
			}
		}
		if !isGPTOSS {
			wtc := &wireThinkingConfig{}
			if tc.IncludeThoughts != nil {
				wtc.IncludeThoughts = *tc.IncludeThoughts
			}
			if tc.ThinkingBudget != nil {
				budget := *tc.ThinkingBudget
				if budget < 128 {
					warnings = append(warnings, fantasy.CallWarning{
						Type:    fantasy.CallWarningTypeOther,
						Message: "The 'thinking_budget' option can not be under 128 and will be set to 128 by default",
					})
					budget = 128
				}
				wtc.ThinkingBudget = &budget
			}
			if tc.ThinkingLevel != nil {
				wtc.ThinkingLevel = *tc.ThinkingLevel
			}
			genCfg.ThinkingConfig = wtc
		}
	}

	req := &wireRequest{
		Contents:          contents,
		SystemInstruction: system,
		GenerationConfig:  genCfg,
		CachedContent:     providerOptions.CachedContent,
		SafetySettings:    providerOptions.SafetySettings,
	}

	if len(call.Tools) > 0 {
		tools, toolConfig, toolWarnings := toWireTools(call.Tools, call.ToolChoice)
		req.Tools = tools
		req.ToolConfig = toolConfig
		warnings = append(warnings, toolWarnings...)
	}

	return req, warnings, nil
}

func ptr[T any](v T) *T { return &v }

// Generate implements fantasy.LanguageModel.
func (g *languageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	req, warnings, err := g.prepareRequest(call)
	if err != nil {
		return nil, err
	}
	resp, err := g.callClient(call).generateContent(ctx, g.modelID, req)
	if err != nil {
		return nil, err
	}
	return g.mapResponse(resp, warnings)
}

// Model implements fantasy.LanguageModel.
func (g *languageModel) Model() string {
	return g.modelID
}

// Provider implements fantasy.LanguageModel.
func (g *languageModel) Provider() string {
	return g.provider
}

// Stream implements fantasy.LanguageModel.
func (g *languageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	req, warnings, err := g.prepareRequest(call)
	if err != nil {
		return nil, err
	}

	chunks, err := g.callClient(call).streamGenerateContent(ctx, g.modelID, req)
	if err != nil {
		return nil, err
	}

	return func(yield func(fantasy.StreamPart) bool) {
		if len(warnings) > 0 {
			if !yield(fantasy.StreamPart{
				Type:     fantasy.StreamPartTypeWarnings,
				Warnings: warnings,
			}) {
				return
			}
		}

		var toolCalls []fantasy.ToolCallContent
		var isActiveText bool
		var isActiveReasoning bool
		var blockCounter int
		var currentTextBlockID string
		var currentReasoningBlockID string
		var currentReasoningSignature string
		var usage *fantasy.Usage
		var lastFinishReason fantasy.FinishReason

		endText := func() bool {
			isActiveText = false
			return yield(fantasy.StreamPart{
				Type: fantasy.StreamPartTypeTextEnd,
				ID:   currentTextBlockID,
			})
		}
		var signatureDelivered bool
		endReasoning := func(toolID string) bool {
			isActiveReasoning = false
			part := fantasy.StreamPart{
				Type: fantasy.StreamPartTypeReasoningEnd,
				ID:   currentReasoningBlockID,
			}
			if currentReasoningSignature != "" {
				part.ProviderMetadata = fantasy.ProviderMetadata{
					Name: &ReasoningMetadata{
						Signature: currentReasoningSignature,
						ToolID:    toolID,
					},
				}
				signatureDelivered = true
			}
			currentReasoningSignature = ""
			return yield(part)
		}
		// Signatures can arrive on functionCall or text parts with no open
		// reasoning block; emit a signature-only block so they still reach
		// the message store and get echoed back on the next request.
		flushOrphanSignature := func(toolID string) bool {
			if isActiveReasoning || currentReasoningSignature == "" || signatureDelivered {
				return true
			}
			currentReasoningBlockID = fmt.Sprintf("%d", blockCounter)
			blockCounter++
			if !yield(fantasy.StreamPart{
				Type: fantasy.StreamPartTypeReasoningStart,
				ID:   currentReasoningBlockID,
			}) {
				return false
			}
			return endReasoning(toolID)
		}

		for chunk, err := range chunks {
			if err != nil {
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: err,
				})
				return
			}

			if len(chunk.Candidates) > 0 && chunk.Candidates[0].Content != nil {
				for _, part := range chunk.Candidates[0].Content.Parts {
					switch {
					case part.Thought:
						if isActiveText && !endText() {
							return
						}
						if !isActiveReasoning {
							isActiveReasoning = true
							currentReasoningBlockID = fmt.Sprintf("%d", blockCounter)
							blockCounter++
							if !yield(fantasy.StreamPart{
								Type: fantasy.StreamPartTypeReasoningStart,
								ID:   currentReasoningBlockID,
							}) {
								return
							}
						}
						if part.ThoughtSignature != "" {
							currentReasoningSignature = part.ThoughtSignature
						}
						if part.Text != "" {
							if !yield(fantasy.StreamPart{
								Type:  fantasy.StreamPartTypeReasoningDelta,
								ID:    currentReasoningBlockID,
								Delta: part.Text,
							}) {
								return
							}
						}
					case part.Text != "":
						if part.ThoughtSignature != "" {
							currentReasoningSignature = part.ThoughtSignature
						}
						if isActiveReasoning && !endReasoning("") {
							return
						}
						if !isActiveText {
							isActiveText = true
							currentTextBlockID = fmt.Sprintf("%d", blockCounter)
							blockCounter++
							if !yield(fantasy.StreamPart{
								Type: fantasy.StreamPartTypeTextStart,
								ID:   currentTextBlockID,
							}) {
								return
							}
						}
						if !yield(fantasy.StreamPart{
							Type:  fantasy.StreamPartTypeTextDelta,
							ID:    currentTextBlockID,
							Delta: part.Text,
						}) {
							return
						}
					case part.FunctionCall != nil:
						if isActiveText && !endText() {
							return
						}
						toolCallID := cmp.Or(part.FunctionCall.ID, g.providerOptions.toolCallIDFunc())
						if part.ThoughtSignature != "" {
							currentReasoningSignature = part.ThoughtSignature
						}
						if isActiveReasoning && !endReasoning(toolCallID) {
							return
						}
						if !flushOrphanSignature(toolCallID) {
							return
						}
						args, err := json.Marshal(part.FunctionCall.Args)
						if err != nil {
							yield(fantasy.StreamPart{
								Type:  fantasy.StreamPartTypeError,
								Error: err,
							})
							return
						}

						if !yield(fantasy.StreamPart{
							Type:         fantasy.StreamPartTypeToolInputStart,
							ID:           toolCallID,
							ToolCallName: part.FunctionCall.Name,
						}) {
							return
						}
						if !yield(fantasy.StreamPart{
							Type:  fantasy.StreamPartTypeToolInputDelta,
							ID:    toolCallID,
							Delta: string(args),
						}) {
							return
						}
						if !yield(fantasy.StreamPart{
							Type: fantasy.StreamPartTypeToolInputEnd,
							ID:   toolCallID,
						}) {
							return
						}
						if !yield(fantasy.StreamPart{
							Type:             fantasy.StreamPartTypeToolCall,
							ID:               toolCallID,
							ToolCallName:     part.FunctionCall.Name,
							ToolCallInput:    string(args),
							ProviderExecuted: false,
						}) {
							return
						}

						toolCalls = append(toolCalls, fantasy.ToolCallContent{
							ToolCallID:       toolCallID,
							ToolName:         part.FunctionCall.Name,
							Input:            string(args),
							ProviderExecuted: false,
						})
					}
				}
			}

			if chunk.UsageMetadata != nil && chunk.UsageMetadata.TotalTokenCount != 0 {
				currentUsage := mapUsage(chunk.UsageMetadata)
				if usage == nil {
					usage = &currentUsage
				} else {
					usage.OutputTokens += currentUsage.OutputTokens
					usage.ReasoningTokens += currentUsage.ReasoningTokens
					usage.CacheReadTokens += currentUsage.CacheReadTokens
				}
			}

			if len(chunk.Candidates) > 0 && chunk.Candidates[0].FinishReason != "" {
				lastFinishReason = mapFinishReason(chunk.Candidates[0].FinishReason)
			}
		}

		// Close any open blocks before finishing.
		if isActiveText && !endText() {
			return
		}
		if isActiveReasoning && !endReasoning("") {
			return
		}
		if !flushOrphanSignature("") {
			return
		}

		finishReason := lastFinishReason
		if len(toolCalls) > 0 {
			finishReason = fantasy.FinishReasonToolCalls
		} else if finishReason == "" {
			// Truncated stream: no candidate emitted a finishReason before
			// close. Surface as a retryable error.
			yield(fantasy.StreamPart{
				Type:  fantasy.StreamPartTypeError,
				Error: fantasy.NewIncompleteStreamError(),
			})
			return
		}

		var finalUsage fantasy.Usage
		if usage != nil {
			finalUsage = *usage
		}

		yield(fantasy.StreamPart{
			Type:         fantasy.StreamPartTypeFinish,
			Usage:        finalUsage,
			FinishReason: finishReason,
		})
	}, nil
}

// GenerateObject implements fantasy.LanguageModel.
func (g *languageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	switch g.objectMode {
	case fantasy.ObjectModeText:
		return object.GenerateWithText(ctx, g, call)
	case fantasy.ObjectModeTool:
		return object.GenerateWithTool(ctx, g, call)
	default:
		return g.generateObjectWithJSONMode(ctx, call)
	}
}

// StreamObject implements fantasy.LanguageModel.
func (g *languageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	switch g.objectMode {
	case fantasy.ObjectModeTool:
		return object.StreamWithTool(ctx, g, call)
	case fantasy.ObjectModeText:
		return object.StreamWithText(ctx, g, call)
	default:
		return g.streamObjectWithJSONMode(ctx, call)
	}
}

func objectCallToCall(call fantasy.ObjectCall) fantasy.Call {
	return fantasy.Call{
		Prompt:           call.Prompt,
		MaxOutputTokens:  call.MaxOutputTokens,
		Temperature:      call.Temperature,
		TopP:             call.TopP,
		TopK:             call.TopK,
		PresencePenalty:  call.PresencePenalty,
		FrequencyPenalty: call.FrequencyPenalty,
		ProviderOptions:  call.ProviderOptions,
		UserAgent:        call.UserAgent,
		Headers:          call.Headers,
	}
}

func (g *languageModel) generateObjectWithJSONMode(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	fantasyCall := objectCallToCall(call)
	req, warnings, err := g.prepareRequest(fantasyCall)
	if err != nil {
		return nil, err
	}
	req.GenerationConfig.ResponseMIMEType = "application/json"
	req.GenerationConfig.ResponseJSONSchema = schema.ToMap(call.Schema)

	resp, err := g.callClient(fantasyCall).generateContent(ctx, g.modelID, req)
	if err != nil {
		return nil, err
	}

	mappedResponse, err := g.mapResponse(resp, warnings)
	if err != nil {
		return nil, err
	}

	jsonText := mappedResponse.Content.Text()
	if jsonText == "" {
		return nil, &fantasy.NoObjectGeneratedError{
			RawText:      "",
			ParseError:   fmt.Errorf("no text content in response"),
			Usage:        mappedResponse.Usage,
			FinishReason: mappedResponse.FinishReason,
		}
	}

	var obj any
	if call.RepairText != nil {
		obj, err = schema.ParseAndValidateWithRepair(ctx, jsonText, call.Schema, call.RepairText)
	} else {
		obj, err = schema.ParseAndValidate(jsonText, call.Schema)
	}
	if err != nil {
		if nogErr, ok := err.(*fantasy.NoObjectGeneratedError); ok {
			nogErr.Usage = mappedResponse.Usage
			nogErr.FinishReason = mappedResponse.FinishReason
		}
		return nil, err
	}

	return &fantasy.ObjectResponse{
		Object:           obj,
		RawText:          jsonText,
		Usage:            mappedResponse.Usage,
		FinishReason:     mappedResponse.FinishReason,
		Warnings:         warnings,
		ProviderMetadata: mappedResponse.ProviderMetadata,
	}, nil
}

func (g *languageModel) streamObjectWithJSONMode(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	fantasyCall := objectCallToCall(call)
	req, warnings, err := g.prepareRequest(fantasyCall)
	if err != nil {
		return nil, err
	}
	req.GenerationConfig.ResponseMIMEType = "application/json"
	req.GenerationConfig.ResponseJSONSchema = schema.ToMap(call.Schema)

	chunks, err := g.callClient(fantasyCall).streamGenerateContent(ctx, g.modelID, req)
	if err != nil {
		return nil, err
	}

	return func(yield func(fantasy.ObjectStreamPart) bool) {
		if len(warnings) > 0 {
			if !yield(fantasy.ObjectStreamPart{
				Type:     fantasy.ObjectStreamPartTypeObject,
				Warnings: warnings,
			}) {
				return
			}
		}

		var accumulated strings.Builder
		var lastParsedObject any
		var usage *fantasy.Usage
		var lastFinishReason fantasy.FinishReason
		var streamErr error

		for chunk, err := range chunks {
			if err != nil {
				streamErr = err
				yield(fantasy.ObjectStreamPart{
					Type:  fantasy.ObjectStreamPartTypeError,
					Error: err,
				})
				return
			}

			if len(chunk.Candidates) > 0 && chunk.Candidates[0].Content != nil {
				for _, part := range chunk.Candidates[0].Content.Parts {
					if part.Text == "" || part.Thought {
						continue
					}
					accumulated.WriteString(part.Text)

					obj, state, parseErr := schema.ParsePartialJSON(accumulated.String())
					if state == schema.ParseStateSuccessful || state == schema.ParseStateRepaired {
						if err := schema.ValidateAgainstSchema(obj, call.Schema); err == nil {
							if !reflect.DeepEqual(obj, lastParsedObject) {
								if !yield(fantasy.ObjectStreamPart{
									Type:   fantasy.ObjectStreamPartTypeObject,
									Object: obj,
								}) {
									return
								}
								lastParsedObject = obj
							}
						}
					}

					if state == schema.ParseStateFailed && call.RepairText != nil {
						repairedText, repairErr := call.RepairText(ctx, accumulated.String(), parseErr)
						if repairErr == nil {
							obj2, state2, _ := schema.ParsePartialJSON(repairedText)
							if (state2 == schema.ParseStateSuccessful || state2 == schema.ParseStateRepaired) &&
								schema.ValidateAgainstSchema(obj2, call.Schema) == nil {
								if !reflect.DeepEqual(obj2, lastParsedObject) {
									if !yield(fantasy.ObjectStreamPart{
										Type:   fantasy.ObjectStreamPartTypeObject,
										Object: obj2,
									}) {
										return
									}
									lastParsedObject = obj2
								}
							}
						}
					}
				}
			}

			if chunk.UsageMetadata != nil && chunk.UsageMetadata.TotalTokenCount != 0 {
				currentUsage := mapUsage(chunk.UsageMetadata)
				if usage == nil {
					usage = &currentUsage
				} else {
					usage.OutputTokens += currentUsage.OutputTokens
					usage.ReasoningTokens += currentUsage.ReasoningTokens
					usage.CacheReadTokens += currentUsage.CacheReadTokens
				}
			}

			if len(chunk.Candidates) > 0 && chunk.Candidates[0].FinishReason != "" {
				lastFinishReason = mapFinishReason(chunk.Candidates[0].FinishReason)
			}
		}

		var finalUsage fantasy.Usage
		if usage != nil {
			finalUsage = *usage
		}
		if streamErr == nil && lastParsedObject != nil {
			yield(fantasy.ObjectStreamPart{
				Type:         fantasy.ObjectStreamPartTypeFinish,
				Usage:        finalUsage,
				FinishReason: cmp.Or(lastFinishReason, fantasy.FinishReasonStop),
			})
		} else if streamErr == nil {
			yield(fantasy.ObjectStreamPart{
				Type: fantasy.ObjectStreamPartTypeError,
				Error: &fantasy.NoObjectGeneratedError{
					RawText:      accumulated.String(),
					ParseError:   fmt.Errorf("no valid object generated in stream"),
					Usage:        finalUsage,
					FinishReason: lastFinishReason,
				},
			})
		}
	}, nil
}

func (g *languageModel) mapResponse(response *wireResponse, warnings []fantasy.CallWarning) (*fantasy.Response, error) {
	if len(response.Candidates) == 0 || response.Candidates[0].Content == nil {
		return nil, errors.New("no response from model")
	}

	var (
		content      []fantasy.Content
		finishReason fantasy.FinishReason
		hasToolCalls bool
		candidate    = response.Candidates[0]
	)

	// attachSignature binds a signature to the most recent unsigned
	// reasoning block, or synthesizes an empty one so the signature is
	// preserved for the next turn.
	attachSignature := func(signature, toolID string) {
		metadata := &ReasoningMetadata{Signature: signature, ToolID: toolID}
		for i := len(content) - 1; i >= 0; i-- {
			reasoningContent, ok := fantasy.AsContentType[fantasy.ReasoningContent](content[i])
			if !ok {
				continue
			}
			if reasoningContent.ProviderMetadata == nil || reasoningContent.ProviderMetadata[Name] == nil {
				reasoningContent.ProviderMetadata = fantasy.ProviderMetadata{Name: metadata}
				content[i] = reasoningContent
				return
			}
		}
		content = append(content, fantasy.ReasoningContent{
			ProviderMetadata: fantasy.ProviderMetadata{Name: metadata},
		})
	}

	for _, part := range candidate.Content.Parts {
		switch {
		case part.Thought:
			reasoningContent := fantasy.ReasoningContent{Text: part.Text}
			if part.ThoughtSignature != "" {
				reasoningContent.ProviderMetadata = fantasy.ProviderMetadata{
					Name: &ReasoningMetadata{Signature: part.ThoughtSignature},
				}
			}
			content = append(content, reasoningContent)
		case part.Text != "":
			if part.ThoughtSignature != "" {
				attachSignature(part.ThoughtSignature, "")
			}
			content = append(content, fantasy.TextContent{Text: part.Text})
		case part.FunctionCall != nil:
			input, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				return nil, err
			}
			toolCallID := cmp.Or(part.FunctionCall.ID, g.providerOptions.toolCallIDFunc())
			if part.ThoughtSignature != "" {
				attachSignature(part.ThoughtSignature, toolCallID)
			}
			content = append(content, fantasy.ToolCallContent{
				ToolCallID:       toolCallID,
				ToolName:         part.FunctionCall.Name,
				Input:            string(input),
				ProviderExecuted: false,
			})
			hasToolCalls = true
		}
	}

	if hasToolCalls {
		finishReason = fantasy.FinishReasonToolCalls
	} else {
		finishReason = mapFinishReason(candidate.FinishReason)
	}

	return &fantasy.Response{
		Content:      content,
		Usage:        mapUsage(response.UsageMetadata),
		FinishReason: finishReason,
		Warnings:     warnings,
	}, nil
}

// GetReasoningMetadata extracts reasoning metadata from provider options.
func GetReasoningMetadata(providerOptions fantasy.ProviderOptions) *ReasoningMetadata {
	if googleOptions, ok := providerOptions[Name]; ok {
		if reasoning, ok := googleOptions.(*ReasoningMetadata); ok {
			return reasoning
		}
	}
	return nil
}

func mapFinishReason(reason string) fantasy.FinishReason {
	switch reason {
	case "STOP":
		return fantasy.FinishReasonStop
	case "MAX_TOKENS":
		return fantasy.FinishReasonLength
	case "SAFETY", "BLOCKLIST", "PROHIBITED_CONTENT", "SPII", "IMAGE_SAFETY":
		return fantasy.FinishReasonContentFilter
	case "RECITATION", "LANGUAGE", "MALFORMED_FUNCTION_CALL":
		return fantasy.FinishReasonError
	case "OTHER":
		return fantasy.FinishReasonOther
	default:
		return fantasy.FinishReasonUnknown
	}
}

func mapUsage(usage *wireUsage) fantasy.Usage {
	if usage == nil {
		return fantasy.Usage{}
	}
	return fantasy.Usage{
		InputTokens:     usage.PromptTokenCount,
		OutputTokens:    usage.CandidatesTokenCount,
		TotalTokens:     usage.TotalTokenCount,
		ReasoningTokens: usage.ThoughtsTokenCount,
		CacheReadTokens: usage.CachedContentTokenCount,
	}
}
