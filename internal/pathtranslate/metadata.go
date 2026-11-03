package pathtranslate

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	fantasy "github.com/example-git/crux/foundation"
)

func (t *Translator) metadataDigest(namespace string, value fantasy.ProviderOptionsData) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return t.names.identity("metadata\x00" + namespace + "\x00" + string(encoded)), nil
}

func (t *Translator) RememberMetadata(ctx context.Context, metadata fantasy.ProviderMetadata) error {
	if t == nil || !t.settings.Enabled || t.names == nil {
		return nil
	}
	for namespace, value := range metadata {
		digest, err := t.metadataDigest(namespace, value)
		if err != nil {
			return err
		}
		if _, err := t.names.store.db.ExecContext(ctx, `INSERT INTO path_translation_metadata (digest, policy) VALUES (?, ?) ON CONFLICT DO NOTHING`, digest, t.Fingerprint()); err != nil {
			return fmt.Errorf("record provider metadata policy: %w", err)
		}
	}
	return nil
}

func (t *Translator) metadataCurrent(ctx context.Context, metadata fantasy.ProviderOptions) (bool, error) {
	if len(metadata) == 0 {
		return true, nil
	}
	for namespace, value := range metadata {
		digest, err := t.metadataDigest(namespace, value)
		if err != nil {
			return false, err
		}
		var present int
		err = t.names.store.db.QueryRowContext(ctx, `SELECT 1 FROM path_translation_metadata WHERE digest = ? AND policy = ?`, digest, t.Fingerprint()).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

func (t *Translator) replayMetadata(ctx context.Context, prompt fantasy.Prompt) (fantasy.Prompt, error) {
	projectedCalls := make(map[string]bool)
	for _, msg := range prompt {
		for _, part := range msg.Content {
			current, err := t.metadataCurrent(ctx, part.Options())
			if err != nil {
				return nil, err
			}
			if current {
				continue
			}
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				projectedCalls[call.ToolCallID] = true
			}
			if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
				projectedCalls[result.ToolCallID] = true
			}
		}
	}
	result := make(fantasy.Prompt, 0, len(prompt))
	for _, msg := range prompt {
		if msg.Role == fantasy.MessageRoleSystem {
			result = append(result, msg)
			continue
		}
		translatedMessage := msg
		current, err := t.metadataCurrent(ctx, msg.ProviderOptions)
		if err != nil {
			return nil, err
		}
		if !current && msg.Role != fantasy.MessageRoleSystem {
			translatedMessage.ProviderOptions = nil
		}
		var parts []fantasy.MessagePart
		toolResults := false
		for _, part := range msg.Content {
			current, err := t.metadataCurrent(ctx, part.Options())
			if err != nil {
				return nil, err
			}
			switch {
			case part.GetType() == fantasy.ContentTypeReasoning:
				reasoning, _ := fantasy.AsMessagePart[fantasy.ReasoningPart](part)
				translated, err := t.Text(ctx, reasoning.Text)
				if err != nil {
					return nil, err
				}
				if !current || translated != reasoning.Text {
					if translated != "" {
						parts = append(parts, fantasy.TextPart{Text: "Prior reasoning:\n" + translated})
					} else {
						parts = append(parts, fantasy.TextPart{Text: "Prior provider reasoning had no visible text."})
					}
					continue
				}
			case part.GetType() == fantasy.ContentTypeToolCall:
				call, _ := fantasy.AsMessagePart[fantasy.ToolCallPart](part)
				if projectedCalls[call.ToolCallID] {
					parts = append(parts, fantasy.TextPart{Text: fmt.Sprintf("Prior tool call %s (%s):\n%s", call.ToolName, call.ToolCallID, call.Input)})
					continue
				}
			case part.GetType() == fantasy.ContentTypeToolResult:
				tool, _ := fantasy.AsMessagePart[fantasy.ToolResultPart](part)
				if projectedCalls[tool.ToolCallID] {
					if tool.Output == nil {
						parts = append(parts, fantasy.TextPart{Text: fmt.Sprintf("Prior tool result (%s) had no visible output.", tool.ToolCallID)})
						continue
					}
					if text, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](tool.Output); ok {
						parts = append(parts, fantasy.TextPart{Text: fmt.Sprintf("Prior tool result (%s):\n%s", tool.ToolCallID, text.Text)})
						continue
					}
					if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tool.Output); ok {
						parts = append(parts, fantasy.TextPart{Text: fmt.Sprintf("Prior tool error (%s):\n%v", tool.ToolCallID, output.Error)})
						continue
					}
					if media, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](tool.Output); ok {
						data, err := base64.StdEncoding.DecodeString(media.Data)
						if err != nil {
							return nil, err
						}
						file := fantasy.FilePart{Data: data, MediaType: media.MediaType, PathTranslated: true, PathTranslationPolicy: t.Fingerprint(), NeedsImageNormalization: true}
						if media.ImageFile != nil {
							file = *media.ImageFile
						}
						parts = append(parts, fantasy.TextPart{Text: fmt.Sprintf("Prior tool result (%s):\n%s", tool.ToolCallID, media.Text)}, file)
						continue
					}
				}
				toolResults = true
			case part.GetType() == fantasy.ContentTypeText:
				text, _ := fantasy.AsMessagePart[fantasy.TextPart](part)
				if !current && !text.OpaqueCheckpoint {
					text.ProviderOptions = nil
					part = text
				}
			case part.GetType() == fantasy.ContentTypeFile:
				file, _ := fantasy.AsMessagePart[fantasy.FilePart](part)
				if !current {
					file.ProviderOptions = nil
					part = file
				}
			}
			parts = append(parts, part)
		}
		translatedMessage.Content = parts
		if msg.Role == fantasy.MessageRoleTool {
			if !toolResults {
				translatedMessage.Role = fantasy.MessageRoleUser
			} else {
				var native, visible []fantasy.MessagePart
				for _, part := range parts {
					if part.GetType() == fantasy.ContentTypeToolResult {
						native = append(native, part)
					} else {
						visible = append(visible, part)
					}
				}
				if len(visible) > 0 {
					translatedMessage.Content = native
					result = append(result, translatedMessage, fantasy.Message{Role: fantasy.MessageRoleUser, Content: visible})
					continue
				}
			}
		}
		result = append(result, translatedMessage)
	}
	return result, nil
}

func (t *Translator) RememberResponse(ctx context.Context, response *fantasy.Response) error {
	if response == nil {
		return nil
	}
	if err := t.RememberMetadata(ctx, response.ProviderMetadata); err != nil {
		return err
	}
	for _, part := range response.Content {
		var metadata fantasy.ProviderMetadata
		switch part.GetType() {
		case fantasy.ContentTypeText:
			value, _ := fantasy.AsContentType[fantasy.TextContent](part)
			metadata = value.ProviderMetadata
		case fantasy.ContentTypeReasoning:
			value, _ := fantasy.AsContentType[fantasy.ReasoningContent](part)
			metadata = value.ProviderMetadata
		case fantasy.ContentTypeToolCall:
			value, _ := fantasy.AsContentType[fantasy.ToolCallContent](part)
			metadata = value.ProviderMetadata
		case fantasy.ContentTypeToolResult:
			value, _ := fantasy.AsContentType[fantasy.ToolResultContent](part)
			metadata = value.ProviderMetadata
		case fantasy.ContentTypeFile:
			value, _ := fantasy.AsContentType[fantasy.FileContent](part)
			metadata = value.ProviderMetadata
		case fantasy.ContentTypeSource:
			value, _ := fantasy.AsContentType[fantasy.SourceContent](part)
			metadata = value.ProviderMetadata
		}
		if err := t.RememberMetadata(ctx, metadata); err != nil {
			return err
		}
	}
	return nil
}
