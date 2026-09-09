package agent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestClientProviderCapturedGenerationRefusesNewRequestsAfterRemoval(t *testing.T) {
	var mu sync.Mutex
	var credentials []string
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"synthetic","object":"chat.completion","model":"client-model","choices":[{"index":0,"message":{"role":"assistant","content":"verified client inference"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer host.Close()
	oldHTTP := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = oldHTTP }()
	owner := providerregistry.RegistrationOwner{ProviderID: "client-only"}
	selected := config.SelectedModel{Provider: owner.ProviderID, Model: "client-model"}
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1,
		Providers:   []config.RemoteProviderDefinition{{Config: config.ProviderConfig{ID: owner.ProviderID, Type: catalog.TypeOpenAICompat, BaseURL: host.URL + "/v1", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}, Models: []catalog.Model{{ID: selected.Model, Name: "Client model", ContextWindow: 8192, DefaultMaxTokens: 1024}}}}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected},
		Credentials: []config.RemoteCredentialBinding{{Owner: owner, Generation: 1, APIKey: "synthetic-first"}},
	}
	seal := func() {
		var err error
		proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
		require.NoError(t, err)
	}
	seal()
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	c := &coordinator{cfg: store}
	build := func() fantasy.LanguageModel {
		snapshot := store.RuntimeSnapshot()
		provider, _ := snapshot.Config().Providers.Get(owner.ProviderID)
		built, err := c.buildProvider(snapshot, provider, selected, false)
		require.NoError(t, err)
		model, err := built.LanguageModel(t.Context(), selected.Model)
		require.NoError(t, err)
		return model
	}
	first := build()
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	proposal.Credentials[0].APIKey = "synthetic-second"
	seal()
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	second := build()
	for _, model := range []fantasy.LanguageModel{first, second} {
		response, err := model.Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("Return the synthetic fixture response.")}})
		require.NoError(t, err)
		require.NotEmpty(t, response.Content)
	}
	admitted := Model{Model: first, ModelCfg: selected}
	refresh := refreshAdmittedModel(&admitted, func() Model {
		return Model{Model: first, ModelCfg: selected}
	}, func(ctx context.Context, _ *fantasy.ProviderError) error {
		target := ctx.Value(clientAuthRefreshKey{}).(*clientAuthRefreshTarget)
		target.refreshed = &Model{Model: second, ModelCfg: selected}
		return nil
	})
	require.NoError(t, refresh(t.Context(), nil))
	response, err := admitted.Model.Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("Use the captured refresh result.")}})
	require.NoError(t, err)
	require.NotEmpty(t, response.Content)
	proposal.Revision = 3
	proposal.Credentials[0].Generation = 3
	proposal.Credentials[0].APIKey = ""
	proposal.Credentials[0].Unavailable = true
	seal()
	_, err = store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 2)
	require.NoError(t, err)
	removed := build()
	for _, model := range []fantasy.LanguageModel{first, second, admitted.Model} {
		_, err = model.Generate(t.Context(), fantasy.Call{Prompt: []fantasy.Message{fantasy.NewUserMessage("New request after logout")}})
		require.ErrorContains(t, err, "has no credential", "retained model cannot start another request after removal")
		stream, streamErr := model.Stream(t.Context(), fantasy.Call{})
		if streamErr == nil {
			for event := range stream {
				if event.Error != nil {
					streamErr = event.Error
					break
				}
			}
		}
		require.ErrorContains(t, streamErr, "has no credential")
	}
	_, err = removed.Generate(t.Context(), fantasy.Call{})
	require.ErrorContains(t, err, "has no credential")
	_, err = removed.Stream(t.Context(), fantasy.Call{})
	require.ErrorContains(t, err, "has no credential")
	_, err = removed.(RemoteCompactor).Compact(t.Context(), fantasy.Call{})
	require.ErrorContains(t, err, "has no credential")
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	waitResult := make(chan error, 1)
	go func() { waitResult <- c.waitForInteractiveReauth(cancelled, owner) }()
	select {
	case err := <-waitResult:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("client authentication wait outlived its cancelled workspace context")
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"Bearer synthetic-first", "Bearer synthetic-second", "Bearer synthetic-second"}, credentials)
}
