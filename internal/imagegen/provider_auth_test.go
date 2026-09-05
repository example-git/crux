package imagegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestProviderClientPrefersConfiguredCodexAccount(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "environment-openai")
	t.Cleanup(func() { codexBaseURLOverride = "" })

	type receivedRequest struct {
		authorization string
		model         string
		path          string
	}
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body GenerateRequest
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		received <- receivedRequest{
			authorization: request.Header.Get("Authorization"),
			model:         body.Model,
			path:          request.URL.Path,
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer server.Close()
	codexBaseURLOverride = server.URL

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"codex": {
				ID: "codex", APIKey: "configured-codex",
				Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
			},
			"openai": {APIKey: "configured-openai", BaseURL: server.URL},
		}),
	})
	response, err := NewProviderClient(store).Generate(t.Context(), GenerateRequest{Prompt: "configured account", N: 1})
	require.NoError(t, err)
	require.Equal(t, AuthCodex, response.AuthMode)
	require.Equal(t, "gpt-image-2", response.Model)

	request := <-received
	require.Equal(t, "Bearer configured-codex", request.authorization)
	require.Equal(t, "gpt-image-2", request.model)
	require.Equal(t, "/images/generations", request.path)
}

func TestProviderClientFallsBackToConfiguredOpenAIAccount(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "environment-openai")

	type receivedRequest struct {
		authorization string
		model         string
		path          string
	}
	received := make(chan receivedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body GenerateRequest
		require.NoError(t, json.NewDecoder(request.Body).Decode(&body))
		received <- receivedRequest{
			authorization: request.Header.Get("Authorization"),
			model:         body.Model,
			path:          request.URL.Path,
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[{"b64_json":"aW1hZ2U="}]}`))
	}))
	defer server.Close()

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"codex":  {},
			"openai": {APIKey: "configured-openai", BaseURL: server.URL},
		}),
	})
	response, err := NewProviderClient(store).Generate(t.Context(), GenerateRequest{Prompt: "configured fallback", N: 1})
	require.NoError(t, err)
	require.Equal(t, AuthAPIKey, response.AuthMode)
	require.Equal(t, "gpt-image-1", response.Model)

	request := <-received
	require.Equal(t, "Bearer configured-openai", request.authorization)
	require.Equal(t, "gpt-image-1", request.model)
	require.Equal(t, "/images/generations", request.path)
}

func TestConfiguredCodexAuthRejectsReplacementOwnerGeneration(t *testing.T) {
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup(codex.ID)
	require.True(t, ok)
	expired := &oauth.Token{AccessToken: "old-generation-token", RefreshToken: "old-generation-refresh", ExpiresAt: 1}
	provider := config.ProviderConfig{
		ID: codex.ID, APIKey: expired.AccessToken, OAuthToken: expired,
		Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
	}
	oldStore := config.NewTestStoreWithRegistrations(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{codex.ID: provider}),
	}, registration)
	snapshot := oldStore.RuntimeSnapshot()

	replacement := registration
	replacement.AccountNamespace = "replacement.codex.accounts"
	replacementProvider := provider
	replacementProvider.APIKey = "new-generation-token"
	replacementProvider.OAuthToken = &oauth.Token{AccessToken: "new-generation-token", RefreshToken: "new-generation-refresh"}
	currentStore := config.NewTestStoreWithRegistrations(&config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{codex.ID: replacementProvider}),
	}, replacement)

	_, configured, err := configuredCodexAuth(t.Context(), currentStore, snapshot)
	require.True(t, configured)
	require.ErrorContains(t, err, "changed")
	actual, ok := currentStore.Config().Providers.Get(codex.ID)
	require.True(t, ok)
	require.Equal(t, replacementProvider, actual)
}

func TestConfiguredCodexAccountIDRejectsSameNamespaceForwardedOwner(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup(codex.ID)
	require.True(t, ok)
	token := "shared-token"
	provider := config.ProviderConfig{
		ID: codex.ID, APIKey: token,
		Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
	}
	oldStore := config.NewTestStoreWithRegistrations(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir()},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{codex.ID: provider}),
	}, registration)
	owner := registration.Owner()
	require.NoError(t, oldStore.ApplyEphemeralProviderState(nil, map[string]config.ForwardedAccount{
		owner.AccountNamespace: {
			Owner: owner,
			Entry: accounts.Entry{ID: "stale", AccessToken: token, Raw: []byte(`{"account_id":"stale-account"}`)},
		},
	}))
	snapshot := oldStore.RuntimeSnapshot()

	replacement := registration
	replacementOAuth := *registration.OAuth
	replacementOAuth.FlowID = "replacement-flow"
	replacement.OAuth = &replacementOAuth
	replacementProvider := config.ProviderConfig{
		ID: codex.ID, APIKey: token,
		Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
	}
	currentStore := config.NewTestStoreWithRegistrations(&config.Config{
		Options:   &config.Options{DataDirectory: t.TempDir()},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{codex.ID: replacementProvider}),
	}, replacement)

	accountID, err := configuredCodexAccountID(t.Context(), currentStore, snapshot, replacement.Owner(), token)
	require.NoError(t, err)
	require.Empty(t, accountID)
}

func TestConfiguredOpenAIAuthUsesOneSnapshotDuringStoreReplacement(t *testing.T) {
	oldProvider := config.ProviderConfig{ID: "openai", APIKey: "old-key", BaseURL: "https://old.example/v1"}
	store := config.NewTestStore(&config.Config{
		Options:   &config.Options{},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"openai": oldProvider}),
	})
	snapshot := store.RuntimeSnapshot()
	replaced := make(chan error, 1)
	go func() {
		replaced <- store.ApplyEphemeralProviderState(map[string]config.ProviderConfig{
			"openai": {ID: "openai", APIKey: "new-key", BaseURL: "https://new.example/v1"},
		}, nil)
	}()

	auth, configured, err := configuredOpenAIAuth(store, snapshot)
	require.NoError(t, err)
	require.True(t, configured)
	require.Equal(t, "old-key", auth.token)
	require.Equal(t, "https://old.example/v1", auth.baseURL)
	require.NoError(t, <-replaced)
	current, ok := store.Config().Providers.Get("openai")
	require.True(t, ok)
	require.Equal(t, "new-key", current.APIKey)
	require.Equal(t, "https://new.example/v1", current.BaseURL)
}

func TestProviderClientRejectsInactiveCapturedOwnerBeforePaidRequest(t *testing.T) {
	t.Run("Codex generation fanout", func(t *testing.T) {
		registry, err := providerregistry.New(providerregistry.Integrated()...)
		require.NoError(t, err)
		registration, ok := registry.Lookup(codex.ID)
		require.True(t, ok)
		provider := config.ProviderConfig{
			ID: codex.ID, APIKey: "configured-codex",
			Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
		}
		store := config.NewTestStoreWithRegistrations(&config.Config{
			Options:   &config.Options{DataDirectory: t.TempDir()},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{codex.ID: provider}),
		}, registration)
		auth, err := resolveConfiguredAuth(t.Context(), store)
		require.NoError(t, err)
		require.Equal(t, registration.Owner(), auth.owner)
		provider.Disable = true
		require.NoError(t, store.ApplyEphemeralProviderState(map[string]config.ProviderConfig{codex.ID: provider}, nil))

		var requests atomic.Int32
		client := NewClient()
		client.authResolver = func(context.Context) (resolvedAuth, error) { return auth, nil }
		client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return jsonHTTPResponse(http.StatusOK, `{"data":[{"b64_json":"aW1hZ2U="}]}`), nil
		})}
		_, err = client.Generate(t.Context(), GenerateRequest{Prompt: "blocked generation", N: 3})
		require.ErrorContains(t, err, "active owner for provider codex changed")
		require.Zero(t, requests.Load())
	})

	t.Run("OpenAI multipart edit", func(t *testing.T) {
		providerID := string(catalog.ProviderOpenAI)
		provider := config.ProviderConfig{
			ID: providerID, APIKey: "configured-openai", BaseURL: "https://images.example/v1",
			Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
		}
		store := config.NewTestStore(&config.Config{
			Options:   &config.Options{DataDirectory: t.TempDir()},
			Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider}),
		})
		auth, err := resolveConfiguredAuth(t.Context(), store)
		require.NoError(t, err)
		require.Equal(t, providerregistry.RegistrationOwner{ProviderID: providerID}, auth.owner)
		provider.Disable = true
		require.NoError(t, store.ApplyEphemeralProviderState(map[string]config.ProviderConfig{providerID: provider}, nil))

		var requests atomic.Int32
		client := NewClient()
		client.authResolver = func(context.Context) (resolvedAuth, error) { return auth, nil }
		client.HTTPClient = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return jsonHTTPResponse(http.StatusOK, `{"data":[{"b64_json":"aW1hZ2U="}]}`), nil
		})}
		_, err = client.Edit(t.Context(), EditRequest{
			Images: []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: []byte("image")}},
			Prompt: "blocked edit",
			N:      1,
		})
		require.ErrorContains(t, err, "active owner for provider openai changed")
		require.Zero(t, requests.Load())
	})
}

func TestProviderClientRejectsEnvironmentOnlyCredentials(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "environment-openai")

	store := config.NewTestStore(&config.Config{
		Providers: csync.NewMap[string, config.ProviderConfig](),
	})
	_, err := NewProviderClient(store).Generate(t.Context(), GenerateRequest{Prompt: "no configured account", N: 1})
	require.ErrorIs(t, err, ErrNoConfiguredCredentials)
}

func TestProviderClientRejectsSameIDOwnedCredentials(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "environment-openai")

	tests := map[string]map[string]config.ProviderConfig{
		"codex preset": {
			"codex": {APIKey: "preset-codex", Preset: &config.ProviderPresetReference{ID: "preset.codex", Version: "1.0.0"}},
		},
		"codex plugin": {
			"codex": {APIKey: "plugin-codex", Plugin: &config.ProviderPluginReference{ID: "plugin.codex", Version: "1.0.0"}},
		},
		"openai preset": {
			"openai": {APIKey: "preset-openai", Preset: &config.ProviderPresetReference{ID: "preset.openai", Version: "1.0.0"}},
		},
		"openai plugin": {
			"openai": {APIKey: "plugin-openai", Plugin: &config.ProviderPluginReference{ID: "plugin.openai", Version: "1.0.0"}},
		},
	}
	for name, providers := range tests {
		t.Run(name, func(t *testing.T) {
			store := config.NewTestStore(&config.Config{Providers: csync.NewMapFrom(providers)})
			_, err := NewProviderClient(store).Generate(t.Context(), GenerateRequest{Prompt: "masked account", N: 1})
			require.ErrorIs(t, err, ErrNoConfiguredCredentials)
		})
	}
}
