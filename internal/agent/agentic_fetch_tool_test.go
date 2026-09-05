package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/foundation/providers/openaicompat"
	"github.com/example-git/crux/internal/agent/tools"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestAgenticFetchRetainsRuntimeSnapshotThroughSubagentExecution(t *testing.T) {
	const (
		providerID = "agentic-fetch-provider"
		modelID    = "agentic-fetch-model"
	)
	var modelRequests atomic.Int32
	modelServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		modelRequests.Add(1)
		response.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(response, "data: {\"id\":\"chatcmpl-agentic-fetch\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"agentic-fetch-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"agentic fetch completed\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-agentic-fetch\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"agentic-fetch-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(modelServer.Close)

	env := testEnv(t)
	cfg := initTestConfig(t, env.workingDir)
	cfg.Config().Options.DataDirectory = t.TempDir()
	provider := config.ProviderConfig{
		ID:      providerID,
		Type:    openaicompat.Name,
		BaseURL: modelServer.URL,
		APIKey:  "test-key",
		Owner: &config.ProviderOwnerReference{
			Type:         config.ProviderOwnerCustom,
			Construction: providerregistry.ConstructionOpenAICompat,
		},
		Models: []catalog.Model{{
			ID:               modelID,
			ContextWindow:    200_000,
			DefaultMaxTokens: 4096,
		}},
	}
	cfg.Config().Providers.Set(providerID, provider)
	selected := config.SelectedModel{Provider: providerID, Model: modelID}
	cfg.Config().Models[config.SelectedModelTypeLarge] = selected
	cfg.Config().Models[config.SelectedModelTypeSmall] = selected
	coord := &coordinator{
		cfg:         cfg,
		sessions:    env.sessions,
		messages:    env.messages,
		permissions: env.permissions,
		filetracker: *env.filetracker,
	}
	parent, err := env.sessions.Create(t.Context(), "Parent")
	require.NoError(t, err)
	tool, err := coord.agenticFetchTool(t.Context(), &http.Client{})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), tools.SessionIDContextKey, parent.ID)
	ctx = context.WithValue(ctx, tools.MessageIDContextKey, "message-1")

	result, err := tool.Run(ctx, fantasy.ToolCall{
		ID:    "agentic-fetch-call",
		Name:  tools.AgenticFetchToolName,
		Input: `{"prompt":"Return the fixture result without using tools."}`,
	})
	require.NoError(t, err)
	require.False(t, result.IsError, result.Content)
	require.Equal(t, "agentic fetch completed", result.Content)
	require.Equal(t, int32(1), modelRequests.Load())
}
