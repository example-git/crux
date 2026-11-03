package pathtranslate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

var ErrStaleCheckpoint = errors.New("compaction checkpoint requires rebuilding for the current path translation policy")

func (t *Translator) Prompt(ctx context.Context, prompt fantasy.Prompt) (fantasy.Prompt, error) {
	if err := t.CheckCheckpoints(prompt); err != nil {
		return nil, err
	}
	result := make(fantasy.Prompt, len(prompt))
	toolNames := make(map[string]string)
	toolInputs := make(map[string]string)
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
				toolNames[call.ToolCallID] = call.ToolName
				toolInputs[call.ToolCallID] = call.Input
			}
		}
	}
	for messageIndex, msg := range prompt {
		result[messageIndex] = msg
		result[messageIndex].Content = make([]fantasy.MessagePart, len(msg.Content))
		for partIndex, part := range msg.Content {
			if !t.settings.Enabled {
				if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok && file.PathTranslationOriginal != nil {
					part = *file.PathTranslationOriginal
				}
				if toolResult, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
					projection, err := readToolOutputProjection(toolResult.ClientMetadata)
					if err != nil {
						return nil, err
					}
					if projection != nil {
						toolResult, err = restoreToolOutput(toolResult, projection)
						if err != nil {
							return nil, err
						}
						part = toolResult
					} else if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](toolResult.Output); ok && output.ImageFile != nil && output.ImageFile.PathTranslationOriginal != nil {
						file := *output.ImageFile.PathTranslationOriginal
						output.Data, output.MediaType = base64.StdEncoding.EncodeToString(file.Data), file.MediaType
						output.ImageFile = &file
						toolResult.Output = output
						part = toolResult
					}
				}
				result[messageIndex].Content[partIndex] = part
				continue
			}
			translated, err := t.messagePart(ctx, part, toolNames, toolInputs)
			if err != nil {
				return nil, err
			}
			result[messageIndex].Content[partIndex] = translated
		}
	}
	if !t.settings.Enabled {
		return result, nil
	}
	return t.replayMetadata(ctx, result)
}

func (t *Translator) CheckCheckpoints(prompt fantasy.Prompt) error {
	fingerprint := t.Fingerprint()
	for _, msg := range prompt {
		for _, part := range msg.Content {
			if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok && text.OpaqueCheckpoint && text.PathTranslationFingerprint != fingerprint {
				return ErrStaleCheckpoint
			}
		}
	}
	return nil
}

func (t *Translator) messagePart(ctx context.Context, part fantasy.MessagePart, toolNames, toolInputs map[string]string) (fantasy.MessagePart, error) {
	if text, ok := fantasy.AsMessagePart[fantasy.TextPart](part); ok {
		value, err := t.Text(ctx, text.Text)
		text.Text = value
		return text, err
	}
	if reasoning, ok := fantasy.AsMessagePart[fantasy.ReasoningPart](part); ok {
		value, err := t.Text(ctx, reasoning.Text)
		if err == nil && value != reasoning.Text && len(reasoning.ProviderOptions) > 0 {
			return fantasy.TextPart{Text: "Prior reasoning:\n" + value}, nil
		}
		reasoning.Text = value
		return reasoning, err
	}
	if call, ok := fantasy.AsMessagePart[fantasy.ToolCallPart](part); ok {
		if call.Input == "" {
			return call, nil
		}
		value, err := t.JSON(ctx, call.Input, false)
		call.Input = value
		return call, err
	}
	if file, ok := fantasy.AsMessagePart[fantasy.FilePart](part); ok {
		return t.ImageFile(ctx, file)
	}
	if result, ok := fantasy.AsMessagePart[fantasy.ToolResultPart](part); ok {
		toolName := toolNames[result.ToolCallID]
		projection, err := readToolOutputProjection(result.ClientMetadata)
		if err != nil {
			return nil, err
		}
		if projection != nil {
			if projection.Fingerprint == t.Fingerprint() {
				if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Output); ok {
					sources, err := ToolImageSources(projection.OriginalMetadata, "")
					if err != nil {
						return nil, err
					}
					data, err := base64.StdEncoding.DecodeString(output.Data)
					if err != nil {
						return nil, err
					}
					output.ImageFile = &fantasy.FilePart{
						Data: data, MediaType: output.MediaType, SourcePaths: sources,
						PathTranslated: true, PathTranslationPolicy: projection.Fingerprint,
						NeedsImageNormalization: projection.ImageCensored,
						PathTranslationOriginal: &fantasy.FilePart{Data: projection.OriginalData, MediaType: projection.OriginalMediaType, SourcePaths: sources},
					}
					result.Output = output
				}
				result.ClientMetadata = ""
				return result, nil
			}
			result, err = restoreToolOutput(result, projection)
			if err != nil {
				return nil, err
			}
			toolName = projection.ToolName
		}
		t, err = t.withToolPaths(ctx, toolInputs[result.ToolCallID])
		if err != nil {
			return nil, err
		}
		if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Output); ok {
			output.Text, err = t.toolResultText(ctx, output.Text, result.ClientMetadata, toolName, false)
			result.Output = output
		} else if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Output); ok {
			if output.Error != nil {
				var text string
				text, err = t.toolResultText(ctx, output.Error.Error(), result.ClientMetadata, toolName, true)
				output.Error = errors.New(text)
			}
			result.Output = output
		} else if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Output); ok {
			output.Text, err = t.Text(ctx, output.Text)
			if err != nil {
				return nil, err
			}
			sources, err := ToolImageSources(result.ClientMetadata, toolInputs[result.ToolCallID])
			if err != nil {
				return nil, fmt.Errorf("decode tool image provenance: %w", err)
			}
			data, err := base64.StdEncoding.DecodeString(output.Data)
			if err != nil {
				return nil, err
			}
			file := fantasy.FilePart{SourcePaths: sources, Data: data, MediaType: output.MediaType}
			if output.ImageFile != nil {
				file = *output.ImageFile
			}
			file, err = t.ImageFile(ctx, file)
			if err != nil {
				return nil, err
			}
			output.Data, output.MediaType = base64.StdEncoding.EncodeToString(file.Data), file.MediaType
			output.ImageFile = &file
			result.Output = output
		}
		result.ClientMetadata = ""
		return result, err
	}
	return part, nil
}

func (t *Translator) toolResultText(ctx context.Context, text, metadata, toolName string, failed bool) (string, error) {
	var references struct {
		Paths            json.RawMessage `json:"path_references"`
		ShellPaths       []string        `json:"shell_output_paths"`
		WorkingDirectory string          `json:"working_directory"`
	}
	if metadata != "" {
		if err := json.Unmarshal([]byte(metadata), &references); err != nil {
			return "", err
		}
	}
	shellOutput := len(references.ShellPaths) > 0 || toolName == "bash" || toolName == "job_output"
	if shellOutput {
		if references.WorkingDirectory != "" {
			references.ShellPaths = append(references.ShellPaths, references.WorkingDirectory)
		}
		var stored []PathReference
		if references.Paths != nil {
			if err := json.Unmarshal(references.Paths, &stored); err != nil {
				return "", err
			}
		}
		var current []PathReference
		var err error
		if toolName == "task_output" && !failed && json.Valid([]byte(text)) {
			current, err = t.OutputJSONReferences(text, references.ShellPaths)
		} else {
			current, err = t.OutputReferences(text, references.ShellPaths)
		}
		if err != nil {
			return "", err
		}
		current, err = mergePathReferences(text, current, stored)
		if err != nil {
			return "", err
		}
		encoded, err := json.Marshal(PathReferences{Paths: current})
		if err != nil {
			return "", err
		}
		return t.ReferencedText(ctx, text, string(encoded))
	}
	if references.Paths != nil {
		return t.ReferencedText(ctx, text, metadata)
	}
	if failed {
		return t.Text(ctx, text)
	}
	if toolName == "ls" {
		return t.Listing(ctx, text)
	}
	metadata, err := searchPathMetadata(text, metadata, toolName)
	if err != nil {
		return "", err
	}
	return t.ReferencedText(ctx, text, metadata)
}

func (t *Translator) Listing(ctx context.Context, text string) (string, error) {
	lines := strings.Split(text, "\n")
	var parents []string
	for index, line := range lines {
		trimmed := strings.TrimLeft(line, " ")
		if !strings.HasPrefix(trimmed, "- ") {
			translated, err := t.Text(ctx, line)
			if err != nil {
				return "", err
			}
			lines[index] = translated
			continue
		}
		depth := (len(line) - len(trimmed)) / 2
		name := strings.TrimPrefix(trimmed, "- ")
		path := strings.TrimSuffix(name, "/")
		if depth > 0 {
			if depth > len(parents) {
				return "", errors.New("invalid directory listing depth")
			}
			path = filepath.Join(parents[depth-1], path)
		}
		if depth == 0 && !filepath.IsAbs(path) {
			return "", errors.New("directory listing root must be absolute")
		}
		parents = append(parents[:depth], path)
		translated, err := t.Path(ctx, path)
		if err != nil {
			return "", err
		}
		if depth > 0 {
			translated = filepath.Base(translated)
		}
		if strings.HasSuffix(name, "/") {
			translated += "/"
		}
		lines[index] = strings.Repeat("  ", depth) + "- " + translated
	}
	return strings.Join(lines, "\n"), nil
}
