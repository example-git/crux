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

func TestProviderToolingRejectsMalformedBeforeBackend(t *testing.T) {
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		valid := `{"scope":0,"owner":{"provider_id":"fixture"}}`
		if method == http.MethodPut {
			valid = `{"scope":0,"owner":{"provider_id":"fixture"},"profile":"crux"}`
		}
		bodies := []string{
			`null`, `{}`, `{"owner":{"provider_id":"fixture"}}`,
			`{"scope":null,"owner":{"provider_id":"fixture"},"profile":"crux"}`,
			`{"scope":2,"owner":{"provider_id":"fixture"},"profile":"crux"}`,
			strings.Replace(valid, `"scope":0`, `"scope":0,"scope":1`, 1),
			strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","provider_id":"other"`, 1),
			strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","unknown":true`, 1),
			strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":""`, 1),
			strings.Replace(valid, `"scope":0`, `"scope":0,"unknown":true`, 1),
			valid + ` {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
			strings.Replace(valid, "fixture", strings.Repeat("x", 16<<10), 1),
		}
		if method == http.MethodPut {
			bodies = append(bodies, strings.Replace(valid, `"crux"`, `"unknown"`, 1), strings.Replace(valid, `,"profile":"crux"`, ``, 1))
		} else {
			bodies = append(bodies, `{"scope":0,"owner":{"provider_id":"fixture"},"profile":""}`)
		}
		for _, body := range bodies {
			r := httptest.NewRequest(method, "/v1/workspaces/fixture/config/provider-tooling", strings.NewReader(body))
			response := httptest.NewRecorder()
			// A malformed request must not dispatch through this nil backend.
			(&controllerV1{}).handleWorkspaceProviderTooling(response, r, method == http.MethodDelete)
			require.Equal(t, http.StatusBadRequest, response.Code, method+" "+body[:min(len(body), 100)])
			require.JSONEq(t, `{"message":"invalid provider tooling request"}`, response.Body.String())
		}
	}
}

func requestProviderTooling(t *testing.T, harness *e2eHarness, method string, owner providerregistry.RegistrationOwner) (int, []byte) {
	t.Helper()
	scope := config.ScopeGlobal
	var request any = proto.ProviderToolingRequest{Scope: &scope, Owner: owner, Profile: config.ToolingInstructionsCrux}
	if method == http.MethodDelete {
		request = proto.RemoveProviderToolingRequest{Scope: &scope, Owner: owner}
	}
	body, err := json.Marshal(request)
	require.NoError(t, err)
	r, err := http.NewRequestWithContext(t.Context(), method, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/provider-tooling", bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Content-Type", "application/json")
	response, err := harness.httpSrv.Client().Do(r)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, data
}

func TestProviderToolingEndpointPublishesOnlyAcceptedOwner(t *testing.T) {
	store, owner, _, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	stale := owner
	stale.OAuthFlowID += "-replacement"
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, body := requestProviderTooling(t, harness, method, stale)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Contains(t, string(body), "owner")
		requireNoConfigChangedEvent(t, events)
		after, err := os.ReadFile(configPath)
		require.NoError(t, err)
		require.Equal(t, before, after)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, body := requestProviderTooling(t, harness, method, owner)
		require.Equal(t, http.StatusOK, status, string(body))
		var state proto.ProviderToolingState
		require.NoError(t, json.Unmarshal(body, &state))
		require.Equal(t, owner, state.Owner)
		require.NoError(t, state.ValidateConfig(store.Config()))
		requireConfigChangedEvent(t, events, harness.workspace.ID)
		require.NotContains(t, string(body), "test-key")
		require.NotContains(t, string(body), "api_key")
	}
}

func TestProviderToolingEndpointRejectsDetachedReceiverBeforeIO(t *testing.T) {
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
	before := store.Config()
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		status, body := requestProviderTooling(t, harness, method, owner)
		require.Equal(t, http.StatusBadRequest, status, string(body))
		require.Contains(t, string(body), config.ErrClientRuntimeManaged.Error())
		require.Same(t, before, store.Config())
		requireNoConfigChangedEvent(t, events)
	}
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, files, "detached receiver must not touch its config/account files")
}

func TestProviderToolingCanceledRequestDoesNotMutate(t *testing.T) {
	store, owner, _, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	scope := config.ScopeGlobal
	body, err := json.Marshal(proto.ProviderToolingRequest{Scope: &scope, Owner: owner, Profile: config.ToolingInstructionsCrux})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodPut, "/v1/workspaces/fixture/config/provider-tooling", bytes.NewReader(body))
	r.SetPathValue("id", harness.workspace.ID)
	response := httptest.NewRecorder()
	(&controllerV1{backend: harness.backend}).handlePutWorkspaceProviderTooling(response, r)
	require.Equal(t, http.StatusRequestTimeout, response.Code)
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, before, after)
}
