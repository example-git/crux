package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHeadlessModelSelectionUsesAcknowledgedState(t *testing.T) {
	for _, mode := range []string{"default-small", "explicit-small", "small-only", "invalid-large", "invalid-small", "default-failure", "empty-default", "rejected-override", "wrong-ack", "stale-config", "refresh-failure", "explicit-beats-restore", "restore", "restore-unavailable", "empty-history", "stream-close", "prepared-implicit"} {
		t.Run(mode, func(t *testing.T) {
			registrations := []providerregistry.Registration{
				{ProviderID: "alpha", Construction: providerregistry.ConstructionOpenAICompat},
				{ProviderID: "beta", Construction: providerregistry.ConstructionOpenAICompat},
			}
			providers := csync.NewMap[string, config.ProviderConfig]()
			for _, registration := range registrations {
				providers.Set(registration.ProviderID, config.ProviderConfig{ID: registration.ProviderID, Name: registration.ProviderID, Type: catalog.TypeOpenAICompat, Models: []catalog.Model{{ID: "initial"}, {ID: "flag"}, {ID: "tiny"}, {ID: "recorded"}}, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: registration.Construction}})
			}
			store := config.NewTestStoreWithRegistrations(&config.Config{Providers: providers, Options: &config.Options{}, Models: map[config.SelectedModelType]config.SelectedModel{
				config.SelectedModelTypeLarge: {Provider: "alpha", Model: "initial"},
				config.SelectedModelTypeSmall: {Provider: "alpha", Model: "tiny"},
			}}, registrations...)
			cfg := store.Config()
			require.True(t, cfg.IsModelAvailable("beta", "flag"), "the fixture must expose its selectable custom provider")
			initial := cfg
			large, small, previous := "beta/flag", "", ""
			expectedLarge := config.SelectedModel{Provider: "beta", Model: "flag"}
			expectedSmall := config.SelectedModel{Provider: "beta", Model: "tiny"}
			wantFailure := "failed to update agent"
			wantOverride := 1
			switch mode {
			case "explicit-small":
				small = "alpha/tiny"
				expectedSmall.Provider = "alpha"
			case "small-only":
				large = ""
				small = "beta/tiny"
				expectedLarge = config.SelectedModel{Provider: "alpha", Model: "initial"}
			case "invalid-large":
				large = "missing/model"
				wantFailure = "failed to override models"
				wantOverride = 0
			case "invalid-small":
				small = "beta/missing"
				wantFailure = "failed to override models"
				wantOverride = 0
			case "default-failure", "empty-default":
				wantFailure = "resolve default small model"
				wantOverride = 0
			case "rejected-override", "wrong-ack", "stale-config", "refresh-failure":
				wantFailure = "failed to override models"
			case "explicit-beats-restore":
				previous = "existing"
			case "stream-close":
				previous = "existing"
				wantFailure = "event stream closed before the run completed"
			case "prepared-implicit":
				large = "alpha/flag"
				command := &cobra.Command{Use: "run"}
				command.SetContext(t.Context())
				command.Flags().String("model", large, "")
				command.Flags().String("small-model", "", "")
				require.NoError(t, prepareRunModelOverrides(command)(store))
				cfg = store.Config()
				initial = cfg
				expectedLarge = config.SelectedModel{Provider: "alpha", Model: "flag"}
				expectedSmall = config.SelectedModel{Provider: "alpha", Model: "initial"}
				require.Equal(t, expectedSmall, cfg.Models[config.SelectedModelTypeSmall], "preparation resolves against the original selected main model")
				wantOverride = 0
			case "restore", "restore-unavailable", "empty-history":
				large = ""
				previous = "existing"
				expectedLarge = config.SelectedModel{Provider: "beta", Model: "recorded"}
				expectedSmall.Provider = "alpha"
				if mode == "restore-unavailable" {
					wantFailure = "failed to restore model from session"
					wantOverride = 0
				}
				if mode == "empty-history" {
					expectedLarge = config.SelectedModel{Provider: "alpha", Model: "initial"}
					wantOverride = 0
				}
			}
			calls := map[string]int{}
			var mu sync.Mutex
			var built config.AgentModelState
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/clients/") {
					w.WriteHeader(http.StatusOK)
					return
				}
				mu.Lock()
				defer mu.Unlock()
				key := r.Method + " " + r.URL.Path
				calls[key]++
				w.Header().Set("Content-Type", "application/json")
				switch key {
				case "GET /v1/workspaces/fixture/agent/default-small-model":
					assert.Equal(t, "beta", r.URL.Query().Get("provider_id"))
					if mode == "default-failure" {
						http.Error(w, "fixture failure", http.StatusServiceUnavailable)
						return
					}
					if mode == "empty-default" {
						_, _ = w.Write([]byte(`null`))
						return
					}
					_ = json.NewEncoder(w).Encode(config.SelectedModel{Provider: "beta", Model: "tiny"})
				case "POST /v1/workspaces/fixture/config/model-overrides":
					if mode == "rejected-override" {
						http.Error(w, "fixture rejection", http.StatusConflict)
						return
					}
					var request proto.ModelOverridesRequest
					if !assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					models := map[config.SelectedModelType]config.SelectedModel{}
					for kind, value := range cfg.Models {
						models[kind] = value
					}
					if request.State.Large != nil {
						models[config.SelectedModelTypeLarge] = request.State.Large.Model
					}
					if request.State.Small != nil {
						models[config.SelectedModelTypeSmall] = request.State.Small.Model
					}
					cfg = config.NewTestStoreWithRegistrations(&config.Config{Providers: providers, Models: models, Options: &config.Options{}}, registrations...).Config()
					state := cfg.AgentModelState()
					if mode == "wrong-ack" {
						state.Large.Model.Model = "initial"
					}
					_ = json.NewEncoder(w).Encode(state)
				case "GET /v1/workspaces/fixture":
					if mode == "refresh-failure" {
						http.Error(w, "fixture refresh failure", http.StatusServiceUnavailable)
						return
					}
					view := cfg
					if mode == "stale-config" {
						view = initial
					}
					_ = json.NewEncoder(w).Encode(proto.Workspace{ID: "fixture", Config: view, ProviderSurfaces: config.ProviderSurfaces(view)})
				case "GET /v1/workspaces/fixture/sessions/existing":
					_ = json.NewEncoder(w).Encode(proto.Session{ID: "existing"})
				case "GET /v1/workspaces/fixture/sessions/existing/messages":
					model := "recorded"
					if mode == "restore-unavailable" {
						model = "missing"
					}
					messages := []proto.Message{{Role: proto.Assistant, Provider: "beta", Model: model}}
					if mode == "empty-history" {
						messages = nil
					}
					_ = json.NewEncoder(w).Encode(messages)
				case "POST /v1/workspaces/fixture/agent/init":
					w.WriteHeader(http.StatusOK)
				case "GET /v1/workspaces/fixture/agent":
					_ = json.NewEncoder(w).Encode(map[string]any{"is_ready": true})
				case "POST /v1/workspaces/fixture/agent/update":
					var request proto.AgentUpdateRequest
					if assert.NoError(t, json.NewDecoder(r.Body).Decode(&request)) {
						built = request.State
					}
					if mode == "stream-close" {
						w.WriteHeader(http.StatusOK)
						return
					}
					// Stop before creating a session or executing a provider. Actual
					// inference and refresh are covered by the separate TLS fixture.
					http.Error(w, "fixture stop after capturing selected state", http.StatusConflict)
				case "GET /v1/workspaces/fixture/events":
					assert.Equal(t, "stream-close", mode)
					w.Header().Set("Content-Type", "text/event-stream")
					w.WriteHeader(http.StatusOK)
				case "POST /v1/workspaces/fixture/agent":
					assert.Equal(t, "stream-close", mode)
					w.WriteHeader(http.StatusOK)
				default:
					t.Errorf("unexpected headless request %s", key)
					http.Error(w, "unexpected", http.StatusInternalServerError)
				}
			}))
			t.Cleanup(remote.Close)
			api, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(remote.URL, "http://"))
			require.NoError(t, err)
			ws := &proto.Workspace{ID: "fixture", Config: initial, ProviderSurfaces: config.ProviderSurfaces(initial)}
			retained := workspace.NewClientWorkspace(api, *ws)
			t.Cleanup(retained.Shutdown)
			err = runNonInteractiveWithWorkspace(t.Context(), api, ws, retained, "fixture prompt", large, small, true, previous, false, proto.AgentPermissionDeny, mode == "prepared-implicit")
			mu.Lock()
			defer mu.Unlock()
			require.ErrorContains(t, err, wantFailure)
			require.Equal(t, wantOverride, calls["POST /v1/workspaces/fixture/config/model-overrides"])
			if wantFailure == "failed to update agent" || mode == "stream-close" {
				require.NotNil(t, built.Large)
				require.NotNil(t, built.Small)
				require.Equal(t, expectedLarge, built.Large.Model)
				require.Equal(t, expectedSmall, built.Small.Model)
				require.Equal(t, retained.Config().AgentModelState(), built)
			} else {
				require.Zero(t, calls["POST /v1/workspaces/fixture/agent/init"], "failed selection must not initialize an agent")
			}
			if mode == "explicit-beats-restore" {
				require.Zero(t, calls["GET /v1/workspaces/fixture/sessions/existing/messages"], "explicit CLI flags must suppress restoration")
			}
			if mode == "small-only" || mode == "explicit-small" || mode == "prepared-implicit" {
				require.Zero(t, calls["GET /v1/workspaces/fixture/agent/default-small-model"])
			}
			if mode == "stream-close" {
				require.Equal(t, 1, calls["POST /v1/workspaces/fixture/agent"], "a submitted run without completion must fail visibly")
			}
		})
	}
}
