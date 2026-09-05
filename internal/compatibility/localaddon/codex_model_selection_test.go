package localaddon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/client"
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
