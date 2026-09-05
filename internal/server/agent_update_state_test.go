package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type recordingAgentUpdateCoordinator struct {
	*runCoordinator
	mu     sync.Mutex
	states []config.AgentModelState
}

func (c *recordingAgentUpdateCoordinator) UpdateModelsForState(_ context.Context, state config.AgentModelState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, state)
	return nil
}

func (c *recordingAgentUpdateCoordinator) capturedStates() []config.AgentModelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]config.AgentModelState(nil), c.states...)
}

func serverAgentModelState() config.AgentModelState {
	temperature := 0.4
	largeOwner := providerregistry.RegistrationOwner{
		ProviderID:           "same",
		AccountNamespace:     "same.accounts",
		Construction:         providerregistry.ConstructionOpenAIResponses,
		CompatibilityAdapter: providerregistry.ConstructionCodex,
		HasOAuth:             true,
		OAuthAdapter:         providerregistry.LoginBrowser,
		OAuthFlowID:          "same-flow",
		HasManifest:          true,
		ManifestID:           "plugin.same",
		ManifestVersion:      "1.2.3",
	}
	smallOwner := providerregistry.RegistrationOwner{
		ProviderID:    "small",
		Construction:  providerregistry.ConstructionOpenAICompat,
		HasPreset:     true,
		PresetID:      "preset.small",
		PresetVersion: "2.0.0",
		PresetDigest:  "digest",
	}
	return config.AgentModelState{
		Large: &config.OwnedSelectedModel{
			Model: config.SelectedModel{
				Provider:    largeOwner.ProviderID,
				Model:       "large-model",
				MaxTokens:   8192,
				Temperature: &temperature,
				ProviderOptions: map[string]any{
					"nested": map[string]any{"enabled": true},
					"values": []any{"one", "two"},
				},
			},
			Owner: largeOwner,
		},
		Small: &config.OwnedSelectedModel{
			Model: config.SelectedModel{Provider: smallOwner.ProviderID, Model: "small-model"},
			Owner: smallOwner,
		},
	}
}

func postAgentUpdateState(t *testing.T, controller *controllerV1, workspaceID string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/"+workspaceID+"/agent/update", bytes.NewReader(body))
	request.SetPathValue("id", workspaceID)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	controller.handlePostWorkspaceAgentUpdate(response, request)
	return response
}

func TestAgentUpdateEndpointRejectsInvalidStateBeforeDispatch(t *testing.T) {
	coordinator := &recordingAgentUpdateCoordinator{runCoordinator: newRunCoordinator(func(context.Context) error { return nil })}
	controller, workspaceID := buildAgentWorkspace(t, coordinator)
	validState := serverAgentModelState()
	validStateJSON, err := json.Marshal(validState)
	require.NoError(t, err)
	mismatched := serverAgentModelState()
	mismatched.Large.Owner.ProviderID = "replacement"
	mismatchedJSON, err := json.Marshal(proto.AgentUpdateRequest{State: mismatched})
	require.NoError(t, err)

	for _, test := range []struct {
		name string
		body []byte
	}{
		{name: "missing body"},
		{name: "empty state", body: []byte(`{"state":{}}`)},
		{name: "owner mismatch", body: mismatchedJSON},
		{name: "unknown field", body: []byte(`{"state":` + string(validStateJSON) + `,"unknown":true}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := postAgentUpdateState(t, controller, workspaceID, test.body)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.Empty(t, coordinator.capturedStates())
		})
	}

	body, err := json.Marshal(proto.AgentUpdateRequest{State: validState})
	require.NoError(t, err)
	response := postAgentUpdateState(t, controller, workspaceID, body)
	require.Equal(t, http.StatusOK, response.Code)
	require.Equal(t, []config.AgentModelState{validState}, coordinator.capturedStates())
}
