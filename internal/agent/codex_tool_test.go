package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	codexresponses "github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/stretchr/testify/require"
)

type codexTestTool struct {
	response fantasy.ToolResponse
	err      error
	panic    any
	options  fantasy.ProviderOptions
}

func (t *codexTestTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "test"}
}

func (t *codexTestTool) Run(context.Context, fantasy.ToolCall) (fantasy.ToolResponse, error) {
	if t.panic != nil {
		panic(t.panic)
	}
	return t.response, t.err
}

func (t *codexTestTool) ProviderOptions() fantasy.ProviderOptions {
	return t.options
}

func (t *codexTestTool) SetProviderOptions(options fantasy.ProviderOptions) {
	t.options = options
}

func TestCodexBoundedToolTruncatesContentAndPreservesResponseFields(t *testing.T) {
	content := strings.Repeat("head", 4_000) + strings.Repeat("tail", 4_000)
	inner := &codexTestTool{response: fantasy.ToolResponse{
		Type:      "media",
		Content:   content,
		Data:      []byte{1, 2, 3},
		MediaType: "application/octet-stream",
		Metadata:  `{"source":"test"}`,
		IsError:   true,
		StopTurn:  true,
	}}
	tool := &codexBoundedTool{tool: inner}

	response, err := tool.Run(t.Context(), fantasy.ToolCall{Name: "test"})
	require.NoError(t, err)
	require.Equal(t, codexresponses.TruncateToolOutput("gpt-5.2", content), response.Content)
	require.LessOrEqual(t, len(response.Content), 12_000)
	require.Equal(t, inner.response.Type, response.Type)
	require.Equal(t, inner.response.Data, response.Data)
	require.Equal(t, inner.response.MediaType, response.MediaType)
	require.Equal(t, inner.response.Metadata, response.Metadata)
	require.Equal(t, inner.response.IsError, response.IsError)
	require.Equal(t, inner.response.StopTurn, response.StopTurn)
}

func TestCodexBoundedToolPreservesOriginalImageResponse(t *testing.T) {
	pixels := image.NewNRGBA(image.Rect(0, 0, 2400, 1200))
	var encoded bytes.Buffer
	require.NoError(t, png.Encode(&encoded, pixels))
	inner := &codexTestTool{response: fantasy.ToolResponse{
		Type:      "media",
		Data:      encoded.Bytes(),
		MediaType: "image/png",
	}}
	tool := &codexBoundedTool{tool: inner, modelID: "gpt-5.4"}

	response, err := tool.Run(t.Context(), fantasy.ToolCall{Name: "view"})
	require.NoError(t, err)
	require.Equal(t, "image/png", response.MediaType)
	require.Equal(t, encoded.Bytes(), response.Data)
	config, format, err := image.DecodeConfig(bytes.NewReader(response.Data))
	require.NoError(t, err)
	require.Equal(t, "png", format)
	require.Equal(t, 2400, config.Width)
	require.Equal(t, 1200, config.Height)
}

func TestCodexBoundedToolTruncatesReturnedErrorsAndPreservesUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	inner := &codexTestTool{err: fmt.Errorf("%s: %w", strings.Repeat("error", 4_000), sentinel)}
	tool := &codexBoundedTool{tool: inner, modelID: "gpt-5.2"}

	_, err := tool.Run(t.Context(), fantasy.ToolCall{Name: "test"})
	require.Error(t, err)
	require.ErrorIs(t, err, sentinel)
	require.LessOrEqual(t, len(err.Error()), 12_000)
	require.Contains(t, err.Error(), "tool output truncated")
}

func TestCodexBoundedToolRecoversAndBoundsPanics(t *testing.T) {
	inner := &codexTestTool{panic: strings.Repeat("panic", 4_000)}
	tool := &codexBoundedTool{tool: inner}

	response, err := tool.Run(t.Context(), fantasy.ToolCall{Name: "test"})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Contains(t, response.Content, `tool "test" panicked`)
	require.Contains(t, response.Content, "tool output truncated")
	require.LessOrEqual(t, len(response.Content), 12_000)
}

func TestToolResultContentUsesStoredCodexPolicy(t *testing.T) {
	content := strings.Repeat("tool output", 4_000)
	codexModel := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name, Model: "gpt-5.6-sol"},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	}
	presetModel := Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name, Model: "gpt-5.6-sol"},
		InstructionPolicy: fantasy.InstructionPolicyGeneric,
	}

	require.Equal(t, codexresponses.TruncateToolOutput(codexModel.ModelCfg.Model, content), toolResultContentForModel(codexModel, content))
	require.Equal(t, content, toolResultContentForModel(presetModel, content))
}

func TestCodexBoundedToolsOnlyWrapsCanonicalCodex(t *testing.T) {
	inner := &codexTestTool{}
	tools := []fantasy.AgentTool{inner}

	codexTools := codexBoundedTools(tools, Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name, Model: "gpt-5.6-sol"},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	})
	require.Len(t, codexTools, 1)
	require.IsType(t, &codexBoundedTool{}, codexTools[0])
	require.Equal(t, "gpt-5.6-sol", codexTools[0].(*codexBoundedTool).modelID)
	require.NotSame(t, tools[0], codexTools[0])
	require.Same(t, codexTools[0], codexBoundedTools(codexTools, Model{
		ModelCfg:          config.SelectedModel{Provider: codexresponses.Name},
		InstructionPolicy: fantasy.InstructionPolicyCodex,
	})[0])

	for _, model := range []Model{
		{ModelCfg: config.SelectedModel{Provider: "openai"}},
		{ModelCfg: config.SelectedModel{Provider: codexresponses.Name}, InstructionPolicy: fantasy.InstructionPolicyGeneric},
	} {
		nonCodexTools := codexBoundedTools(tools, model)
		require.Same(t, tools[0], nonCodexTools[0])
	}
}
