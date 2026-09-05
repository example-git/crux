package client

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func clientAgentModelState() config.AgentModelState {
	temperature := 0.4
	topP := 0.8
	topK := int64(32)
	frequencyPenalty := 0.2
	presencePenalty := 0.1
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
				Provider:         largeOwner.ProviderID,
				Model:            "large-model",
				ReasoningEffort:  "high",
				Think:            true,
				MaxTokens:        8192,
				Temperature:      &temperature,
				TopP:             &topP,
				TopK:             &topK,
				FrequencyPenalty: &frequencyPenalty,
				PresencePenalty:  &presencePenalty,
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

func TestUpdateAgentSendsCompleteModelState(t *testing.T) {
	t.Parallel()

	var got proto.AgentUpdateRequest
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, http.MethodPost, request.Method)
		require.Equal(t, "/v1/workspaces/workspace/agent/update", request.URL.Path)
		require.Equal(t, "application/json", request.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(request.Body).Decode(&got))
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	state := clientAgentModelState()
	require.NoError(t, captureClient(t, server).UpdateAgent(t.Context(), "workspace", state))
	require.Equal(t, state, got.State)
}

func TestUpdateAgentRejectsInvalidModelStateLocally(t *testing.T) {
	t.Parallel()

	called := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called <- struct{}{}
	}))
	defer server.Close()
	client := captureClient(t, server)

	err := client.UpdateAgent(t.Context(), "workspace", config.AgentModelState{})
	require.ErrorContains(t, err, "agent model state is required")

	mismatched := clientAgentModelState()
	mismatched.Large.Owner.ProviderID = "replacement"
	err = client.UpdateAgent(t.Context(), "workspace", mismatched)
	require.ErrorContains(t, err, "does not match model provider")

	select {
	case <-called:
		t.Fatal("server should not have been reached")
	default:
	}
}
