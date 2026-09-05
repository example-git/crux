package responses

// Wire types for the Codex Responses API over WebSocket, matching the frames
// the Codex CLI exchanges with wss://chatgpt.com/backend-api/codex/responses:
//
//   - out: {type:"response.create", model, instructions, input[], tools,
//     reasoning, stream:true, include:["reasoning.encrypted_content"], ...}
//   - in:  response.output_text.delta / response.output_item.added|done /
//     response.function_call_arguments.delta|done /
//     response.reasoning_summary_text.delta / response.completed / error
//
// Logical history remains fully reconstructable. A compatible session may send
// only an append-only input delta with previous_response_id; otherwise the
// transport safely replays prior reasoning items and function calls in full.

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
)

// inputItem is one item in the request input array. Exactly one shape is
// populated per item.
const (
	defaultToolOutputLimit     = 10_000
	toolOutputBytesPerToken    = 4
	toolOutputHistoryAllowance = 6
	toolOutputAllowanceBase    = 5
	toolOutputTruncationTag    = "\n\n[... tool output truncated ...]\n\n"
	maxDynamicEnvironmentBytes = 10_000
	dynamicEnvironmentTag      = "\n[... dynamic environment truncated ...]\n"
)

type truncationMode uint8

const (
	truncateBytes truncationMode = iota
	truncateTokens
)

type truncationPolicy struct {
	mode  truncationMode
	limit int
}

type inputItem struct {
	Type string `json:"type"`

	// message
	Role    string           `json:"role,omitempty"`
	Content []messageContent `json:"content,omitempty"`

	// function_call
	ID        string `json:"id,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call_output
	Output *string `json:"output,omitempty"`

	// reasoning (re-emitted verbatim)
	EncryptedContent string          `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage `json:"summary,omitempty"`
}

type messageContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// wireTool is the flat tool shape Codex expects.
type wireTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Strict      bool           `json:"strict"`
	Parameters  map[string]any `json:"parameters"`
}

// requestFrame is the outgoing response.create frame.
type requestFrame struct {
	Type               string            `json:"type"`
	Model              string            `json:"model"`
	Instructions       string            `json:"instructions,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Input              []inputItem       `json:"input"`
	Tools              []wireTool        `json:"tools,omitempty"`
	ToolChoice         string            `json:"tool_choice,omitempty"`
	ParallelToolCalls  bool              `json:"parallel_tool_calls"`
	Reasoning          *wireReasoning    `json:"reasoning,omitempty"`
	Stream             bool              `json:"stream"`
	Include            []string          `json:"include,omitempty"`
	Text               *wireTextFormat   `json:"text,omitempty"`
	PromptCacheKey     string            `json:"prompt_cache_key,omitempty"`
	Store              bool              `json:"store"`
	ClientMetadata     map[string]string `json:"client_metadata,omitempty"`
	DynamicContext     string            `json:"-"`
	RequestKind        string            `json:"-"`
}

type wireReasoning struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary"`
}

type wireTextFormat struct {
	Format struct {
		Type string `json:"type"`
	} `json:"format"`
	Verbosity string `json:"verbosity,omitempty"`
}

// eventFrame is one incoming WebSocket frame.
type eventFrame struct {
	Type        string          `json:"type"`
	Delta       string          `json:"delta,omitempty"`
	ItemID      string          `json:"item_id,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
	Item        json.RawMessage `json:"item,omitempty"`
	Response    *wireResponse   `json:"response,omitempty"`
	Error       *wireError      `json:"error,omitempty"`
	Code        string          `json:"code,omitempty"`
	Message     string          `json:"message,omitempty"`
	mappedError error           `json:"-"`
}

// outputItem is one item in a completed response's output array (also the
// shape of response.output_item frames).
type outputItem struct {
	Type             string           `json:"type"`
	ID               string           `json:"id,omitempty"`
	CallID           string           `json:"call_id,omitempty"`
	Name             string           `json:"name,omitempty"`
	Arguments        string           `json:"arguments,omitempty"`
	Role             string           `json:"role,omitempty"`
	Content          []messageContent `json:"content,omitempty"`
	EncryptedContent string           `json:"encrypted_content,omitempty"`
	Summary          json.RawMessage  `json:"summary,omitempty"`
}

type wireResponse struct {
	ID     string            `json:"id,omitempty"`
	Output []json.RawMessage `json:"output,omitempty"`
	Usage  *wireUsage        `json:"usage,omitempty"`
	Error  *wireError        `json:"error,omitempty"`
}

type wireUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens,omitempty"`
	InputTokensDetails *struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens,omitempty"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

type wireError struct {
	Message string `json:"message"`
	Code    string `json:"code,omitempty"`
}

// ReasoningMetadata is attached to reasoning parts so encrypted reasoning
// items can be replayed verbatim on the next request.
type ReasoningMetadata struct {
	// ItemID is the rs_* id the API issued for the reasoning item.
	ItemID string `json:"item_id,omitempty"`
	// EncryptedContent is the opaque reasoning payload.
	EncryptedContent string `json:"encrypted_content,omitempty"`
	// Summary is the raw summary array, replayed verbatim.
	Summary json.RawMessage `json:"summary,omitempty"`
}

// ToolCallMetadata is attached to tool-call parts so the fc_* item id the
// API issued can be echoed back; the API validates it.
type ToolCallMetadata struct {
	ItemID string `json:"item_id,omitempty"`
}

// CompactedHistory is the opaque replacement transcript returned by Codex
// compaction. It is stored on the visible summary message and replayed verbatim
// on the next request.
type CompactedHistory struct {
	Items []inputItem `json:"items"`
}

// toInput converts a fantasy prompt into Responses input items plus the
// instructions string (from system messages).
func toInput(modelID string, prompt fantasy.Prompt) (instructions string, items []inputItem, warnings []fantasy.CallWarning) {
	instructions, _, items, warnings = toInputWithDynamic(modelID, prompt)
	return instructions, items, warnings
}

func toInputWithDynamic(modelID string, prompt fantasy.Prompt) (instructions, dynamicContext string, items []inputItem, warnings []fantasy.CallWarning) {
	var systemParts []string
	var dynamicParts []string
	typedSystem := false
	functionCallID := newFunctionCallIDConverter()
	for _, msg := range prompt {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			for _, part := range msg.Content {
				text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
				if !ok || text.Text == "" {
					continue
				}
				options := fantasy.InstructionPartOptionsFrom(part.Options())
				if options != nil {
					typedSystem = true
					if options.Stability == fantasy.InstructionStabilityDynamic && slices.Contains(options.Kinds, fantasy.InstructionKindRuntime) {
						dynamicParts = append(dynamicParts, text.Text)
						continue
					}
				}
				systemParts = append(systemParts, text.Text)
			}
		case fantasy.MessageRoleUser:
			var content []messageContent
			for _, part := range msg.Content {
				switch part.GetType() {
				case fantasy.ContentTypeText:
					text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
					if !ok {
						continue
					}
					if value, ok := text.ProviderOptions[Name]; ok {
						if compacted, ok := value.(*CompactedHistory); ok && len(compacted.Items) > 0 {
							items = append(items, compacted.Items...)
							continue
						}
					}
					if text.Text != "" {
						content = append(content, messageContent{Type: "input_text", Text: text.Text})
					}
				case fantasy.ContentTypeFile:
					file, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
					if !ok {
						continue
					}
					if !strings.HasPrefix(file.MediaType, "image/") {
						warnings = append(warnings, fantasy.CallWarning{
							Type:    fantasy.CallWarningTypeOther,
							Message: "unsupported Codex file media type: " + file.MediaType,
						})
						continue
					}
					if len(file.Data) == 0 {
						warnings = append(warnings, fantasy.CallWarning{
							Type:    fantasy.CallWarningTypeOther,
							Message: "Codex image data is empty",
						})
						continue
					}
					content = append(content, messageContent{
						Type:     "input_image",
						ImageURL: "data:" + file.MediaType + ";base64," + base64.StdEncoding.EncodeToString(file.Data),
						Detail:   "high",
					})
				}
			}
			if len(content) > 0 {
				items = append(items, inputItem{Type: "message", Role: "user", Content: content})
			}
		case fantasy.MessageRoleAssistant:
			// The Responses API requires prior output items to be replayed in
			// the order they were received: reasoning items directly before
			// the function_call items they produced.
			for _, part := range msg.Content {
				switch part.GetType() {
				case fantasy.ContentTypeText:
					if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && text.Text != "" {
						items = append(items, inputItem{
							Type: "message",
							Role: "assistant",
							Content: []messageContent{{
								Type: "output_text",
								Text: text.Text,
							}},
						})
					}
				case fantasy.ContentTypeReasoning:
					reasoning, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part)
					if !ok {
						continue
					}
					var meta *ReasoningMetadata
					if v, ok := reasoning.ProviderOptions[Name]; ok {
						meta, _ = v.(*ReasoningMetadata)
					}
					if meta != nil && meta.EncryptedContent != "" {
						summary := meta.Summary
						if len(summary) == 0 {
							summary = json.RawMessage(`[]`)
						}
						items = append(items, inputItem{
							Type:             "reasoning",
							ID:               meta.ItemID,
							EncryptedContent: meta.EncryptedContent,
							Summary:          summary,
						})
					}
				case fantasy.ContentTypeToolCall:
					toolCall, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
					if !ok {
						continue
					}
					itemID := toolCall.ToolCallID
					if v, ok := toolCall.ProviderOptions[Name]; ok {
						if meta, ok := v.(*ToolCallMetadata); ok && meta.ItemID != "" {
							itemID = meta.ItemID
						}
					}
					args := strings.TrimSpace(toolCall.Input)
					if args == "" || args == "null" {
						args = "{}"
					}
					items = append(items, inputItem{
						Type:      "function_call",
						ID:        functionCallID(itemID),
						CallID:    toolCall.ToolCallID,
						Name:      toolCall.ToolName,
						Arguments: args,
					})
				}
			}
		case fantasy.MessageRoleTool:
			for _, part := range msg.Content {
				if part.GetType() != fantasy.ContentTypeToolResult {
					continue
				}
				result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if !ok {
					continue
				}
				var output string
				switch result.Output.GetType() {
				case fantasy.ToolResultContentTypeText:
					if content, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Output); ok {
						output = content.Text
					}
				case fantasy.ToolResultContentTypeError:
					if content, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Output); ok {
						output = content.Error.Error()
					}
				case fantasy.ToolResultContentTypeMedia:
					// Keep the result paired with its call. Codex accepts structured
					// output, but Crux currently persists media separately; a stable
					// placeholder is safer than deleting both history items.
					output = "[Tool returned media content]"
				default:
					continue
				}
				output = TruncateToolOutput(modelID, output)
				items = append(items, inputItem{
					Type:   "function_call_output",
					CallID: result.ToolCallID,
					Output: &output,
				})
			}
		default:
			warnings = append(warnings, fantasy.CallWarning{
				Type:    fantasy.CallWarningTypeOther,
				Message: "unsupported message role: " + string(msg.Role),
			})
		}
	}

	// Drop unpaired function_call / function_call_output items: the API
	// rejects requests holding a call without its output or vice versa.
	items = dropUnpairedCalls(items)

	separator := "\n"
	if typedSystem {
		separator = "\n\n"
	}
	return strings.Join(systemParts, separator), strings.Join(dynamicParts, "\n\n"), items, warnings
}

// newFunctionCallIDConverter returns a request-local converter that maps any
// tool call identifier to a stable Responses API function-call item ID.
func newFunctionCallIDConverter() func(string) string {
	converted := map[string]string{}
	return func(id string) string {
		if id == "" {
			return ""
		}
		if strings.HasPrefix(id, "fc_") {
			return id
		}
		if fcID, ok := converted[id]; ok {
			return fcID
		}

		suffix := strings.TrimPrefix(id, "call_")
		fcID := "fc_" + suffix
		converted[id] = fcID
		return fcID
	}
}

// dropUnpairedCalls removes function_call items without a matching
// function_call_output and vice versa.
func dropUnpairedCalls(items []inputItem) []inputItem {
	calls := map[string]bool{}
	outputs := map[string]bool{}
	for _, item := range items {
		switch item.Type {
		case "function_call":
			calls[item.CallID] = true
		case "function_call_output":
			outputs[item.CallID] = true
		}
	}
	filtered := items[:0]
	for _, item := range items {
		switch item.Type {
		case "function_call":
			if !outputs[item.CallID] {
				continue
			}
		case "function_call_output":
			if !calls[item.CallID] {
				continue
			}
		}
		filtered = append(filtered, item)
	}
	return filtered
}

// toWireTools converts fantasy tools into the flat Codex tool shape.
func toWireTools(tools []fantasy.Tool) ([]wireTool, []fantasy.CallWarning) {
	var out []wireTool
	var warnings []fantasy.CallWarning
	for _, tool := range tools {
		ft, ok := tool.(fantasy.FunctionTool)
		if !ok {
			warnings = append(warnings, fantasy.CallWarning{
				Type:    fantasy.CallWarningTypeUnsupportedTool,
				Tool:    tool,
				Message: "unsupported tool type",
			})
			continue
		}
		params := ft.InputSchema
		if params == nil {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		out = append(out, wireTool{
			Type:        "function",
			Name:        ft.Name,
			Description: ft.Description,
			Strict:      false,
			Parameters:  params,
		})
	}
	slices.SortFunc(out, func(a, b wireTool) int {
		return strings.Compare(a.Name, b.Name)
	})
	return out, warnings
}

func toolOutputPolicy(modelID string) truncationPolicy {
	switch modelID {
	case "gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return truncationPolicy{mode: truncateTokens, limit: defaultToolOutputLimit}
	default:
		return truncationPolicy{mode: truncateBytes, limit: defaultToolOutputLimit}
	}
}

func toolOutputLimitBytes(modelID string) int {
	policy := toolOutputPolicy(modelID)
	limit := policy.limit * toolOutputHistoryAllowance / toolOutputAllowanceBase
	if policy.mode == truncateTokens {
		limit *= toolOutputBytesPerToken
	}
	return limit
}

func dynamicEnvironmentItem(snapshot string) inputItem {
	const prefix = "<environment_context>\nThis context replaces the previous dynamic environment snapshot.\n"
	const suffix = "\n</environment_context>"
	text := prefix + snapshot + suffix
	if len(text) > maxDynamicEnvironmentBytes {
		available := maxDynamicEnvironmentBytes - len(prefix) - len(dynamicEnvironmentTag) - len(suffix)
		snapshot = utf8Prefix(snapshot, available)
		text = prefix + snapshot + dynamicEnvironmentTag + suffix
	}
	return inputItem{
		Type:    "message",
		Role:    "user",
		Content: []messageContent{{Type: "input_text", Text: text}},
	}
}

func utf8Prefix(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func TruncateToolOutput(modelID, output string) string {
	maxBytes := toolOutputLimitBytes(modelID)
	if len(output) <= maxBytes {
		return output
	}
	available := maxBytes - len(toolOutputTruncationTag)
	headBytes := available / 2
	tailBytes := available - headBytes
	headEnd := headBytes
	for headEnd > 0 && !utf8.RuneStart(output[headEnd]) {
		headEnd--
	}
	tailStart := len(output) - tailBytes
	for tailStart < len(output) && !utf8.RuneStart(output[tailStart]) {
		tailStart++
	}
	return output[:headEnd] + toolOutputTruncationTag + output[tailStart:]
}
