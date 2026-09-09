package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
	openairesponsestransport "github.com/example-git/crux/internal/providertransport/openairesponses"
	"github.com/example-git/crux/internal/skills"
	"github.com/stretchr/testify/require"
)

func authenticationRevocationRuntimeConfig(baseURL, key string) *config.Config {
	return &config.Config{
		Options: &config.Options{DisableDefaultProviders: true, DisableAutoSummarize: true},
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "fixture", Model: "main", MaxTokens: 91, Temperature: new(0.25)},
			config.SelectedModelTypeSmall: {Provider: "fixture", Model: "small", MaxTokens: 37},
		},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"fixture": {
			ID: "fixture", Type: catalog.TypeOpenAICompat, BaseURL: baseURL, APIKey: key,
			Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
			Models: []catalog.Model{
				{ID: "main", Name: "Main", ContextWindow: 32000, DefaultMaxTokens: 128},
				{ID: "small", Name: "Small", ContextWindow: 16000, DefaultMaxTokens: 64},
			},
		}}),
		Agents: map[string]config.Agent{config.AgentCoder: {ID: config.AgentCoder, Model: config.SelectedModelTypeLarge}},
	}
}

// This exercises production runtime preparation, SessionAgent dispatch, and the
// HTTP owner fence. The fixture supplies the accepted store after preparation;
// the fixed logout transaction's persistence/publication is tested separately.
func TestAuthenticationRevocationRuntimeDeniesFreshAndRetainedRequests(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	var mu sync.Mutex
	var credentials []string
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		credentials = append(credentials, r.Header.Get("Authorization"))
		mu.Unlock()
		if !body.Stream {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"fixture","object":"chat.completion","model":%q,"choices":[{"message":{"role":"assistant","content":"accepted response"},"finish_reason":"stop"}]}`, body.Model)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"accepted response\"},\"finish_reason\":null}]}\n\n", body.Model)
		fmt.Fprintf(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", body.Model)
	}))
	defer host.Close()
	previousClient := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = previousClient }()
	requestCredentials := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), credentials...) }

	environment := testEnv(t)
	initial := config.NewTestStore(authenticationRevocationRuntimeConfig(host.URL+"/v1", "synthetic-old-key"))
	current := testSessionAgent(environment, nil, nil, "initial")
	backgroundContext, stopBackground := context.WithCancel(t.Context())
	stopBackground()
	coord := &coordinator{cfg: initial, currentAgent: current, sessions: environment.sessions,
		messages: environment.messages, permissions: environment.permissions, history: environment.history,
		filetracker: *environment.filetracker, skillTracker: skills.NewTracker(nil), codebaseIndexLifecycleCtx: backgroundContext}
	require.NoError(t, coord.UpdateModels(t.Context()))
	retained := current.Runtime()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("before logout")}}
	_, err := retained.LargeModel.Model.Generate(t.Context(), call)
	require.NoError(t, err)
	text, err := current.GenerateMemory(t.Context(), "memory_extraction", "before logout", 32)
	require.NoError(t, err)
	require.Equal(t, "accepted response", text)
	prior := requestCredentials()
	require.Equal(t, []string{"Bearer synthetic-old-key", "Bearer synthetic-old-key"}, prior)

	clean := authenticationRevocationRuntimeConfig(host.URL+"/v1", "")
	owner, ok := initial.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	marked, err := clean.WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	accepted := config.NewTestStore(marked)
	candidate, err := coord.prepareRuntimeGeneration(t.Context(), accepted.RuntimeSnapshot())
	require.NoError(t, err)
	defer candidate.Abort()
	require.Same(t, retained.Snapshot.Config(), current.Runtime().Snapshot.Config(), "preparation must not alter the installed runtime")
	coord.cfg = accepted
	candidate.Commit()
	require.Equal(t, retained.LargeModel.ModelCfg, current.Runtime().LargeModel.ModelCfg)
	require.Equal(t, retained.SmallModel.ModelCfg, current.Runtime().SmallModel.ModelCfg)
	require.Equal(t, retained.LargeModel.CatalogModel, current.Runtime().LargeModel.CatalogModel)
	require.Equal(t, retained.SmallModel.CatalogModel, current.Runtime().SmallModel.CatalogModel)

	_, err = retained.LargeModel.Model.Generate(t.Context(), call)
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked, "retained HTTP transport must fence a new request")
	_, err = retained.SmallModel.Model.Generate(t.Context(), call)
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
	sess, err := environment.sessions.Create(t.Context(), "logout test")
	require.NoError(t, err)
	_, err = current.Run(t.Context(), SessionAgentCall{SessionID: sess.ID, Prompt: "after logout"})
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked, "fresh foreground dispatch must be denied")
	_, err = current.GenerateMemory(t.Context(), "memory_extraction", "after logout", 32)
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked, "fresh auxiliary dispatch must be denied")
	current.GenerateTitle(t.Context(), sess.ID, "after logout")
	require.Equal(t, prior, requestCredentials(), "no title fallback or retained model may send the old credential")

	for _, credential := range []string{"key", "oauth"} {
		t.Run("restore "+credential, func(t *testing.T) {
			// A fresh immutable config with the same marker can regain credentials.
			restored, err := clean.WithAuthenticationRevocation(owner)
			require.NoError(t, err)
			provider, _ := restored.Providers.Get("fixture")
			if credential == "key" {
				provider.APIKey = "synthetic-new-key"
			} else {
				provider.OAuthToken = &oauth.Token{AccessToken: "synthetic-new-oauth"}
			}
			restored.Providers.Set("fixture", provider)
			coord.cfg = config.NewTestStore(restored)
			require.NoError(t, coord.UpdateModels(t.Context()))
			_, err = current.Runtime().LargeModel.Model.Generate(t.Context(), call)
			require.NoError(t, err)
		})
	}
	require.Equal(t, 4, len(requestCredentials()))
}

func TestAuthenticationRevocationPreservesValidationAndDisabledSelection(t *testing.T) {
	clean := authenticationRevocationRuntimeConfig("https://example.invalid/v1", "")
	owner := providerregistry.RegistrationOwner{ProviderID: "fixture"}
	marked, err := clean.WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	provider, _ := marked.Providers.Get("fixture")
	provider.Disable = true
	marked.Providers.Set("fixture", provider)
	store := config.NewTestStore(marked)
	coord := &coordinator{cfg: store}
	large, small, err := coord.buildAgentModels(context.Background(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
	require.NoError(t, err)
	require.Equal(t, clean.Models[config.SelectedModelTypeLarge], large.ModelCfg)
	require.Equal(t, clean.Models[config.SelectedModelTypeSmall], small.ModelCfg)
	_, err = large.Model.Generate(t.Context(), fantasy.Call{})
	require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
	missing := config.SelectedModel{Provider: "fixture", Model: "absent"}
	_, _, err = coord.buildAgentModels(t.Context(), config.Agent{PrimaryModelOverride: &missing}, false)
	require.ErrorContains(t, err, `model "absent"`)
	invalid := authenticationRevocationRuntimeConfig("https://example.invalid/v1", "")
	other := provider
	other.ID, other.Disable = "other", true
	invalid.Providers.Set("other", other)
	invalid.Models[config.SelectedModelTypeSmall] = config.SelectedModel{Provider: "other", Model: "small"}
	marked, err = invalid.WithAuthenticationRevocation(owner)
	require.NoError(t, err)
	coord.cfg = config.NewTestStore(marked)
	_, _, err = coord.buildAgentModels(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
	require.ErrorContains(t, err, "disabled", "an unrelated disabled provider must remain invalid")
}

func TestAuthenticationRevocationNativeResponsesAndRemoteCompaction(t *testing.T) {
	for _, mode := range []string{"local-summary", "remote-operation"} {
		t.Run(mode, func(t *testing.T) {
			registration := providerregistry.Registration{
				ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAIResponses,
				Manifest: &manifest.Manifest{ID: "fixture.responses", Version: "1.0.0"},
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: "openai-responses", Transport: "sse"},
					Endpoint: manifest.Endpoint{BaseURL: "https://example.invalid"}, Method: http.MethodPost, Path: "/v1/responses",
					Retry:        manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "before-first-event"},
					Compaction:   &manifest.CompactionPolicy{Mode: mode},
					Continuation: &manifest.ContinuationPolicy{Mode: "previous-response", ResponseIDPointer: "/id", RequestField: "previous_response_id", RequiredStableFields: []string{"model", "instructions", "tools"}, AppendOnlyHistory: true, Store: "required", Fallback: "full-replay"},
				},
			}
			if mode == "remote-operation" {
				var ok bool
				registration, ok = integratedRegistration(t, "codex")
				require.True(t, ok)
				registration.Operation = &providertransport.Operation{Compaction: &manifest.CompactionPolicy{Mode: mode, Operation: "remote-compact"}}
				registration.Operations = map[string]*providertransport.Operation{"remote-compact": {Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "before-first-event"}}}
			}
			_, err := providerregistry.New(registration)
			require.NoError(t, err)
			cfg := authenticationRevocationRuntimeConfig("https://example.invalid", "")
			provider, _ := cfg.Providers.Get("fixture")
			provider.Owner = &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: registration.Construction}
			if registration.Manifest != nil {
				provider.Owner.Type = config.ProviderOwnerPlugin
				provider.Plugin = &config.ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version}
			}
			provider.ID = registration.ProviderID
			cfg.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider})
			for kind, selected := range cfg.Models {
				selected.Provider = provider.ID
				cfg.Models[kind] = selected
			}
			initial := config.NewTestStoreWithRegistrations(cfg, registration)
			marked, err := initial.Config().WithAuthenticationRevocation(registration.Owner())
			require.NoError(t, err)
			store := config.NewTestStoreWithRegistrations(marked, registration)
			environment := testEnv(t)
			backgroundContext, stopBackground := context.WithCancel(t.Context())
			stopBackground()
			coord := &coordinator{cfg: store, currentAgent: testSessionAgent(environment, nil, nil, "initial"),
				sessions: environment.sessions, messages: environment.messages, permissions: environment.permissions,
				history: environment.history, filetracker: *environment.filetracker, skillTracker: skills.NewTracker(nil),
				responsesContinuations: openairesponsestransport.NewContinuationStore(), codebaseIndexLifecycleCtx: backgroundContext}
			candidate, err := coord.prepareRuntimeGeneration(t.Context(), store.RuntimeSnapshot())
			require.NoError(t, err, "logout must pass the real runtime preparer, including continuation and compaction gates")
			defer candidate.Abort()
			candidate.Commit()
			installed := coord.currentAgent.Runtime()
			_, err = installed.LargeModel.Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("must be denied")}, Headers: map[string]string{"x-session-id": "fixture", "x-request-purpose": "conversation"}})
			require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
			if mode == "remote-operation" {
				require.NotNil(t, installed.LargeModel.Compactor)
				_, err = installed.LargeModel.Compactor.Compact(t.Context(), fantasy.Call{})
				require.ErrorIs(t, err, config.ErrAuthenticationRevoked)
			}
			if mode == "remote-operation" {
				registration.Operation.Compaction = &manifest.CompactionPolicy{Mode: "invalid"}
				invalid, err := marked.WithAuthenticationRevocation(registration.Owner())
				require.NoError(t, err)
				coord.cfg = config.NewTestStoreWithRegistrations(invalid, registration)
				_, err = coord.prepareRuntimeGeneration(t.Context(), coord.cfg.RuntimeSnapshot())
				require.ErrorContains(t, err, `mode "invalid" is unsupported`, "logout must not bypass invalid compaction configuration")
			} else {
				invalid, err := marked.WithAuthenticationRevocation(registration.Owner())
				require.NoError(t, err)
				selected := invalid.Models[config.SelectedModelTypeLarge]
				selected.ProviderOptions = map[string]any{"max_tool_calls": "invalid"}
				invalid.Models[config.SelectedModelTypeLarge] = selected
				coord.cfg = config.NewTestStoreWithRegistrations(invalid, registration)
				_, err = coord.prepareRuntimeGeneration(t.Context(), coord.cfg.RuntimeSnapshot())
				require.ErrorContains(t, err, "max_tool_calls", "logout must not bypass invalid provider options")
			}
		})
	}
}

func TestAuthenticationRevocationPreparationWaitIsCancelable(t *testing.T) {
	coord := &coordinator{}
	coord.updateMu.Lock()
	defer coord.updateMu.Unlock()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, result := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		_, err := coord.prepareRuntimeGeneration(ctx, config.RuntimeSnapshot{})
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled runtime preparation waited for the held update mutex")
	}
}
