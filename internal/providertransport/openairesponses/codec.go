package openairesponses

import (
	"encoding/json"
	"fmt"
	"strings"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/providerplugin/manifest"
)

func encodeToolCodecCall(call fantasy.Call, codec *manifest.ToolCodec) (fantasy.Call, error) {
	if codec == nil {
		return call, nil
	}
	call.Prompt = append(fantasy.Prompt(nil), call.Prompt...)
	for messageIndex := range call.Prompt {
		message := call.Prompt[messageIndex]
		message.Content = append([]fantasy.MessagePart(nil), message.Content...)
		for partIndex, part := range message.Content {
			switch value := part.(type) {
			case fantasy.TextPart:
				if message.Role == fantasy.MessageRoleSystem && toolCodecSurface(codec, "prompt-references") {
					value.Text = encodeToolReferences(value.Text, codec)
					message.Content[partIndex] = value
				}
			case *fantasy.TextPart:
				if message.Role == fantasy.MessageRoleSystem && toolCodecSurface(codec, "prompt-references") {
					clone := *value
					clone.Text = encodeToolReferences(clone.Text, codec)
					message.Content[partIndex] = &clone
				}
			case fantasy.ToolCallPart:
				if toolCodecSurface(codec, "history-calls") {
					host := value.ToolName
					if provider, ok := outboundToolName(host, codec); ok {
						value.ToolName = provider
						var err error
						value.Input, err = remapToolInput(value.Input, host, codec, true)
						if err != nil {
							return fantasy.Call{}, err
						}
						message.Content[partIndex] = value
					}
				}
			case *fantasy.ToolCallPart:
				if toolCodecSurface(codec, "history-calls") {
					clone := *value
					host := clone.ToolName
					if provider, ok := outboundToolName(host, codec); ok {
						clone.ToolName = provider
						var err error
						clone.Input, err = remapToolInput(clone.Input, host, codec, true)
						if err != nil {
							return fantasy.Call{}, err
						}
						message.Content[partIndex] = &clone
					}
				}
			}
		}
		call.Prompt[messageIndex] = message
	}
	call.Tools = append([]fantasy.Tool(nil), call.Tools...)
	if toolCodecSurface(codec, "definitions") {
		for index, tool := range call.Tools {
			var function fantasy.FunctionTool
			switch value := tool.(type) {
			case fantasy.FunctionTool:
				function = value
			case *fantasy.FunctionTool:
				function = *value
			default:
				continue
			}
			host := function.Name
			provider, ok := outboundToolName(host, codec)
			if !ok {
				continue
			}
			function.Name = provider
			if toolCodecSurface(codec, "prompt-references") {
				function.Description = encodeToolReferences(function.Description, codec)
			}
			function.InputSchema = cloneMap(function.InputSchema)
			remapToolSchema(function.InputSchema, host, codec)
			call.Tools[index] = function
		}
	}
	if call.ToolChoice != nil && toolCodecSurface(codec, "definitions") {
		choice := string(*call.ToolChoice)
		if provider, ok := outboundToolName(choice, codec); ok {
			mapped := fantasy.SpecificToolChoice(provider)
			call.ToolChoice = &mapped
		}
	}
	return call, nil
}

func decodeToolCodecResponse(response *fantasy.Response, codec *manifest.ToolCodec) error {
	if response == nil || codec == nil || !toolCodecSurface(codec, "stream-events") {
		return nil
	}
	for index, content := range response.Content {
		switch value := content.(type) {
		case fantasy.ToolCallContent:
			if err := decodeToolCall(&value.ToolName, &value.Input, codec); err != nil {
				return err
			}
			response.Content[index] = value
		case *fantasy.ToolCallContent:
			clone := *value
			if err := decodeToolCall(&clone.ToolName, &clone.Input, codec); err != nil {
				return err
			}
			response.Content[index] = &clone
		case fantasy.ToolResultContent:
			if host, ok := inboundToolName(value.ToolName, codec); ok {
				value.ToolName = host
				response.Content[index] = value
			}
		case *fantasy.ToolResultContent:
			if host, ok := inboundToolName(value.ToolName, codec); ok {
				clone := *value
				clone.ToolName = host
				response.Content[index] = &clone
			}
		}
	}
	return nil
}

func decodeToolCodecStreamPart(part fantasy.StreamPart, codec *manifest.ToolCodec) (fantasy.StreamPart, error) {
	if codec == nil || !toolCodecSurface(codec, "stream-events") || part.ToolCallName == "" {
		return part, nil
	}
	provider := part.ToolCallName
	host, ok := inboundToolName(provider, codec)
	if !ok {
		return part, nil
	}
	part.ToolCallName = host
	if part.Type == fantasy.StreamPartTypeToolCall && part.ToolCallInput != "" {
		input, err := remapToolInput(part.ToolCallInput, host, codec, false)
		if err != nil {
			return fantasy.StreamPart{}, err
		}
		part.ToolCallInput = input
	}
	return part, nil
}

func decodeToolCall(name, input *string, codec *manifest.ToolCodec) error {
	provider := *name
	host, ok := inboundToolName(provider, codec)
	if !ok {
		return nil
	}
	*name = host
	mapped, err := remapToolInput(*input, host, codec, false)
	if err != nil {
		return err
	}
	*input = mapped
	return nil
}

func outboundToolName(host string, codec *manifest.ToolCodec) (string, bool) {
	for _, alias := range codec.Aliases {
		if alias.Host == host {
			return alias.Provider, true
		}
	}
	return "", false
}

func inboundToolName(provider string, codec *manifest.ToolCodec) (string, bool) {
	key := provider
	if codec.CaseFoldInbound {
		key = strings.ToLower(key)
	}
	for _, alias := range codec.Aliases {
		candidate := alias.Provider
		if codec.CaseFoldInbound {
			candidate = strings.ToLower(candidate)
		}
		if candidate == key {
			return alias.Host, true
		}
	}
	return "", false
}

func remapToolInput(input, host string, codec *manifest.ToolCodec, outbound bool) (string, error) {
	var document map[string]any
	if err := json.Unmarshal([]byte(input), &document); err != nil {
		return "", fmt.Errorf("tool %q input is not valid JSON: %w", host, err)
	}
	for _, mapping := range codec.Parameters {
		if mapping.Tool != host {
			continue
		}
		from, to := mapping.Provider, mapping.Host
		if outbound {
			from, to = mapping.Host, mapping.Provider
		}
		if value, ok := document[from]; ok {
			document[to] = value
			delete(document, from)
		}
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", fmt.Errorf("encode tool %q input: %w", host, err)
	}
	return string(encoded), nil
}

func remapToolSchema(schema map[string]any, host string, codec *manifest.ToolCodec) {
	properties, _ := schema["properties"].(map[string]any)
	for _, mapping := range codec.Parameters {
		if mapping.Tool != host {
			continue
		}
		if value, ok := properties[mapping.Host]; ok {
			properties[mapping.Provider] = value
			delete(properties, mapping.Host)
		}
		switch required := schema["required"].(type) {
		case []any:
			for index, value := range required {
				if value == mapping.Host {
					required[index] = mapping.Provider
				}
			}
		case []string:
			for index, value := range required {
				if value == mapping.Host {
					required[index] = mapping.Provider
				}
			}
		}
	}
}

func encodeToolReferences(value string, codec *manifest.ToolCodec) string {
	for _, alias := range codec.Aliases {
		value = strings.ReplaceAll(value, "`"+alias.Host+"`", "`"+alias.Provider+"`")
	}
	return value
}

func cloneMap(value map[string]any) map[string]any {
	encoded, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var clone map[string]any
	if json.Unmarshal(encoded, &clone) != nil {
		return value
	}
	return clone
}

func toolCodecSurface(codec *manifest.ToolCodec, surface string) bool {
	for _, candidate := range codec.Surfaces {
		if candidate == surface {
			return true
		}
	}
	return false
}
