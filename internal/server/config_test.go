package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestProviderCredentialRemovalEndpointEnforcesExactOwner(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataRoot, 0o700))
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfileCoreOnly))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	configPath := filepath.Join(dataRoot, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"providers": {
			"copilot": {
				"api_key": "old-access",
				"oauth": {
					"access_token": "old-access",
					"refresh_token": "old-refresh",
					"expires_at": 9999999999
				}
			}
		}
	}`), 0o600))
	store, err := config.Load(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	registration, ok := store.ProviderRegistration("copilot")
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCopilot, registration.Construction)

	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	postRemoval := func(owner providerregistry.RegistrationOwner) *http.Response {
		t.Helper()
		body, err := json.Marshal(proto.ConfigProviderKeyRequest{
			Scope:      config.ScopeGlobal,
			ProviderID: owner.ProviderID,
			Kind:       proto.APIKeyKindRemove,
			Owner:      &owner,
		})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/provider-key", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := harness.httpSrv.Client().Do(request)
		require.NoError(t, err)
		return response
	}

	mismatched := registration.Owner()
	mismatched.Construction = providerregistry.ConstructionCodex
	response := postRemoval(mismatched)
	require.NotEqual(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	provider, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "old-access", provider.APIKey)
	require.NotNil(t, provider.OAuthToken)
	data, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "old-access", gjson.GetBytes(data, "providers.copilot.api_key").String())

	response = postRemoval(registration.Owner())
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	provider, ok = store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	data, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(data, "providers.copilot.api_key").Exists())
	require.False(t, gjson.GetBytes(data, "providers.copilot.oauth").Exists())
}

func TestOAuthRefreshEndpointEnforcesExactOwner(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataRoot, 0o700))
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfileCoreOnly))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	configPath := filepath.Join(dataRoot, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"providers": {
			"copilot": {
				"api_key": "old-access",
				"oauth": {
					"access_token": "old-access",
					"refresh_token": "old-refresh",
					"expires_at": 9999999999
				}
			}
		}
	}`), 0o600))
	store, err := config.Load(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	registration, ok := store.ProviderRegistration("copilot")
	require.True(t, ok)

	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	postRefresh := func(owner providerregistry.RegistrationOwner) *http.Response {
		t.Helper()
		body, err := json.Marshal(proto.ConfigRefreshOAuthRequest{Scope: config.ScopeGlobal, Owner: owner})
		require.NoError(t, err)
		request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/refresh-oauth", bytes.NewReader(body))
		require.NoError(t, err)
		request.Header.Set("Content-Type", "application/json")
		response, err := harness.httpSrv.Client().Do(request)
		require.NoError(t, err)
		return response
	}

	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	response := postRefresh(providerregistry.RegistrationOwner{})
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NoError(t, response.Body.Close())

	mismatched := registration.Owner()
	mismatched.OAuthFlowID += "-replacement"
	response = postRefresh(mismatched)
	require.NotEqual(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())

	provider, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, "old-access", provider.APIKey)
	require.NotNil(t, provider.OAuthToken)
	require.Equal(t, "old-access", provider.OAuthToken.AccessToken)
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	_, err = os.Stat(filepath.Join(dataRoot, "locks"))
	require.ErrorIs(t, err, os.ErrNotExist)

	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok)
			if _, ok := event.Payload.(pubsub.Event[proto.ConfigChanged]); ok {
				t.Fatal("OAuth refresh rejection must not publish ConfigChanged")
			}
		case <-timer.C:
			return
		}
	}
}

func ownerMutationEndpointStore(t *testing.T) (*config.ConfigStore, providerregistry.RegistrationOwner, config.SelectedModel, string) {
	t.Helper()
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(dataRoot, 0o700))
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(config.ProviderProfileCoreOnly))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	configPath := filepath.Join(dataRoot, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"copilot":{"api_key":"test-key"}}}`), 0o600))
	store, err := config.Load(root, filepath.Join(root, "workspace-data"), false)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("copilot")
	require.True(t, ok)
	provider, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.NotEmpty(t, provider.Models)
	current := store.Config().Models[config.SelectedModelTypeLarge]
	target := config.SelectedModel{Provider: "copilot", Model: provider.Models[0].ID, Think: !current.Think}
	return store, owner, target, configPath
}

func requireNoConfigChangedEvent[T any](t *testing.T, events <-chan pubsub.Event[T]) {
	t.Helper()
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok)
			if _, changed := any(event.Payload).(pubsub.Event[proto.ConfigChanged]); changed {
				t.Fatal("rejected owner-sensitive mutation published ConfigChanged")
			}
		case <-timer.C:
			return
		}
	}
}

func requireConfigChangedEvent[T any](t *testing.T, events <-chan pubsub.Event[T], workspaceID string) {
	t.Helper()
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-events:
			require.True(t, ok)
			changed, match := any(event.Payload).(pubsub.Event[proto.ConfigChanged])
			if match {
				require.Equal(t, workspaceID, changed.Payload.WorkspaceID)
				return
			}
		case <-timer.C:
			t.Fatal("timed out waiting for ConfigChanged")
		}
	}
}

func TestPreferredModelEndpointEnforcesExactOwner(t *testing.T) {
	store, owner, target, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	postModel := func(request proto.ConfigModelRequest) *http.Response {
		t.Helper()
		body, err := json.Marshal(request)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/model", bytes.NewReader(body))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		response, err := harness.httpSrv.Client().Do(httpRequest)
		require.NoError(t, err)
		return response
	}

	beforeModel := store.Config().Models[config.SelectedModelTypeLarge]
	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	request := proto.ConfigModelRequest{Scope: config.ScopeGlobal, ModelType: config.SelectedModelTypeLarge, Model: target}
	response := postModel(request)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NoError(t, response.Body.Close())

	stale := owner
	stale.Construction = providerregistry.ConstructionCodex
	request.Owner = stale
	response = postModel(request)
	require.NotEqual(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Equal(t, beforeModel, store.Config().Models[config.SelectedModelTypeLarge])
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	requireNoConfigChangedEvent(t, events)

	request.Owner = owner
	response = postModel(request)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	require.Equal(t, target, store.Config().Models[config.SelectedModelTypeLarge])
	requireConfigChangedEvent(t, events, harness.workspace.ID)
}

func TestProviderDisabledEndpointEnforcesExactOwner(t *testing.T) {
	store, owner, _, configPath := ownerMutationEndpointStore(t)
	harness := newE2EHarness(t)
	harness.workspace.Cfg = store
	events := harness.workspace.Events(t.Context())
	postSet := func(request proto.ConfigSetRequest) *http.Response {
		t.Helper()
		body, err := json.Marshal(request)
		require.NoError(t, err)
		httpRequest, err := http.NewRequestWithContext(t.Context(), http.MethodPost, harness.httpSrv.URL+"/v1/workspaces/"+harness.workspace.ID+"/config/set", bytes.NewReader(body))
		require.NoError(t, err)
		httpRequest.Header.Set("Content-Type", "application/json")
		response, err := harness.httpSrv.Client().Do(httpRequest)
		require.NoError(t, err)
		return response
	}

	beforeDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	request := proto.ConfigSetRequest{Scope: config.ScopeGlobal, Key: "providers.copilot.disable", Value: true}
	response := postSet(request)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
	require.NoError(t, response.Body.Close())

	stale := owner
	stale.Construction = providerregistry.ConstructionCodex
	request.Owner = &stale
	response = postSet(request)
	require.NotEqual(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	provider, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.False(t, provider.Disable)
	afterDisk, err := os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, beforeDisk, afterDisk)
	requireNoConfigChangedEvent(t, events)

	request.Owner = &owner
	response = postSet(request)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.NoError(t, response.Body.Close())
	provider, ok = store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.True(t, provider.Disable)
	afterDisk, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.True(t, gjson.GetBytes(afterDisk, "providers.copilot.disable").Bool())
	requireConfigChangedEvent(t, events, harness.workspace.ID)
}
