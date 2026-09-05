package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestFindModelMatchesRejectsUnavailableOwners(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"available": {
			ID: "available", Models: []catalog.Model{{ID: "shared-model"}},
		},
		"unavailable": {
			ID: "unavailable", Models: []catalog.Model{{ID: "shared-model"}, {ID: "unavailable-model"}},
			Plugin: &config.ProviderPluginReference{ID: "missing.plugin", Version: "1"},
		},
	})}

	large, small := findModelMatches(cfg, "shared-model", "unavailable-model")
	require.Equal(t, []modelMatch{{provider: "available", modelID: "shared-model"}}, large)
	require.Empty(t, small)
}

func TestRunNonInteractiveStopsBeforeSessionAfterAgentUpdateFailure(t *testing.T) {
	const (
		workspaceID = "workspace-1"
		providerID  = "copilot"
	)
	registration := providerregistry.Registration{
		ProviderID:   providerID,
		Construction: providerregistry.ConstructionCopilot,
	}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			providerID: {
				ID: providerID,
				Owner: &config.ProviderOwnerReference{
					Type:         config.ProviderOwnerCore,
					Construction: registration.Construction,
				},
			},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: providerID, Model: "model"},
		},
		Options: &config.Options{},
	}
	bound := config.NewTestStoreWithRegistrations(cfg, registration).RuntimeSnapshot().Config()

	var mu sync.Mutex
	requests := make(map[string]int)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		key := request.Method + " " + request.URL.Path
		mu.Lock()
		requests[key]++
		mu.Unlock()
		switch key {
		case "GET /v1/workspaces/" + workspaceID + "/agent":
			require.NoError(t, json.NewEncoder(writer).Encode(map[string]any{"is_ready": true}))
		case "POST /v1/workspaces/" + workspaceID + "/agent/update":
			http.Error(writer, "update rejected", http.StatusConflict)
		default:
			http.Error(writer, "unexpected request", http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	api, err := client.NewClient(t.TempDir(), "tcp", serverURL.Host)
	require.NoError(t, err)

	err = runNonInteractive(t.Context(), api, &proto.Workspace{ID: workspaceID, Config: bound}, "prompt", "", "", true, "", false, proto.AgentPermissionBypass)
	require.ErrorContains(t, err, "failed to update agent")

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, map[string]int{
		"GET /v1/workspaces/" + workspaceID + "/agent":         1,
		"POST /v1/workspaces/" + workspaceID + "/agent/update": 1,
	}, requests)
}
