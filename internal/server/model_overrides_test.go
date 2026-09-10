package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestModelOverridesRejectMalformedRequestsBeforeBackend(t *testing.T) {
	valid, err := json.Marshal(proto.ModelOverridesRequest{State: serverAgentModelState()})
	require.NoError(t, err)
	for _, body := range []string{
		`null`, `{}`, `{"state":null}`, `{"state":{}}`,
		`{"state":{"large":{"model":{"provider":"one","model":"model"},"owner":{"provider_id":"two"}}}}`,
		`{"state":{},"state":{}}`,
		`{"state":{},"secret":"synthetic-private"}`,
		strings.Replace(string(valid), `"state":`, `"unexpected":true,"state":`, 1),
		strings.Replace(string(valid), `"model":"large-model"`, `"model":"large-model","model":"replacement"`, 1),
		strings.Replace(string(valid), `"model":"large-model"`, `"model":"large-model","unknown":true`, 1),
		string(valid) + ` {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
		`{"state":"` + strings.Repeat("x", 64<<10) + `"}`,
	} {
		r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/workspaces/fixture/config/model-overrides", strings.NewReader(body))
		response := httptest.NewRecorder()
		// Dispatch against a nil backend makes a validation bypass observable.
		(&controllerV1{}).handlePostWorkspaceModelOverrides(response, r)
		require.Equal(t, http.StatusBadRequest, response.Code, body[:min(len(body), 200)])
		require.JSONEq(t, `{"message":"invalid model overrides request"}`, response.Body.String())
	}
}

func postModelOverrides(t *testing.T, harness *e2eHarness, state config.AgentModelState) (int, []byte) {
	t.Helper()
	body, err := json.Marshal(proto.ModelOverridesRequest{State: state})
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/model-overrides", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	response, err := harness.httpSrv.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func TestModelOverridesEndpointIsAtomicAndTransient(t *testing.T) {
	store, owner, target, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	before := store.RuntimeSnapshot().AgentModelState()
	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	target.MaxTokens = 2048
	target.ReasoningEffort = "high"
	target.ProviderOptions = map[string]any{"custom": map[string]any{"enabled": true}}
	requested := config.AgentModelState{Large: &config.OwnedSelectedModel{Model: target, Owner: owner}}
	stale := *requested.Large
	stale.Owner.OAuthFlowID += "-replacement"
	status, body := postModelOverrides(t, harness, config.AgentModelState{Large: &stale})
	require.Equal(t, http.StatusBadRequest, status, string(body))
	require.Contains(t, string(body), "owner")
	require.Equal(t, before, store.RuntimeSnapshot().AgentModelState())
	requireNoConfigChangedEvent(t, events)

	invalidSmall := *requested.Large
	invalidSmall.Model.Model = "missing-model"
	status, body = postModelOverrides(t, harness, config.AgentModelState{Large: requested.Large, Small: &invalidSmall})
	require.Equal(t, http.StatusBadRequest, status, string(body))
	require.Contains(t, string(body), "missing-model")
	require.Equal(t, before, store.RuntimeSnapshot().AgentModelState(), "invalid second slot must preserve the first slot")
	requireNoConfigChangedEvent(t, events)

	status, body = postModelOverrides(t, harness, requested)
	require.Equal(t, http.StatusOK, status, string(body))
	var acknowledged config.AgentModelState
	require.NoError(t, json.Unmarshal(body, &acknowledged))
	require.Equal(t, requested.Large, acknowledged.Large)
	require.Equal(t, before.Small, acknowledged.Small)
	require.Equal(t, acknowledged, store.RuntimeSnapshot().AgentModelState())
	requireConfigChangedEvent(t, events, harness.workspace.ID)
	require.NotContains(t, string(body), "test-key")
	require.NotContains(t, string(body), "api_key")
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
}

func TestModelOverridesEndpointRejectsDetachedClientRuntime(t *testing.T) {
	root := t.TempDir()
	owner := providerregistry.RegistrationOwner{ProviderID: "client-only"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers: []config.RemoteProviderDefinition{{Config: config.ProviderConfig{
			ID: owner.ProviderID, Type: catalog.TypeOpenAICompat, BaseURL: "https://client.invalid/v1",
			Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{{ID: "client-model", Name: "Client", ContextWindow: 8192, DefaultMaxTokens: 1024}},
		}}},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "client-model"},
			config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "client-model"},
		},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-client-secret"}},
	}
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	before := store.RuntimeSnapshot()
	requested := before.AgentModelState()
	requested.Large.Model.Think = true
	status, body := postModelOverrides(t, harness, requested)
	require.Equal(t, http.StatusBadRequest, status, string(body))
	require.Contains(t, string(body), config.ErrClientRuntimeManaged.Error())
	require.NotContains(t, string(body), "synthetic-client-secret")
	require.Same(t, before.Config(), store.Config())
	require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
	requireNoConfigChangedEvent(t, events)
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, files, "detached compile and rejected override must not write receiver state")
}

func TestModelOverridesRejectCanceledRequestBeforeMutation(t *testing.T) {
	store, owner, target, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	before := store.RuntimeSnapshot()
	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	body, err := json.Marshal(proto.ModelOverridesRequest{State: config.AgentModelState{Large: &config.OwnedSelectedModel{Model: target, Owner: owner}}})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/workspaces/fixture/config/model-overrides", bytes.NewReader(body))
	r.SetPathValue("id", harness.workspace.ID)
	response := httptest.NewRecorder()
	(&controllerV1{backend: harness.backend}).handlePostWorkspaceModelOverrides(response, r)
	require.Equal(t, http.StatusRequestTimeout, response.Code)
	require.Same(t, before.Config(), store.Config())
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	requireNoConfigChangedEvent(t, events)
}
