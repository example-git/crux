package antigravity

// Wire types for the Antigravity Cloud Code dialect of the Gemini
// generateContent protocol. The endpoint differs from the public Gemini API
// in ways the stock SDK cannot express, so requests and responses are
// (de)serialized natively:
//
//   - every request is wrapped in an envelope carrying project, requestId,
//     model, userAgent, and requestType;
//   - tool results are sent as their own model-role turns, not user turns;
//   - thoughtSignature must be echoed back on thinking, functionCall, and
//     functionResponse parts or the endpoint rejects the conversation;
//   - each streamed chunk arrives wrapped in a {"response": ...} object.

import (
	"encoding/json"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

// wireEnvelope is the top-level Antigravity request body.
type wireEnvelope struct {
	Project     string       `json:"project"`
	RequestID   string       `json:"requestId"`
	Request     *wireRequest `json:"request"`
	Model       string       `json:"model"`
	UserAgent   string       `json:"userAgent"`
	RequestType string       `json:"requestType"`
}

// wireRequest is the inner generateContent request.
type wireRequest struct {
	Contents          []wireContent         `json:"contents"`
	SystemInstruction *wireContent          `json:"systemInstruction,omitempty"`
	GenerationConfig  *wireGenerationConfig `json:"generationConfig,omitempty"`
	Tools             []wireTool            `json:"tools,omitempty"`
	ToolConfig        *wireToolConfig       `json:"toolConfig,omitempty"`
	SessionID         string                `json:"sessionId,omitempty"`
	SafetySettings    []SafetySetting       `json:"safetySettings,omitempty"`
	CachedContent     string                `json:"cachedContent,omitempty"`
}

type wireContent struct {
	Role  string     `json:"role,omitempty"`
	Parts []wirePart `json:"parts"`
}

type wirePart struct {
	Text             string                `json:"text,omitempty"`
	Thought          bool                  `json:"thought,omitempty"`
	ThoughtSignature string                `json:"thoughtSignature,omitempty"`
	FunctionCall     *wireFunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *wireFunctionResponse `json:"functionResponse,omitempty"`
	InlineData       *wireBlob             `json:"inlineData,omitempty"`
}

type wireFunctionCall struct {
	ID   string         `json:"id,omitempty"`
	Name string         `json:"name"`
	Args map[string]any `json:"args"`
}

type wireFunctionResponse struct {
	ID       string         `json:"id,omitempty"`
	Name     string         `json:"name"`
	Response map[string]any `json:"response"`
}

type wireBlob struct {
	MIMEType string `json:"mimeType,omitempty"`
	Data     []byte `json:"data,omitempty"`
}

type wireGenerationConfig struct {
	MaxOutputTokens    int64               `json:"maxOutputTokens,omitempty"`
	Temperature        *float64            `json:"temperature,omitempty"`
	TopP               *float64            `json:"topP,omitempty"`
	TopK               *int64              `json:"topK,omitempty"`
	PresencePenalty    *float64            `json:"presencePenalty,omitempty"`
	FrequencyPenalty   *float64            `json:"frequencyPenalty,omitempty"`
	ThinkingConfig     *wireThinkingConfig `json:"thinkingConfig,omitempty"`
	ResponseMIMEType   string              `json:"responseMimeType,omitempty"`
	ResponseJSONSchema any                 `json:"responseJsonSchema,omitempty"`
}

type wireThinkingConfig struct {
	IncludeThoughts bool   `json:"includeThoughts,omitempty"`
	ThinkingBudget  *int64 `json:"thinkingBudget,omitempty"`
	ThinkingLevel   string `json:"thinkingLevel,omitempty"`
}

type wireTool struct {
	FunctionDeclarations []wireFunctionDeclaration `json:"functionDeclarations"`
}

type wireFunctionDeclaration struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type wireToolConfig struct {
	FunctionCallingConfig *wireFunctionCallingConfig `json:"functionCallingConfig,omitempty"`
}

type wireFunctionCallingConfig struct {
	Mode string `json:"mode"`
}

// Response wire types. Streamed chunks arrive as {"response": <this>} and are
// unwrapped by the SSE scanner before decoding.
type wireResponse struct {
	Candidates    []wireCandidate `json:"candidates"`
	UsageMetadata *wireUsage      `json:"usageMetadata"`
	Error         *wireError      `json:"error"`
}

type wireCandidate struct {
	Content      *wireContent `json:"content"`
	FinishReason string       `json:"finishReason"`
}

type wireUsage struct {
	PromptTokenCount        int64 `json:"promptTokenCount"`
	CandidatesTokenCount    int64 `json:"candidatesTokenCount"`
	TotalTokenCount         int64 `json:"totalTokenCount"`
	ThoughtsTokenCount      int64 `json:"thoughtsTokenCount"`
	CachedContentTokenCount int64 `json:"cachedContentTokenCount"`
}

type wireError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Status  string          `json:"status"`
	Details json.RawMessage `json:"details"`
}

// toWirePrompt converts a fantasy prompt into Antigravity wire contents,
// matching the shape the endpoint accepts:
//
//   - assistant thinking parts with signatures are echoed back as
//     {thought: true, thoughtSignature} (text omitted);
//   - functionCall parts carry the signature bound to that tool call, or the
//     last signature seen in the turn;
//   - each tool result becomes its own model-role functionResponse turn,
//     also carrying the signature of the originating call.
func toWirePrompt(prompt fantasy.Prompt) (system *wireContent, contents []wireContent, warnings []fantasy.CallWarning) {
	// Signatures bound to specific tool calls, and the last signature seen,
	// carried across turns so tool-result turns can echo them.
	toolSig := map[string]string{}
	toolName := map[string]string{}
	lastSig := ""

	finishedSystemBlock := false
	for _, msg := range prompt {
		switch msg.Role {
		case fantasy.MessageRoleSystem:
			if finishedSystemBlock {
				continue
			}
			finishedSystemBlock = true
			var systemMessages []string
			for _, part := range msg.Content {
				text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
				if !ok || text.Text == "" {
					continue
				}
				systemMessages = append(systemMessages, text.Text)
			}
			if len(systemMessages) > 0 {
				system = &wireContent{
					Parts: []wirePart{{Text: strings.Join(systemMessages, "\n")}},
				}
			}
		case fantasy.MessageRoleUser:
			var parts []wirePart
			for _, part := range msg.Content {
				switch part.GetType() {
				case fantasy.ContentTypeText:
					text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
					if !ok || text.Text == "" {
						continue
					}
					parts = append(parts, wirePart{Text: text.Text})
				case fantasy.ContentTypeFile:
					file, ok := fantasy.AsMessagePart[fantasy.FilePart](part)
					if !ok {
						continue
					}
					parts = append(parts, wirePart{
						InlineData: &wireBlob{Data: file.Data, MIMEType: file.MediaType},
					})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, wireContent{Role: "user", Parts: parts})
			}
		case fantasy.MessageRoleAssistant:
			var parts []wirePart
			var pendingSig string
			for _, part := range msg.Content {
				switch part.GetType() {
				case fantasy.ContentTypeReasoning:
					reasoning, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part)
					if !ok {
						continue
					}
					var meta *ReasoningMetadata
					if v, ok := reasoning.ProviderOptions[Name]; ok {
						meta, _ = v.(*ReasoningMetadata)
					}
					if meta != nil && meta.Signature != "" {
						lastSig = meta.Signature
						pendingSig = meta.Signature
						if meta.ToolID != "" {
							toolSig[meta.ToolID] = meta.Signature
						}
						// Signed thinking is echoed back signature-only; the
						// endpoint validates the signature, not the text.
						parts = append(parts, wirePart{
							Thought:          true,
							ThoughtSignature: meta.Signature,
						})
					}
				case fantasy.ContentTypeText:
					text, ok := fantasy.AsMessagePart[fantasy.TextPart](part)
					if !ok || text.Text == "" {
						continue
					}
					parts = append(parts, wirePart{Text: text.Text})
				case fantasy.ContentTypeToolCall:
					toolCall, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
					if !ok {
						continue
					}
					// Args must always be a non-nil object: the endpoint
					// rejects tool calls without an input field, and a call
					// that fails to parse is still sent (dropping it would
					// orphan the following tool result).
					args := map[string]any{}
					if input := strings.TrimSpace(toolCall.Input); input != "" && input != "null" {
						if err := json.Unmarshal([]byte(input), &args); err != nil || args == nil {
							args = map[string]any{}
						}
					}
					sig := toolSig[toolCall.ToolCallID]
					if sig == "" {
						sig = pendingSig
					}
					if sig == "" {
						sig = lastSig
					}
					if sig != "" {
						toolSig[toolCall.ToolCallID] = sig
						lastSig = sig
					}
					pendingSig = ""
					toolName[toolCall.ToolCallID] = toolCall.ToolName
					parts = append(parts, wirePart{
						ThoughtSignature: sig,
						FunctionCall: &wireFunctionCall{
							ID:   toolCall.ToolCallID,
							Name: toolCall.ToolName,
							Args: args,
						},
					})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, wireContent{Role: "model", Parts: parts})
			}
		case fantasy.MessageRoleTool:
			// Antigravity quirk: functionResponse goes in its own model-role
			// turn, one turn per response, not batched into a user turn.
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
					content, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Output)
					if !ok {
						continue
					}
					output = content.Text
				case fantasy.ToolResultContentTypeError:
					content, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Output)
					if !ok {
						continue
					}
					output = content.Error.Error()
				default:
					continue
				}
				sig := toolSig[result.ToolCallID]
				if sig == "" {
					sig = lastSig
				}
				contents = append(contents, wireContent{
					Role: "model",
					Parts: []wirePart{{
						ThoughtSignature: sig,
						FunctionResponse: &wireFunctionResponse{
							ID:       result.ToolCallID,
							Name:     toolName[result.ToolCallID],
							Response: map[string]any{"output": output},
						},
					}},
				})
			}
		default:
			warnings = append(warnings, fantasy.CallWarning{
				Type:    fantasy.CallWarningTypeOther,
				Message: "unsupported message role: " + string(msg.Role),
			})
		}
	}
	return system, contents, warnings
}

// geminiSchemaStripKeys are JSON Schema keywords the endpoint rejects.
var geminiSchemaStripKeys = map[string]struct{}{
	"$schema":               {},
	"additionalProperties":  {},
	"propertyNames":         {},
	"exclusiveMinimum":      {},
	"exclusiveMaximum":      {},
	"const":                 {},
	"unevaluatedProperties": {},
	"patternProperties":     {},
}

// sanitizeGeminiSchema strips JSON Schema keywords the endpoint rejects,
// recursively, leaving the rest of the schema untouched.
func sanitizeGeminiSchema(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = sanitizeGeminiSchema(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, raw := range v {
			if _, strip := geminiSchemaStripKeys[key]; strip {
				continue
			}
			out[key] = sanitizeGeminiSchema(raw)
		}
		return out
	default:
		return value
	}
}

// toWireTools converts fantasy tools into wire function declarations with
// sanitized schemas.
func toWireTools(tools []fantasy.Tool, toolChoice *fantasy.ToolChoice) ([]wireTool, *wireToolConfig, []fantasy.CallWarning) {
	var declarations []wireFunctionDeclaration
	var warnings []fantasy.CallWarning
	for _, tool := range tools {
		ft, ok := tool.(fantasy.FunctionTool)
		if tool.GetType() != fantasy.ToolTypeFunction || !ok {
			warnings = append(warnings, fantasy.CallWarning{
				Type:    fantasy.CallWarningTypeUnsupportedTool,
				Tool:    tool,
				Message: "tool is not supported",
			})
			continue
		}
		declarations = append(declarations, wireFunctionDeclaration{
			Name:        ft.Name,
			Description: ft.Description,
			Parameters:  sanitizeGeminiSchema(map[string]any(ft.InputSchema)),
		})
	}

	var wireTools []wireTool
	if len(declarations) > 0 {
		wireTools = []wireTool{{FunctionDeclarations: declarations}}
	}

	if toolChoice == nil {
		return wireTools, nil, warnings
	}
	mode := "AUTO"
	switch *toolChoice {
	case fantasy.ToolChoiceRequired:
		mode = "ANY"
	case fantasy.ToolChoiceNone:
		mode = "NONE"
	}
	return wireTools, &wireToolConfig{
		FunctionCallingConfig: &wireFunctionCallingConfig{Mode: mode},
	}, warnings
}
