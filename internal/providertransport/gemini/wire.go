// Package gemini implements provider-neutral transport primitives for the
// documented Gemini GenerateContent and Interactions APIs. Consumer OAuth,
// private envelopes, identity headers, quotas, and model-specific defaults live
// outside this package.
package gemini

import "encoding/json"

// Content is the public GenerateContent turn shape.
type Content struct {
	Role  string `json:"role,omitempty"`
	Parts []Part `json:"parts"`
}

// Part is the documented public multimodal/function/thought union. Unknown
// future fields remain outside this typed helper and may be sent with RawClient.
type Part struct {
	Text             string            `json:"text,omitempty"`
	Thought          bool              `json:"thought,omitempty"`
	ThoughtSignature string            `json:"thoughtSignature,omitempty"`
	InlineData       *Blob             `json:"inlineData,omitempty"`
	FunctionCall     *FunctionCall     `json:"functionCall,omitempty"`
	FunctionResponse *FunctionResponse `json:"functionResponse,omitempty"`
}

type Blob struct {
	MIMEType string `json:"mimeType"`
	Data     string `json:"data"`
}

type FunctionCall struct {
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"args"`
}

type FunctionResponse struct {
	ID       string          `json:"id,omitempty"`
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type FunctionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type Tool struct {
	FunctionDeclarations []FunctionDeclaration `json:"functionDeclarations,omitempty"`
}

type GenerateContentRequest struct {
	Contents          []Content       `json:"contents"`
	SystemInstruction *Content        `json:"systemInstruction,omitempty"`
	Tools             []Tool          `json:"tools,omitempty"`
	ToolConfig        json.RawMessage `json:"toolConfig,omitempty"`
	GenerationConfig  json.RawMessage `json:"generationConfig,omitempty"`
	SafetySettings    json.RawMessage `json:"safetySettings,omitempty"`
	CachedContent     string          `json:"cachedContent,omitempty"`
}
