package localaddon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestCodexModelIDsRemainUniqueAndStable(t *testing.T) {
	surfaces := []providerregistry.Surface{
		{ID: "a.b", Models: []catalog.Model{{ID: "shared/model"}, {ID: "model.v1"}, {ID: "model-v1"}}},
		{ID: "a-b", Models: []catalog.Model{{ID: "shared/model"}}},
		{ID: "other", Models: []catalog.Model{{ID: "shared/model"}}},
	}
	workspace := &codexNativeWorkspace{}
	workspace.registerModelIDs(surfaces)
	require.Len(t, workspace.modelIDs, 5)
	unique := make(map[string]bool)
	for key, id := range workspace.modelIDs {
		require.NotEmpty(t, id)
		require.False(t, unique[id], "duplicate alias %s", id)
		unique[id] = true
		require.Equal(t, id, workspace.modelID(config.SelectedModel{Provider: key.provider, Model: key.model}))
	}
	require.Equal(t, "OTHER-SHARED-MODEL", workspace.modelID(config.SelectedModel{Provider: "other", Model: "shared/model"}))
	reversed := slices.Clone(surfaces)
	slices.Reverse(reversed)
	second := &codexNativeWorkspace{}
	second.registerModelIDs(reversed)
	require.Equal(t, workspace.modelIDs, second.modelIDs)
	workspace.registerModelIDs(surfaces[:1])
	require.Equal(t, second.modelIDs, workspace.modelIDs)
	workspace.registerModelIDs(append(surfaces, providerregistry.Surface{ID: "a_b", Models: []catalog.Model{{ID: "shared/model"}}}))
	require.Len(t, workspace.modelIDs, 6)
	for key, id := range second.modelIDs {
		require.Equal(t, id, workspace.modelIDs[key])
	}
	newID := workspace.modelID(config.SelectedModel{Provider: "a_b", Model: "shared/model"})
	require.False(t, unique[newID])
}

func TestCodexModelCatalogIncludesUnavailableAndRoutesExactOwners(t *testing.T) {
	const workspaceID = "workspace-1"
	firstOwner := providerregistry.RegistrationOwner{ProviderID: "first", Construction: providerregistry.ConstructionOpenAICompat}
	secondOwner := providerregistry.RegistrationOwner{ProviderID: "second", Construction: providerregistry.ConstructionOpenAICompat}
	surfaces := []providerregistry.Surface{
		{ID: "first", Owner: &firstOwner, Available: true, Models: []catalog.Model{{ID: "shared/model"}}},
		{ID: "second", Owner: &secondOwner, Available: false, Models: []catalog.Model{{ID: "shared/model"}}},
		{ID: "ownerless", Available: false, Models: []catalog.Model{{ID: "retained-model"}}},
	}
	published := make(chan config.OwnedSelectedModel, 10)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/workspaces/" + workspaceID + "/providers":
			require.NoError(t, json.NewEncoder(writer).Encode(surfaces))
		case "/v1/workspaces/" + workspaceID + "/config":
			require.NoError(t, json.NewEncoder(writer).Encode(config.Config{Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: "missing-default", Model: "missing-model"},
			}}))
		case "/v1/workspaces/" + workspaceID + "/config/model-overrides":
			var value proto.ModelOverridesRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&value))
			require.NotNil(t, value.State.Large)
			published <- *value.State.Large
			require.NoError(t, json.NewEncoder(writer).Encode(value.State))
		case "/v1/workspaces/" + workspaceID + "/agent/update":
			var value proto.AgentUpdateRequest
			require.NoError(t, json.NewDecoder(request.Body).Decode(&value))
			require.NotNil(t, value.State.Large)
			published <- *value.State.Large
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
	bridge := newCodexNativeBridgeState(t.Context(), compatibility.Invocation{}, compatibility.Request{})
	models, err := bridge.listModels(t.Context(), workspace)
	require.NoError(t, err)
	require.Len(t, models, 3)
	for _, value := range models {
		model := value.(map[string]any)
		require.Equal(t, model["id"], model["model"])
		require.Equal(t, model["id"], model["displayName"])
		require.Equal(t, false, model["isDefault"])
		require.Equal(t, false, model["hidden"])
	}
	for _, test := range []struct {
		alias string
		owner providerregistry.RegistrationOwner
	}{
		{"FIRST-SHARED-MODEL", firstOwner},
		{"SECOND-SHARED-MODEL", secondOwner},
	} {
		selected, err := bridge.selectModel(t.Context(), workspace, test.alias, "")
		require.NoError(t, err)
		expected := config.SelectedModel{Provider: test.owner.ProviderID, Model: "shared/model"}
		require.Equal(t, expected, selected)
		for range 2 {
			require.Equal(t, config.OwnedSelectedModel{Model: expected, Owner: test.owner}, <-published)
		}
	}
	_, err = bridge.selectModel(t.Context(), workspace, "SECOND-SHARED-MODEL", "first")
	require.ErrorContains(t, err, "not requested provider")
	require.Empty(t, published)
	_, err = bridge.selectModel(t.Context(), workspace, "OWNERLESS-RETAINED-MODEL", "")
	require.ErrorContains(t, err, "provider owner is unavailable for ownerless")
	for range 2 {
		require.Equal(t, config.OwnedSelectedModel{Model: config.SelectedModel{Provider: "second", Model: "shared/model"}, Owner: secondOwner}, <-published)
	}
	binding := &codexThreadBinding{workspace: workspace, model: config.SelectedModel{Provider: "first", Model: "shared-model"}}
	_, err = bridge.selectThreadModel(t.Context(), binding, "", "")
	require.ErrorContains(t, err, "thread model")
	require.Empty(t, published)
}
