package pathtranslate

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
)

const toolOutputProjectionKey = "path_translation_output"

type toolOutputProjection struct {
	Fingerprint       string `json:"fingerprint"`
	ToolName          string `json:"tool_name"`
	OriginalContent   string `json:"original_content"`
	OriginalMetadata  string `json:"original_metadata"`
	OriginalType      string `json:"original_type,omitempty"`
	OriginalMediaType string `json:"original_media_type,omitempty"`
	OriginalData      []byte `json:"original_data,omitempty"`
	ImageCensored     bool   `json:"image_censored,omitempty"`
}

func readToolOutputProjection(metadata string) (*toolOutputProjection, error) {
	if !strings.Contains(metadata, `"`+toolOutputProjectionKey+`"`) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(metadata), &fields); err != nil {
		return nil, err
	}
	raw, exists := fields[toolOutputProjectionKey]
	if !exists {
		return nil, nil
	}
	var projection toolOutputProjection
	if err := json.Unmarshal(raw, &projection); err != nil {
		return nil, err
	}
	if projection.Fingerprint == "" || projection.ToolName == "" {
		return nil, errors.New("invalid translated tool output provenance")
	}
	return &projection, nil
}

func restoreToolOutput(result fantasy.ToolResultPart, projection *toolOutputProjection) (fantasy.ToolResultPart, error) {
	if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentText](result.Output); ok {
		output.Text = projection.OriginalContent
		result.Output = output
	} else if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Output); ok {
		output.Error = errors.New(projection.OriginalContent)
		result.Output = output
	} else if output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Output); ok && projection.OriginalType != "" {
		output.Text, output.MediaType = projection.OriginalContent, projection.OriginalMediaType
		output.Data = base64.StdEncoding.EncodeToString(projection.OriginalData)
		output.ImageFile = nil
		result.Output = output
	} else {
		return result, errors.New("translated tool output provenance does not match the result")
	}
	result.ClientMetadata = projection.OriginalMetadata
	return result, nil
}

func RestoreToolResult(result fantasy.ToolResultPart) (fantasy.ToolResultPart, error) {
	projection, err := readToolOutputProjection(result.ClientMetadata)
	if err != nil {
		return result, err
	}
	if projection != nil {
		return restoreToolOutput(result, projection)
	}
	output, ok := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentMedia](result.Output)
	if !ok || output.ImageFile == nil || output.ImageFile.PathTranslationOriginal == nil {
		return result, nil
	}
	original := output.ImageFile.PathTranslationOriginal
	fields := make(map[string]json.RawMessage)
	if result.ClientMetadata != "" {
		if err := json.Unmarshal([]byte(result.ClientMetadata), &fields); err != nil {
			return result, err
		}
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	fields["image_source_paths"], err = json.Marshal(append(append([]string(nil), original.SourcePaths...), original.SourcePath))
	if err != nil {
		return result, err
	}
	metadata, err := json.Marshal(fields)
	if err != nil {
		return result, err
	}
	output.Data, output.MediaType = base64.StdEncoding.EncodeToString(original.Data), original.MediaType
	output.ImageFile = nil
	result.Output, result.ClientMetadata = output, string(metadata)
	return result, nil
}

func (t *Translator) ToolResponse(ctx context.Context, toolName, input string, response fantasy.ToolResponse) (fantasy.ToolResponse, error) {
	projection, err := readToolOutputProjection(response.Metadata)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if projection != nil {
		if t.settings.Enabled && projection.Fingerprint == t.Fingerprint() {
			return response, nil
		}
		response.Content, response.Metadata = projection.OriginalContent, projection.OriginalMetadata
		if projection.OriginalType != "" {
			response.Type, response.MediaType, response.Data = projection.OriginalType, projection.OriginalMediaType, projection.OriginalData
		}
	}
	if !t.settings.Enabled {
		return response, nil
	}
	media := !response.IsError && (response.Type == "image" || response.Type == "media")
	if media && !imageData(response.MediaType, response.Data) {
		return response, nil
	}
	t, err = t.withToolPaths(ctx, input)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	var sources []string
	if media {
		sources, err = ToolImageSources(response.Metadata, input)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		sources, err = t.imageSources(ctx, sources)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		fields := make(map[string]json.RawMessage)
		if response.Metadata != "" {
			if err := json.Unmarshal([]byte(response.Metadata), &fields); err != nil {
				return fantasy.ToolResponse{}, err
			}
		}
		if fields == nil {
			fields = make(map[string]json.RawMessage)
		}
		fields["image_source_paths"], err = json.Marshal(sources)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		encoded, err := json.Marshal(fields)
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		response.Metadata = string(encoded)
	}
	original := toolOutputProjection{
		Fingerprint: t.Fingerprint(), ToolName: toolName,
		OriginalContent: response.Content, OriginalMetadata: response.Metadata,
	}
	if media {
		original.OriginalType, original.OriginalMediaType, original.OriginalData = response.Type, response.MediaType, response.Data
		file, err := t.ImageFile(ctx, fantasy.FilePart{SourcePaths: sources, Data: response.Data, MediaType: response.MediaType})
		if err != nil {
			return fantasy.ToolResponse{}, err
		}
		response.Data, response.MediaType = file.Data, file.MediaType
		original.ImageCensored = file.NeedsImageNormalization
	}
	response.Content, err = t.toolResultText(ctx, response.Content, response.Metadata, toolName, response.IsError)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	fields := make(map[string]json.RawMessage)
	if response.Metadata != "" {
		if err := json.Unmarshal([]byte(response.Metadata), &fields); err != nil {
			return fantasy.ToolResponse{}, err
		}
	}
	if fields == nil {
		fields = make(map[string]json.RawMessage)
	}
	delete(fields, "path_references")
	outputMetadata, err := json.Marshal(fields)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	delete(fields, "shell_output_paths")
	displayMetadata, err := json.Marshal(fields)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	translatedMetadata, err := transformJSONStrings(string(displayMetadata), func(key, value string) (string, error) {
		if key == "output" || key == "content" {
			return t.toolResultText(ctx, value, string(outputMetadata), toolName, response.IsError)
		}
		return value, nil
	})
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	if err := json.Unmarshal([]byte(translatedMetadata), &fields); err != nil {
		return fantasy.ToolResponse{}, err
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	fields[toolOutputProjectionKey] = encoded
	encoded, err = json.Marshal(fields)
	if err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("encode translated tool output: %w", err)
	}
	response.Metadata = string(encoded)
	return response, nil
}
