package server

import (
	"encoding/json"
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

func TestRuntimeControlRejectsMalformedBeforeBackend(t *testing.T) {
	for _, operation := range []string{"resolve", "set", "remove"} {
		valid := `{"scope":0,"target":{"owner":{"provider_id":"fixture"},"control_id":"vendor.control","descriptor_digest":"digest","selection":{"model_type":"large","model_id":"model"}}}`
		if operation == "set" {
			valid = strings.TrimSuffix(valid, "}") + `,"value":false}`
		}
		bodies := []string{
			`null`, `{}`, valid + ` {}`, strings.Repeat("[", 66) + strings.Repeat("]", 66),
			strings.Replace(valid, `"scope":0,`, ``, 1), strings.Replace(valid, `"scope":0`, `"scope":null`, 1),
			strings.Replace(valid, `"scope":0`, `"scope":0,"scope":1`, 1),
			strings.Replace(valid, `"scope":0`, `"scope":0,"unknown":true`, 1),
			strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","provider_id":"other"`, 1),
			strings.Replace(valid, `"provider_id":"fixture"`, `"provider_id":"fixture","unknown":true`, 1),
			strings.Replace(valid, `"model_id":"model"`, `"model_id":null`, 1),
			strings.Repeat(" ", proto.MaxRuntimeControlRequestBytes) + valid,
		}
		if operation == "set" {
			for _, value := range []string{`null`, `{}`, `[]`, `"\ud800"`, `"` + strings.Repeat("x", 16<<10) + `"`} {
				bodies = append(bodies, strings.Replace(valid, `false`, value, 1))
			}
			bodies = append(bodies, strings.Replace(valid, `,"value":false`, ``, 1))
		} else {
			bodies = append(bodies, strings.TrimSuffix(valid, "}")+`,"value":false}`)
		}
		if operation != "resolve" {
			bodies = append(bodies, strings.Replace(valid, `,"descriptor_digest":"digest"`, ``, 1))
		}
		for _, body := range bodies {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(body))
			response := httptest.NewRecorder()
			(&controllerV1{}).handleWorkspaceRuntimeControl(response, r, operation != "resolve", operation == "set")
			require.Equal(t, http.StatusBadRequest, response.Code, operation+" "+body[:min(100, len(body))])
			require.JSONEq(t, `{"message":"invalid runtime control request"}`, response.Body.String())
		}
	}
}

func TestRuntimeControlDetachedReceiverRejectsResolveAndMutationsBeforeIO(t *testing.T) {
	root := t.TempDir()
	owner := providerregistry.RegistrationOwner{ProviderID: "client-only"}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: owner.ProviderID, Type: catalog.TypeOpenAICompat, BaseURL: "https://client.invalid/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: "model", Name: "Model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: owner.ProviderID, Model: "model"}, config.SelectedModelTypeSmall: {Provider: owner.ProviderID, Model: "model"}},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-client-secret"}},
	}
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	before := store.Config()
	scope := config.ScopeGlobal
	target := config.RuntimeControlTarget{Owner: owner, ControlID: "vendor.control", DescriptorDigest: "digest", Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: "model"}}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		var request any = proto.RuntimeControlRequest{Scope: &scope, Target: target}
		if method == http.MethodPut {
			request = proto.SetRuntimeControlRequest{Scope: &scope, Target: target, Value: json.RawMessage(`false`)}
		}
		body, err := json.Marshal(request)
		require.NoError(t, err)
		r := httptest.NewRequestWithContext(t.Context(), method, "/", strings.NewReader(string(body)))
		r.SetPathValue("id", harness.workspace.ID)
		response := httptest.NewRecorder()
		(&controllerV1{backend: harness.backend}).handleWorkspaceRuntimeControl(response, r, method != http.MethodPost, method == http.MethodPut)
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Contains(t, response.Body.String(), config.ErrClientRuntimeManaged.Error())
		require.Same(t, before, store.Config())
	}
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestRuntimeControlPrivateRequestPreservesExactNumbers(t *testing.T) {
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPut, "/", strings.NewReader(`{"values":{"vendor.control":9007199254740993}}`))
	var request struct {
		Values map[string]any `json:"values"`
	}
	require.NoError(t, decodeRuntimeRequest(httptest.NewRecorder(), r, &request))
	require.Equal(t, json.Number("9007199254740993"), request.Values["vendor.control"])
}
