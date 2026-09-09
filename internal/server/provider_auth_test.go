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
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderAuthRejectsMalformedBeforeBackend(t *testing.T) {
	valid := `{"workspace_id":"fixture","owner":{"provider_id":"provider"},"generation":{"epoch":"` + strings.Repeat("a", 32) + `","sequence":1}}`
	for name, body := range map[string]string{
		"null": `null`, "empty": `{}`, "trailing": valid + `{}`,
		"unknown":           strings.Replace(valid, `"workspace_id":`, `"unknown":true,"workspace_id":`, 1),
		"duplicate":         strings.Replace(valid, `"workspace_id":`, `"workspace_id":"fixture","workspace_id":`, 1),
		"private namespace": strings.Replace(valid, `"provider_id":"provider"`, `"provider_id":"provider","account_namespace":"host-private"`, 1),
		"case alias":        strings.Replace(valid, `"provider_id":"provider"`, `"provider_id":"provider","Provider_ID":"other"`, 1),
		"wrong workspace":   strings.Replace(valid, `"workspace_id":"fixture"`, `"workspace_id":"other"`, 1),
		"null owner":        strings.Replace(valid, `"owner":{"provider_id":"provider"}`, `"owner":null`, 1),
		"no generation":     strings.Replace(valid, `"sequence":1`, `"sequence":0`, 1),
		"oversized":         strings.Repeat(" ", proto.MaxProviderAuthRequestBytes) + valid,
		"depth":             strings.Repeat("[", 66) + strings.Repeat("]", 66),
	} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			r.SetPathValue("id", "fixture")
			response := httptest.NewRecorder()
			(&controllerV1{}).handlePostWorkspaceProviderAccounts(response, r)
			require.Equal(t, http.StatusBadRequest, response.Code)
			require.JSONEq(t, `{"message":"invalid provider account request"}`, response.Body.String())
		})
	}
	for _, body := range []string{`{}`, " ", strings.Repeat(" ", proto.MaxProviderAuthRequestBytes+1)} {
		r := httptest.NewRequest(http.MethodGet, "/", strings.NewReader(body))
		response := httptest.NewRecorder()
		(&controllerV1{}).handleGetWorkspaceProviderAuthentication(response, r)
		require.Equal(t, http.StatusBadRequest, response.Code)
	}
}

func TestProviderAuthDetachedReceiverRejectsReadsBeforeIO(t *testing.T) {
	root := t.TempDir()
	owner := providerregistry.RegistrationOwner{ProviderID: "client-only"}
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
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
	target := providerauth.Target{WorkspaceID: harness.workspace.ID, Owner: providerauth.PublicOwner(owner), Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}}
	encoded, err := json.Marshal(target)
	require.NoError(t, err)
	for _, operation := range []string{"status", "accounts"} {
		body, method := "", http.MethodGet
		if operation == "accounts" {
			body, method = string(encoded), http.MethodPost
		}
		r := httptest.NewRequest(method, "/", strings.NewReader(body))
		r.SetPathValue("id", harness.workspace.ID)
		response := httptest.NewRecorder()
		controller := &controllerV1{backend: harness.backend}
		if operation == "status" {
			controller.handleGetWorkspaceProviderAuthentication(response, r)
		} else {
			controller.handlePostWorkspaceProviderAccounts(response, r)
		}
		require.Equal(t, http.StatusBadRequest, response.Code)
		require.Contains(t, response.Body.String(), config.ErrClientRuntimeManaged.Error())
		require.Same(t, before, store.Config())
	}
	files, err := os.ReadDir(root)
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestProviderAuthResponseSizeLimitBeforeWrite(t *testing.T) {
	response := httptest.NewRecorder()
	writeProviderAuthResponse(response, map[string]string{"large": strings.Repeat("x", proto.MaxProviderAuthResponseBytes)})
	require.Equal(t, http.StatusInternalServerError, response.Code)
	require.NotContains(t, response.Body.String(), "xxxxxxxx")
}
