package message

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/stringext"
)

type MessageRole string

const (
	Assistant MessageRole = "assistant"
	User      MessageRole = "user"
	System    MessageRole = "system"
	Tool      MessageRole = "tool"
)

// mediaLoadFailedPlaceholder is the text substituted for image data that
// cannot be decoded during session replay.
const mediaLoadFailedPlaceholder = "[Image data could not be loaded]"

type FinishReason string

const (
	FinishReasonEndTurn   FinishReason = "end_turn"
	FinishReasonMaxTokens FinishReason = "max_tokens"
	FinishReasonToolUse   FinishReason = "tool_use"
	FinishReasonCanceled  FinishReason = "canceled"
	FinishReasonError     FinishReason = "error"
	// FinishReasonContentFilter is a provider safety/refusal stop
	// (Anthropic stop_reason=refusal, OpenAI content_filter, etc.).
	// The TUI renders this as a REFUSED banner rather than a silent
	// empty turn.
	FinishReasonContentFilter FinishReason = "content_filter"

	// Should never happen
	FinishReasonUnknown FinishReason = "unknown"
)

type ContentPart interface {
	isPart()
}

// ProviderMetadataContent stores message- and continuation-scoped opaque
// metadata without exposing a provider-specific core field.
type ProviderMetadataContent struct {
	ProviderMetadata ProviderMetadata `json:"provider_metadata"`
}

func (ProviderMetadataContent) isPart() {}

type ReasoningContent struct {
	Thinking         string           `json:"thinking"`
	ProviderMetadata ProviderMetadata `json:"provider_metadata,omitempty"`
	StartedAt        int64            `json:"started_at,omitempty"`
	FinishedAt       int64            `json:"finished_at,omitempty"`
}

func (tc ReasoningContent) String() string {
	return tc.Thinking
}
func (ReasoningContent) isPart() {}

type TextContent struct {
	Text             string           `json:"text"`
	ProviderMetadata ProviderMetadata `json:"provider_metadata,omitempty"`
}

func (tc TextContent) String() string {
	return tc.Text
}

func (TextContent) isPart() {}

type ImageURLContent struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func (iuc ImageURLContent) String() string {
	return iuc.URL
}

func (ImageURLContent) isPart() {}

type BinaryContent struct {
	Path     string
	MIMEType string
	Data     []byte
}

func (bc BinaryContent) String(p catalog.ProviderID) string {
	base64Encoded := base64.StdEncoding.EncodeToString(bc.Data)
	if p == catalog.ProviderOpenAI {
		return "data:" + bc.MIMEType + ";base64," + base64Encoded
	}
	return base64Encoded
}

func (BinaryContent) isPart() {}

type ToolCall struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	Input            string           `json:"input"`
	ProviderExecuted bool             `json:"provider_executed"`
	Finished         bool             `json:"finished"`
	ProviderMetadata ProviderMetadata `json:"provider_metadata,omitempty"`
}

func (ToolCall) isPart() {}

type ToolResult struct {
	ToolCallID       string           `json:"tool_call_id"`
	Name             string           `json:"name"`
	Content          string           `json:"content"`
	Data             string           `json:"data"`
	MIMEType         string           `json:"mime_type"`
	Metadata         string           `json:"metadata"`
	IsError          bool             `json:"is_error"`
	ProviderExecuted bool             `json:"provider_executed,omitempty"`
	ProviderMetadata ProviderMetadata `json:"provider_metadata,omitempty"`
}

func (ToolResult) isPart() {}

type Finish struct {
	Reason  FinishReason `json:"reason"`
	Time    int64        `json:"time"`
	Message string       `json:"message,omitempty"`
	Details string       `json:"details,omitempty"`
}

func (Finish) isPart() {}

type RetryingContent struct{}

func (RetryingContent) isPart() {}

// ShellCommand stores a bang-mode shell command and its output as a
// distinct content part so it can be reconstructed on session restore.
type ShellCommand struct {
	Command  string `json:"command"`
	Output   string `json:"output"`
	ExitCode int    `json:"exit_code"`
}

func (ShellCommand) isPart() {}

// HasShellCommand reports whether the message contains any ShellCommand parts.
func (m *Message) HasShellCommand() bool {
	for _, part := range m.Parts {
		if _, ok := part.(ShellCommand); ok {
			return true
		}
	}
	return false
}

// ShellCommands returns all ShellCommand parts from the message.
func (m *Message) ShellCommands() []ShellCommand {
	var cmds []ShellCommand
	for _, part := range m.Parts {
		if sc, ok := part.(ShellCommand); ok {
			cmds = append(cmds, sc)
		}
	}
	return cmds
}

type Message struct {
	ID               string
	Role             MessageRole
	SessionID        string
	Parts            []ContentPart
	Model            string
	Provider         string
	CreatedAt        int64
	UpdatedAt        int64
	IsSummaryMessage bool
}

// SetMessageProviderMetadata replaces the metadata-only part with a deep copy.
func (m *Message) SetMessageProviderMetadata(metadata ProviderMetadata) {
	for i, part := range m.Parts {
		if _, ok := part.(ProviderMetadataContent); ok {
			if len(metadata) == 0 {
				m.Parts = append(m.Parts[:i], m.Parts[i+1:]...)
			} else {
				m.Parts[i] = ProviderMetadataContent{ProviderMetadata: metadata.Clone()}
			}
			return
		}
	}
	if len(metadata) > 0 {
		m.Parts = append(m.Parts, ProviderMetadataContent{ProviderMetadata: metadata.Clone()})
	}
}

// MetadataContent returns all message-level opaque metadata envelopes.
func (m *Message) MetadataContent() ProviderMetadata {
	var metadata ProviderMetadata
	for _, part := range m.Parts {
		if content, ok := part.(ProviderMetadataContent); ok {
			metadata = append(metadata, content.ProviderMetadata...)
		}
	}
	return metadata
}

func (m *Message) Content() TextContent {
	for _, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			return c
		}
	}
	return TextContent{}
}

func (m *Message) ReasoningContent() ReasoningContent {
	for _, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			return c
		}
	}
	return ReasoningContent{}
}

func (m *Message) ImageURLContent() []ImageURLContent {
	imageURLContents := make([]ImageURLContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ImageURLContent); ok {
			imageURLContents = append(imageURLContents, c)
		}
	}
	return imageURLContents
}

func (m *Message) BinaryContent() []BinaryContent {
	binaryContents := make([]BinaryContent, 0)
	for _, part := range m.Parts {
		if c, ok := part.(BinaryContent); ok {
			binaryContents = append(binaryContents, c)
		}
	}
	return binaryContents
}

func (m *Message) ToolCalls() []ToolCall {
	toolCalls := make([]ToolCall, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			toolCalls = append(toolCalls, c)
		}
	}
	return toolCalls
}

func (m *Message) ToolResults() []ToolResult {
	toolResults := make([]ToolResult, 0)
	for _, part := range m.Parts {
		if c, ok := part.(ToolResult); ok {
			toolResults = append(toolResults, c)
		}
	}
	return toolResults
}

func (m *Message) IsFinished() bool {
	for _, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			return true
		}
	}
	return false
}

func (m *Message) FinishPart() *Finish {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return &c
		}
	}
	return nil
}

func (m *Message) FinishReason() FinishReason {
	for _, part := range m.Parts {
		if c, ok := part.(Finish); ok {
			return c.Reason
		}
	}
	return ""
}

// IsErrorLike reports whether the message finished with an error-style
// banner (a real error or a provider safety refusal). The TUI renders
// both through the same banner path.
func (m *Message) IsErrorLike() bool {
	switch m.FinishReason() {
	case FinishReasonError, FinishReasonContentFilter:
		return true
	}
	return false
}

func (m *Message) IsThinking() bool {
	if m.ReasoningContent().Thinking != "" && m.Content().Text == "" && !m.IsFinished() {
		return true
	}
	return false
}

func (m *Message) AppendContent(delta string) {
	m.ClearRetrying()
	found := false
	for i, part := range m.Parts {
		if c, ok := part.(TextContent); ok {
			c.Text += delta
			m.Parts[i] = c
			found = true
		}
	}
	if !found {
		m.Parts = append(m.Parts, TextContent{Text: delta})
	}
}

func (m *Message) AppendReasoningContent(delta string) {
	m.ClearRetrying()
	found := false
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			c.Thinking += delta
			m.Parts[i] = c
			found = true
		}
	}
	if !found {
		m.Parts = append(m.Parts, ReasoningContent{
			Thinking:  delta,
			StartedAt: time.Now().Unix(),
		})
	}
}

// SetReasoningProviderMetadata attaches opaque metadata to the current
// reasoning part without interpreting or normalizing its payloads.
func (m *Message) SetReasoningProviderMetadata(metadata ProviderMetadata) {
	if len(metadata) == 0 {
		return
	}
	for i, part := range m.Parts {
		if content, ok := part.(ReasoningContent); ok {
			content.ProviderMetadata = append(content.ProviderMetadata, metadata.Clone()...)
			m.Parts[i] = content
			return
		}
	}
	m.Parts = append(m.Parts, ReasoningContent{ProviderMetadata: metadata.Clone()})
}

func (m *Message) FinishThinking() {
	for i, part := range m.Parts {
		if c, ok := part.(ReasoningContent); ok {
			if c.FinishedAt == 0 {
				c.FinishedAt = time.Now().Unix()
				m.Parts[i] = c
			}
			return
		}
	}
}

func (m *Message) ThinkingDuration() time.Duration {
	reasoning := m.ReasoningContent()
	if reasoning.StartedAt == 0 {
		return 0
	}

	endTime := reasoning.FinishedAt
	if endTime == 0 {
		endTime = time.Now().Unix()
	}

	return time.Duration(endTime-reasoning.StartedAt) * time.Second
}

func (m *Message) FinishToolCall(toolCallID string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				c.Finished = true
				m.Parts[i] = c
				return
			}
		}
	}
}

func (m *Message) AppendToolCallInput(toolCallID string, inputDelta string) {
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == toolCallID {
				c.Input += inputDelta
				m.Parts[i] = c
				return
			}
		}
	}
}

func (m *Message) AddToolCall(tc ToolCall) {
	m.ClearRetrying()
	for i, part := range m.Parts {
		if c, ok := part.(ToolCall); ok {
			if c.ID == tc.ID {
				m.Parts[i] = tc
				return
			}
		}
	}
	m.Parts = append(m.Parts, tc)
}

func (m *Message) SetToolCalls(tc []ToolCall) {
	// remove any existing tool call part it could have multiple
	parts := make([]ContentPart, 0)
	for _, part := range m.Parts {
		if _, ok := part.(ToolCall); ok {
			continue
		}
		parts = append(parts, part)
	}
	m.Parts = parts
	for _, toolCall := range tc {
		m.Parts = append(m.Parts, toolCall)
	}
}

func (m *Message) AddToolResult(tr ToolResult) {
	m.Parts = append(m.Parts, tr)
}

func (m *Message) SetToolResults(tr []ToolResult) {
	for _, toolResult := range tr {
		m.Parts = append(m.Parts, toolResult)
	}
}

// Clone returns a deep copy of the message with independent opaque metadata.
func (m *Message) Clone() Message {
	clone := *m
	clone.Parts = make([]ContentPart, len(m.Parts))
	for i, part := range m.Parts {
		switch value := part.(type) {
		case ReasoningContent:
			value.ProviderMetadata = value.ProviderMetadata.Clone()
			clone.Parts[i] = value
		case TextContent:
			value.ProviderMetadata = value.ProviderMetadata.Clone()
			clone.Parts[i] = value
		case ToolCall:
			value.ProviderMetadata = value.ProviderMetadata.Clone()
			clone.Parts[i] = value
		case ToolResult:
			value.ProviderMetadata = value.ProviderMetadata.Clone()
			clone.Parts[i] = value
		case ProviderMetadataContent:
			value.ProviderMetadata = value.ProviderMetadata.Clone()
			clone.Parts[i] = value
		case BinaryContent:
			value.Data = slices.Clone(value.Data)
			clone.Parts[i] = value
		default:
			clone.Parts[i] = part
		}
	}
	return clone
}

// ResetStreamedContent removes all parts that were added during streaming
// (text, reasoning, tool calls, finish) so the message is ready for a
// retry. Non-streamed parts (images, binary attachments, tool results,
// shell commands) are preserved.
func (m *Message) ResetStreamedContent() {
	kept := m.Parts[:0]
	for _, part := range m.Parts {
		switch part.(type) {
		case TextContent, ReasoningContent, ToolCall, Finish:
			// Drop streamed parts.
		default:
			kept = append(kept, part)
		}
	}
	m.Parts = kept
}

func (m *Message) SetRetrying() {
	m.ClearRetrying()
	m.Parts = append(m.Parts, RetryingContent{})
}

func (m *Message) ClearRetrying() {
	for i, part := range m.Parts {
		if _, ok := part.(RetryingContent); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			return
		}
	}
}

func (m *Message) Retrying() (RetryingContent, bool) {
	for _, part := range m.Parts {
		if c, ok := part.(RetryingContent); ok {
			return c, true
		}
	}
	return RetryingContent{}, false
}

func (m *Message) AddFinish(reason FinishReason, message, details string) {
	m.ClearRetrying()
	// remove any existing finish part
	for i, part := range m.Parts {
		if _, ok := part.(Finish); ok {
			m.Parts = slices.Delete(m.Parts, i, i+1)
			break
		}
	}
	m.Parts = append(m.Parts, Finish{Reason: reason, Time: time.Now().Unix(), Message: message, Details: details})
}

func (m *Message) AddImageURL(url, detail string) {
	m.Parts = append(m.Parts, ImageURLContent{URL: url, Detail: detail})
}

func (m *Message) AddBinary(mimeType string, data []byte) {
	m.Parts = append(m.Parts, BinaryContent{MIMEType: mimeType, Data: data})
}

func PromptWithTextAttachments(prompt string, attachments []Attachment) string {
	var sb strings.Builder
	sb.WriteString(prompt)
	addedAttachments := false
	for _, content := range attachments {
		if !content.IsText() {
			continue
		}
		if !addedAttachments {
			sb.WriteString("\n<system_info>The files below have been attached by the user, consider them in your response</system_info>\n")
			addedAttachments = true
		}
		if content.FilePath != "" {
			fmt.Fprintf(&sb, "<file path='%s'>\n", content.FilePath)
		} else {
			sb.WriteString("<file>\n")
		}
		sb.WriteString("\n")
		sb.Write(content.Content)
		sb.WriteString("\n</file>\n")
	}
	return sb.String()
}

func toolResultToAIMessagePart(result ToolResult) fantasy.ToolResultPart {
	var content fantasy.ToolResultOutputContent
	if result.IsError {
		content = fantasy.ToolResultOutputContentError{
			Error: errors.New(result.Content),
		}
	} else if result.Data != "" {
		if stringext.IsValidBase64(result.Data) {
			content = fantasy.ToolResultOutputContentMedia{
				Data:      result.Data,
				MediaType: result.MIMEType,
				Text:      result.Content,
			}
		} else {
			content = fantasy.ToolResultOutputContentText{
				Text: mediaLoadFailedPlaceholder,
			}
		}
	} else {
		content = fantasy.ToolResultOutputContentText{
			Text: result.Content,
		}
	}
	return fantasy.ToolResultPart{
		ToolCallID:       result.ToolCallID,
		Output:           content,
		ProviderExecuted: result.ProviderExecuted,
		ProviderOptions:  result.ProviderMetadata.FantasyOptions(ProviderMetadataScopeToolResult),
		ClientMetadata:   result.Metadata,
	}
}

func (m *Message) ToAIMessage() []fantasy.Message {
	var messages []fantasy.Message
	switch m.Role {
	case User:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		var textAttachments []Attachment
		for _, content := range m.BinaryContent() {
			if !strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			textAttachments = append(textAttachments, Attachment{
				FilePath: content.Path,
				MimeType: content.MIMEType,
				Content:  content.Data,
			})
		}
		text = PromptWithTextAttachments(text, textAttachments)
		// Include bang-mode shell commands as context for the agent.
		for _, sc := range m.ShellCommands() {
			shellText := fmt.Sprintf("$ %s\n%s\n(exit code %d)", sc.Command, ansi.Strip(sc.Output), sc.ExitCode)
			if text != "" {
				text += "\n\n" + shellText
			} else {
				text = shellText
			}
		}
		if text != "" {
			content := m.Content()
			textPart := fantasy.TextPart{Text: text}
			textPart.ProviderOptions = mergeProviderOptions(content.ProviderMetadata.FantasyOptions(ProviderMetadataScopeText), content.ProviderMetadata.FantasyOptions(ProviderMetadataScopeCompaction))
			parts = append(parts, textPart)
		}
		for _, content := range m.BinaryContent() {
			// skip text attachements
			if strings.HasPrefix(content.MIMEType, "text/") {
				continue
			}
			parts = append(parts, fantasy.FilePart{
				Filename:  content.Path,
				Data:      content.Data,
				MediaType: content.MIMEType,
			})
		}
		messages = append(messages, fantasy.Message{
			Role:            fantasy.MessageRoleUser,
			Content:         parts,
			ProviderOptions: mergeProviderOptions(m.MetadataContent().FantasyOptions(ProviderMetadataScopeMessage), m.MetadataContent().FantasyOptions(ProviderMetadataScopeContinuation)),
		})
	case Assistant:
		var parts []fantasy.MessagePart
		text := strings.TrimSpace(m.Content().Text)
		if text != "" {
			parts = append(parts, fantasy.TextPart{Text: text})
		}
		reasoning := m.ReasoningContent()
		if reasoning.Thinking != "" || len(reasoning.ProviderMetadata) > 0 {
			reasoningPart := fantasy.ReasoningPart{
				Text:            reasoning.Thinking,
				ProviderOptions: reasoning.ProviderMetadata.FantasyOptions(ProviderMetadataScopeReasoning),
			}
			parts = append(parts, reasoningPart)
		}
		for _, call := range m.ToolCalls() {
			providerOptions := call.ProviderMetadata.FantasyOptions(ProviderMetadataScopeToolCall)
			parts = append(parts, fantasy.ToolCallPart{
				ToolCallID:       call.ID,
				ToolName:         call.Name,
				Input:            call.Input,
				ProviderExecuted: call.ProviderExecuted,
				ProviderOptions:  providerOptions,
			})
		}
		for _, result := range m.ToolResults() {
			if result.ProviderExecuted {
				parts = append(parts, toolResultToAIMessagePart(result))
			}
		}
		messages = append(messages, fantasy.Message{
			Role:            fantasy.MessageRoleAssistant,
			Content:         parts,
			ProviderOptions: mergeProviderOptions(m.MetadataContent().FantasyOptions(ProviderMetadataScopeMessage), m.MetadataContent().FantasyOptions(ProviderMetadataScopeContinuation)),
		})
	case Tool:
		var parts []fantasy.MessagePart
		for _, result := range m.ToolResults() {
			parts = append(parts, toolResultToAIMessagePart(result))
		}
		messages = append(messages, fantasy.Message{
			Role:            fantasy.MessageRoleTool,
			Content:         parts,
			ProviderOptions: mergeProviderOptions(m.MetadataContent().FantasyOptions(ProviderMetadataScopeMessage), m.MetadataContent().FantasyOptions(ProviderMetadataScopeContinuation)),
		})
	}
	return messages
}

func mergeProviderOptions(groups ...fantasy.ProviderOptions) fantasy.ProviderOptions {
	var merged fantasy.ProviderOptions
	for _, group := range groups {
		for provider, value := range group {
			if merged == nil {
				merged = make(fantasy.ProviderOptions)
			}
			merged[provider] = value
		}
	}
	return merged
}
