// Package responses is a native fantasy adapter for the Codex Responses API
// over WebSocket. It reuses conversation-scoped connections and chains
// compatible completed responses while preserving full-history fallback.
package responses

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/object"
)

// Name is the name of the Codex provider adapter.
const Name = "codex"

type provider struct {
	options options
}

type options struct {
	name         string
	url          string
	token        TokenSource
	accountID    AccountIDSource
	userAgent    string
	originator   string
	version      string
	headers      map[string]string
	sessionStore *SessionStore
}

// Option configures the Codex provider.
type Option = func(*options)

// New creates a new Codex fantasy provider.
func New(opts ...Option) (fantasy.Provider, error) {
	options := options{headers: map[string]string{}}
	for _, o := range opts {
		o(&options)
	}
	options.name = cmp.Or(options.name, Name)
	return &provider{options: options}, nil
}

// WithURL sets the WebSocket endpoint URL.
func WithURL(url string) Option {
	return func(o *options) { o.url = url }
}

// WithName sets the provider name.
func WithName(name string) Option {
	return func(o *options) { o.name = name }
}

// WithTokenSource sets the OAuth token source; the token is read per request
// so refreshed credentials are picked up automatically.
func WithTokenSource(token TokenSource) Option {
	return func(o *options) { o.token = token }
}

// WithAccountIDSource sets the selected ChatGPT account id source. Codex
// stores this value alongside, rather than necessarily inside, its OAuth JWT.
func WithAccountIDSource(accountID AccountIDSource) Option {
	return func(o *options) { o.accountID = accountID }
}

// WithUserAgent sets the User-Agent presented on the WebSocket handshake.
func WithUserAgent(ua string) Option {
	return func(o *options) { o.userAgent = ua }
}

// WithOriginator sets the originator identity header.
func WithOriginator(originator string) Option {
	return func(o *options) { o.originator = originator }
}

// WithVersion sets the client version header.
func WithVersion(version string) Option {
	return func(o *options) { o.version = version }
}

// WithHeaders sets extra headers for the WebSocket handshake.
func WithHeaders(headers map[string]string) Option {
	return func(o *options) {
		for k, v := range headers {
			o.headers[k] = v
		}
	}
}

func WithSessionStore(store *SessionStore) Option {
	return func(o *options) { o.sessionStore = store }
}

// Name implements fantasy.Provider.
func (p *provider) Name() string { return p.options.name }

// LanguageModel implements fantasy.Provider.
func (p *provider) LanguageModel(_ context.Context, modelID string) (fantasy.LanguageModel, error) {
	return &languageModel{
		modelID:  modelID,
		provider: p.options.name,
		client: &client{
			url:          p.options.url,
			token:        p.options.token,
			accountID:    p.options.accountID,
			userAgent:    p.options.userAgent,
			originator:   cmp.Or(p.options.originator, "codex_cli_rs"),
			version:      p.options.version,
			headers:      p.options.headers,
			sessionStore: p.options.sessionStore,
		},
	}, nil
}

type languageModel struct {
	modelID  string
	provider string
	client   *client
}

type CompactionImplementation string

const CompactionRemoteV2 CompactionImplementation = "remote_v2"

type CompactionResult struct {
	History           *CompactedHistory
	Summary           string
	Usage             fantasy.Usage
	UsageAvailable    bool
	Implementation    CompactionImplementation
	ActiveInputTokens int64
	finalizeOnce      sync.Once
	finalize          func()
}

func (r *CompactionResult) Finalize() {
	if r == nil {
		return
	}
	r.finalizeOnce.Do(func() {
		if r.finalize != nil {
			r.finalize()
		}
	})
}

// Model implements fantasy.LanguageModel.
func (g *languageModel) Model() string { return g.modelID }

// Provider implements fantasy.LanguageModel.
func (g *languageModel) Provider() string { return g.provider }

// prepareRequest builds the response.create frame from a fantasy call.
func (g *languageModel) prepareRequest(call fantasy.Call) (*requestFrame, []fantasy.CallWarning, error) {
	providerOptions := &ProviderOptions{}
	if v, ok := call.ProviderOptions[Name]; ok {
		providerOptions, ok = v.(*ProviderOptions)
		if !ok {
			return nil, nil, &fantasy.Error{
				Title:   "invalid argument",
				Message: "codex provider options should be *responses.ProviderOptions",
			}
		}
	}

	instructions, input, warnings := toInput(g.modelID, call.Prompt)
	instructions, dynamicContext := splitDynamicEnvironment(instructions)
	tools, toolWarnings := toWireTools(call.Tools)
	warnings = append(warnings, toolWarnings...)

	frame := &requestFrame{
		Type:              "response.create",
		Model:             g.modelID,
		Instructions:      instructions,
		Input:             input,
		DynamicContext:    dynamicContext,
		ParallelToolCalls: true,
		Stream:            true,
		Text:              &wireTextFormat{},
		RequestKind:       "turn",
	}
	if !providerOptions.DisableReasoning {
		effort := cmp.Or(providerOptions.ReasoningEffort, "medium")
		frame.Reasoning = &wireReasoning{Effort: effort, Summary: "auto"}
		frame.Include = []string{"reasoning.encrypted_content"}
	}
	frame.Text.Format.Type = "text"
	frame.Text.Verbosity = providerOptions.ResponseVerbosity
	if len(tools) > 0 {
		frame.Tools = tools
		frame.ToolChoice = "auto"
	}

	// Unsupported sampling knobs surface as warnings rather than being sent.
	if call.Temperature != nil {
		warnings = append(warnings, fantasy.CallWarning{
			Type:    fantasy.CallWarningTypeUnsupportedSetting,
			Setting: "temperature",
		})
	}
	if call.TopP != nil {
		warnings = append(warnings, fantasy.CallWarning{
			Type:    fantasy.CallWarningTypeUnsupportedSetting,
			Setting: "top_p",
		})
	}
	if call.MaxOutputTokens != nil {
		warnings = append(warnings, fantasy.CallWarning{
			Type:    fantasy.CallWarningTypeUnsupportedSetting,
			Setting: "max_output_tokens",
		})
	}

	return frame, warnings, nil
}

// Generate implements fantasy.LanguageModel.
func (g *languageModel) Generate(ctx context.Context, call fantasy.Call) (*fantasy.Response, error) {
	stream, err := g.Stream(ctx, call)
	if err != nil {
		return nil, err
	}

	var content []fantasy.Content
	var usage fantasy.Usage
	finishReason := fantasy.FinishReasonStop
	var warnings []fantasy.CallWarning
	var streamErr error

	var textBuf string
	var reasoningBuf string
	var reasoningMeta *ReasoningMetadata

	flushText := func() {
		if textBuf != "" {
			content = append(content, fantasy.TextContent{Text: textBuf})
			textBuf = ""
		}
	}
	flushReasoning := func() {
		if reasoningBuf == "" && reasoningMeta == nil {
			return
		}
		rc := fantasy.ReasoningContent{Text: reasoningBuf}
		if reasoningMeta != nil {
			rc.ProviderMetadata = fantasy.ProviderMetadata{Name: reasoningMeta}
		}
		content = append(content, rc)
		reasoningBuf = ""
		reasoningMeta = nil
	}

	for part := range stream {
		switch part.Type {
		case fantasy.StreamPartTypeWarnings:
			warnings = append(warnings, part.Warnings...)
		case fantasy.StreamPartTypeTextDelta:
			textBuf += part.Delta
		case fantasy.StreamPartTypeTextEnd:
			flushText()
		case fantasy.StreamPartTypeReasoningDelta:
			reasoningBuf += part.Delta
		case fantasy.StreamPartTypeReasoningEnd:
			if part.ProviderMetadata != nil {
				if meta, ok := part.ProviderMetadata[Name].(*ReasoningMetadata); ok {
					reasoningMeta = meta
				}
			}
			flushReasoning()
		case fantasy.StreamPartTypeToolCall:
			flushText()
			tc := fantasy.ToolCallContent{
				ToolCallID:       part.ID,
				ToolName:         part.ToolCallName,
				Input:            part.ToolCallInput,
				ProviderExecuted: part.ProviderExecuted,
			}
			if part.ProviderMetadata != nil {
				tc.ProviderMetadata = part.ProviderMetadata
			}
			content = append(content, tc)
		case fantasy.StreamPartTypeFinish:
			usage = part.Usage
			finishReason = part.FinishReason
		case fantasy.StreamPartTypeError:
			streamErr = part.Error
		}
	}
	if streamErr != nil {
		return nil, streamErr
	}
	flushText()
	flushReasoning()

	return &fantasy.Response{
		Content:      content,
		Usage:        usage,
		FinishReason: finishReason,
		Warnings:     warnings,
	}, nil
}

// Stream implements fantasy.LanguageModel.
func (g *languageModel) Stream(ctx context.Context, call fantasy.Call) (fantasy.StreamResponse, error) {
	frame, warnings, err := g.prepareRequest(call)
	if err != nil {
		return nil, err
	}
	conversationID := call.Headers["x-session-id"]
	purpose := call.Headers["x-request-purpose"]
	if purpose == "" {
		purpose = "conversation"
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

		events := g.client.stream(ctx, frame, g.provider, conversationID, purpose)

		var blockCounter int
		var textActive bool
		var textID string
		var reasoningActive bool
		var reasoningID string
		// Codex emits a reasoning item containing one or more summary parts.
		// Crux stores them in one field, so preserve every visible part boundary.
		var hasReasoningSummary bool
		var reasoningSummaryPartOpen bool
		var pendingReasoningSummaryBoundary bool
		var hasToolCalls bool
		var finished bool

		// function-call accumulation per item id
		type fnState struct {
			name   string
			args   string
			callID string
		}
		fnCalls := map[string]*fnState{}

		endText := func() bool {
			textActive = false
			return yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextEnd, ID: textID})
		}
		endReasoning := func(meta *ReasoningMetadata) bool {
			reasoningActive = false
			reasoningSummaryPartOpen = false
			part := fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningEnd, ID: reasoningID}
			if meta != nil {
				part.ProviderMetadata = fantasy.ProviderMetadata{Name: meta}
			}
			return yield(part)
		}

		emitToolCall := func(item *outputItem) bool {
			state := fnCalls[item.ID]
			if state == nil {
				state = &fnState{callID: cmp.Or(item.CallID, item.ID)}
			}
			if item.Name != "" {
				state.name = item.Name
			}
			if item.Arguments != "" {
				state.args = item.Arguments
			}
			if item.CallID != "" {
				state.callID = item.CallID
			}
			args := state.args
			if args == "" {
				args = "{}"
			}
			hasToolCalls = true
			meta := fantasy.ProviderMetadata{Name: &ToolCallMetadata{ItemID: item.ID}}
			if !yield(fantasy.StreamPart{
				Type:         fantasy.StreamPartTypeToolInputStart,
				ID:           state.callID,
				ToolCallName: state.name,
			}) {
				return false
			}
			if !yield(fantasy.StreamPart{
				Type:  fantasy.StreamPartTypeToolInputDelta,
				ID:    state.callID,
				Delta: args,
			}) {
				return false
			}
			if !yield(fantasy.StreamPart{
				Type: fantasy.StreamPartTypeToolInputEnd,
				ID:   state.callID,
			}) {
				return false
			}
			return yield(fantasy.StreamPart{
				Type:             fantasy.StreamPartTypeToolCall,
				ID:               state.callID,
				ToolCallName:     state.name,
				ToolCallInput:    args,
				ProviderExecuted: false,
				ProviderMetadata: meta,
			})
		}

		for event, err := range events {
			if err != nil {
				yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: err})
				return
			}
			switch event.Type {
			case "response.output_text.delta":
				if event.Delta == "" {
					continue
				}
				if reasoningActive && !endReasoning(nil) {
					return
				}
				if !textActive {
					textActive = true
					textID = fmt.Sprintf("%d", blockCounter)
					blockCounter++
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeTextStart, ID: textID}) {
						return
					}
				}
				if !yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeTextDelta,
					ID:    textID,
					Delta: event.Delta,
				}) {
					return
				}
			case "response.reasoning_summary_part.added":
				// A reasoning item can contain multiple terse summary parts. The
				// text deltas themselves do not carry a separator, so defer adding
				// one until this part emits visible text.
				if hasReasoningSummary {
					pendingReasoningSummaryBoundary = true
				}
				reasoningSummaryPartOpen = true
			case "response.reasoning_summary_part.done":
				reasoningSummaryPartOpen = false
			case "response.reasoning_summary_text.delta":
				if event.Delta == "" {
					continue
				}
				if textActive && !endText() {
					return
				}
				wasReasoningActive := reasoningActive
				if !reasoningActive {
					reasoningActive = true
					reasoningID = fmt.Sprintf("%d", blockCounter)
					blockCounter++
					if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: reasoningID}) {
						return
					}
				}
				// Most streams use summary-part events, but retain a fallback for
				// streams that expose only separate reasoning items. In both cases,
				// the raw deltas omit the whitespace between visible updates.
				if (pendingReasoningSummaryBoundary || (hasReasoningSummary && !wasReasoningActive && !reasoningSummaryPartOpen)) && !yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeReasoningDelta,
					ID:    reasoningID,
					Delta: "\n\n",
				}) {
					return
				}
				pendingReasoningSummaryBoundary = false
				if !yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeReasoningDelta,
					ID:    reasoningID,
					Delta: event.Delta,
				}) {
					return
				}
				hasReasoningSummary = true
			case "response.output_item.added":
				var item outputItem
				if err := json.Unmarshal(event.Item, &item); err != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("codex: decode added output item: %w", err)})
					return
				}
				if item.Type == "function_call" && item.ID != "" {
					fnCalls[item.ID] = &fnState{
						name:   item.Name,
						args:   item.Arguments,
						callID: cmp.Or(item.CallID, item.ID),
					}
				}
			case "response.function_call_arguments.delta":
				if state := fnCalls[event.ItemID]; state != nil {
					state.args += event.Delta
				}
			case "response.function_call_arguments.done":
				if state := fnCalls[event.ItemID]; state != nil && event.Arguments != "" {
					state.args = event.Arguments
				}
			case "response.output_item.done":
				var item outputItem
				if err := json.Unmarshal(event.Item, &item); err != nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("codex: decode completed output item: %w", err)})
					return
				}
				switch item.Type {
				case "function_call":
					if textActive && !endText() {
						return
					}
					if reasoningActive && !endReasoning(nil) {
						return
					}
					if !emitToolCall(&item) {
						return
					}
				case "reasoning":
					if item.EncryptedContent != "" {
						meta := &ReasoningMetadata{
							ItemID:           item.ID,
							EncryptedContent: item.EncryptedContent,
							Summary:          item.Summary,
						}
						if !reasoningActive {
							reasoningActive = true
							reasoningID = fmt.Sprintf("%d", blockCounter)
							blockCounter++
							if !yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeReasoningStart, ID: reasoningID}) {
								return
							}
						}
						if !endReasoning(meta) {
							return
						}
					}
				}
			case "response.failed", "response.incomplete":
				var responseError *wireError
				if event.Response != nil {
					responseError = event.Response.Error
				}
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: codexProviderError(responseError, "codex response did not complete"),
				})
				return
			case "response.completed":
				if event.Response == nil {
					yield(fantasy.StreamPart{Type: fantasy.StreamPartTypeError, Error: fmt.Errorf("codex: completed response missing payload")})
					return
				}
				if event.Response.Error != nil {
					yield(fantasy.StreamPart{
						Type:  fantasy.StreamPartTypeError,
						Error: codexProviderError(event.Response.Error, "codex response failed"),
					})
					return
				}
				if textActive && !endText() {
					return
				}
				if reasoningActive && !endReasoning(nil) {
					return
				}

				usage := usageFromWire(event.Response.Usage)
				if event.Response.Usage != nil {
					u := event.Response.Usage
					slog.Debug("Codex usage received",
						"provider_input_tokens", u.InputTokens,
						"cached_input_tokens", usage.CacheReadTokens,
						"noncached_input_tokens", usage.InputTokens,
						"output_tokens", u.OutputTokens,
						"reasoning_tokens", usage.ReasoningTokens,
					)
				}
				finishReason := fantasy.FinishReasonStop
				if hasToolCalls {
					finishReason = fantasy.FinishReasonToolCalls
				}
				finished = true
				yield(fantasy.StreamPart{
					Type:         fantasy.StreamPartTypeFinish,
					Usage:        usage,
					FinishReason: finishReason,
				})
				return
			case "error":
				yield(fantasy.StreamPart{
					Type:  fantasy.StreamPartTypeError,
					Error: codexProviderError(event.Error, "unknown codex error"),
				})
				return
			}
		}

		if !finished {
			yield(fantasy.StreamPart{
				Type:  fantasy.StreamPartTypeError,
				Error: fantasy.NewIncompleteStreamError(),
			})
		}
	}, nil
}

func codexProviderError(responseError *wireError, fallbackMessage string) *fantasy.ProviderError {
	message := fallbackMessage
	code := ""
	if responseError != nil {
		if responseError.Message != "" {
			message = responseError.Message
		}
		code = responseError.Code
	}
	return &fantasy.ProviderError{
		Title:              "codex error",
		Message:            message,
		ContextTooLargeErr: code == "context_length_exceeded",
		TransientError:     fantasy.TransientStreamErrorTypes[code],
	}
}

// GenerateObject implements fantasy.LanguageModel.
func (g *languageModel) GenerateObject(ctx context.Context, call fantasy.ObjectCall) (*fantasy.ObjectResponse, error) {
	return object.GenerateWithText(ctx, g, call)
}

// StreamObject implements fantasy.LanguageModel.
func (g *languageModel) StreamObject(ctx context.Context, call fantasy.ObjectCall) (fantasy.ObjectStreamResponse, error) {
	return object.StreamWithText(ctx, g, call)
}
