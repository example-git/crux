package localaddon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestCodexNativeModelSelectionCachesOnlyAfterAgentUpdate(t *testing.T) {
	const workspaceID = "workspace-1"
	candidate := config.SelectedModel{Provider: "anthropic", Model: "claude-sonnet"}
	current := config.SelectedModel{Provider: "openai", Model: "gpt-5"}
	owner := providerregistry.RegistrationOwner{
		ProviderID:   candidate.Provider,
		Construction: providerregistry.ConstructionOpenAICompat,
	}
	state := config.AgentModelState{
		Large: &config.OwnedSelectedModel{Model: candidate, Owner: owner},
	}

	var mu sync.Mutex
	modelCalls := 0
	updateCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workspaces/" + workspaceID + "/config/model":
			var got proto.ConfigModelRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
			require.Equal(t, candidate, got.Model)
			require.Equal(t, owner, got.Owner)
			mu.Lock()
			modelCalls++
			mu.Unlock()
			require.NoError(t, json.NewEncoder(writer).Encode(state))
		case "/v1/workspaces/" + workspaceID + "/agent/update":
			var got proto.AgentUpdateRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
			require.Equal(t, state, got.State)
			mu.Lock()
			updateCalls++
			call := updateCalls
			mu.Unlock()
			if call == 1 {
				http.Error(writer, "update rejected", http.StatusConflict)
				return
			}
			writer.WriteHeader(http.StatusOK)
		default:
			http.Error(writer, "unexpected request", http.StatusNotFound)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	api, err := client.NewClient(t.TempDir(), "tcp", serverURL.Host)
	require.NoError(t, err)

	workspace := &codexNativeWorkspace{client: api, value: &proto.Workspace{ID: workspaceID}}
	bridge := &codexNativeBridge{
		ctx: context.Background(),
		models: map[*codexNativeWorkspace]map[string]config.SelectedModel{
			workspace: {"anthropic/claude-sonnet": candidate},
		},
		modelOwners: map[*codexNativeWorkspace]map[string]providerregistry.RegistrationOwner{
			workspace: {candidate.Provider: owner},
		},
		selectedModel: map[*codexNativeWorkspace]config.SelectedModel{workspace: current},
	}

	_, err = bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.Error(t, err)
	require.Equal(t, current, bridge.selectedModel[workspace])

	selected, err := bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.NoError(t, err)
	require.Equal(t, candidate, selected)
	require.Equal(t, candidate, bridge.selectedModel[workspace])

	selected, err = bridge.selectModel(t.Context(), workspace, "anthropic/claude-sonnet", "")
	require.NoError(t, err)
	require.Equal(t, candidate, selected)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 2, modelCalls)
	require.Equal(t, 2, updateCalls)
}

func TestCodexAppServerRetainsThreadModels(t *testing.T) {
	type submission struct {
		session, provider, model string
	}
	submissions := make(chan submission, 8)
	workingDir := nativeAPIFixture(t, "done", func(session, provider, model string) {
		submissions <- submission{session, provider, model}
	})
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputWriter.Close()
		_ = outputReader.Close()
	})
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	read := func() map[string]any {
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		return response
	}
	send := func(method string, params map[string]any) {
		require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": method, "params": params}))
	}
	send("initialize", map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}})
	require.NotNil(t, read()["result"])
	start := func(model string) string {
		send("thread/start", map[string]any{"cwd": workingDir, "model": model})
		response := read()
		require.Nil(t, response["error"])
		id := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
		require.Equal(t, "thread/started", read()["method"])
		return id
	}
	first := start("openai/gpt-5")
	second := start("anthropic/claude-sonnet")
	turn := func(thread, model string, expected submission) {
		params := map[string]any{"threadId": thread, "input": []any{map[string]any{"type": "text", "text": "hello"}}}
		if model != "" {
			params["model"] = model
		}
		send("turn/start", params)
		for {
			response := read()
			require.Nil(t, response["error"])
			if response["method"] == "turn/completed" {
				require.Equal(t, "completed", response["params"].(map[string]any)["turn"].(map[string]any)["status"])
				break
			}
		}
		require.Equal(t, expected, <-submissions)
	}
	turn(first, "", submission{"thread-1", "openai", "gpt-5"})
	turn(second, "", submission{"thread-2", "anthropic", "claude-sonnet"})
	turn(first, "openai/gpt-4.1", submission{"thread-1", "openai", "gpt-4.1"})
	turn(second, "", submission{"thread-2", "anthropic", "claude-sonnet"})
	send("turn/start", map[string]any{"threadId": first, "model": "missing-model"})
	require.NotNil(t, read()["error"])
	turn(first, "", submission{"thread-1", "openai", "gpt-4.1"})
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}
