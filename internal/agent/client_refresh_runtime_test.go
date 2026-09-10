package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestIntegratedClientExpiryRetainsAdmittedRuntime(t *testing.T) {
	for _, name := range []string{"controls", "model-selection", "other-auxiliary", "fresh-rejected"} {
		changeSelection := name == "model-selection"
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
				t.Setenv(key, t.TempDir())
			}
			t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
			// This isolates refresh capture. Client-owned Gemini project metadata is a
			// separate boundary; do not use this fixture as evidence for that lookup.
			t.Setenv("GEMINI_PROJECT_ID", "synthetic-project")
			t.Setenv("ANTIGRAVITY_CLI_VERSION", "test")
			var mu sync.Mutex
			var requests []string
			var credentials []string
			var rejectFresh atomic.Bool
			rejectFresh.Store(name == "fresh-rejected")
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				require.NoError(t, err)
				mu.Lock()
				requests = append(requests, string(body))
				credentials = append(credentials, r.Header.Get("Authorization"))
				mu.Unlock()
				if rejectFresh.Load() {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":{"message":"fresh credential rejected","code":401,"status":"UNAUTHENTICATED"}}`))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/chat/completions") {
					_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","model":"fixture-small","choices":[{"message":{"role":"assistant","content":"captured integrated response"},"finish_reason":"stop"}]}`))
					return
				}
				_, _ = w.Write([]byte(`{"response":{"candidates":[{"content":{"parts":[{"text":"captured integrated response"}]},"finishReason":"STOP"}]}}`))
			}))
			defer host.Close()
			oldClient := http.DefaultClient
			http.DefaultClient = host.Client()
			defer func() { http.DefaultClient = oldClient }()
			registry, err := providerregistry.New(registrytest.Registrations()...)
			require.NoError(t, err)
			registration, ok := registry.Lookup(gemini.ID)
			require.True(t, ok)
			selected := config.SelectedModel{Provider: gemini.ID, Model: "fixture", MaxTokens: 1024}
			small := config.SelectedModel{Provider: gemini.ID, Model: "fixture-small", MaxTokens: 128}
			proposal := config.RemoteRuntimeProposal{
				Version: config.RemoteRuntimeVersion, Revision: 1,
				Providers:   []config.RemoteProviderDefinition{{NativeIdentity: &config.NativeIdentity{UserAgent: "antigravity/cli/captured-version client-os/client-arch"}, GeminiProjectID: new("synthetic-project"), Config: config.ProviderConfig{ID: gemini.ID, Type: catalog.TypeOpenAICompat, BaseURL: host.URL, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity}, Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}, {ID: "fixture-small", Name: "Small"}, {ID: "new-model", Name: "New"}}}}},
				Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: small},
				Controls:    config.RemoteRuntimeControls{AnalysisEffort: "high", DisableAutoSummarize: true, SummarizationMaxTokens: 1024},
				Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "old-refresh", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}}},
			}
			if name == "other-auxiliary" {
				small.Provider = "other-auxiliary"
				proposal.Models[config.SelectedModelTypeSmall] = small
				proposal.Providers = append(proposal.Providers, config.RemoteProviderDefinition{Config: config.ProviderConfig{
					ID: small.Provider, Type: catalog.TypeOpenAICompat, BaseURL: host.URL + "/v1",
					Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
					Models: []catalog.Model{{ID: small.Model, Name: "Other auxiliary"}},
				}})
				proposal.Credentials = append(proposal.Credentials, config.RemoteCredentialBinding{Owner: providerregistry.RegistrationOwner{ProviderID: small.Provider}, Generation: 1, APIKey: "auxiliary-old"})
			}
			sealClientResponsesProposal(t, &proposal)
			principal := strings.Repeat("a", 64)
			root := t.TempDir()
			store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
			require.NoError(t, err)
			environment := testEnv(t)
			built, err := NewCoordinator(t.Context(), CoordinatorOptions{
				Config: store, Sessions: environment.sessions, Messages: environment.messages,
				Permissions: environment.permissions, History: environment.history,
				FileTracker: *environment.filetracker,
			})
			require.NoError(t, err)
			coord := built.(*coordinator)
			t.Cleanup(coord.Close)
			old := store.RuntimeSnapshot()
			largeModel, smallModel, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, old)
			require.NoError(t, err)
			admitted := InstalledRuntime{LargeModel: largeModel, SmallModel: smallModel, Snapshot: old, DisableAutoSummarize: true, SummarizationMaxTokens: 1024}
			refreshes := make(chan config.ClientRefreshRequest, 1)
			var refreshCount atomic.Int32
			store.SetClientRefreshPublisher(func(_ context.Context, request config.ClientRefreshRequest) {
				if refreshCount.Add(1) > 1 {
					require.NoError(t, store.CompleteClientRefresh(principal, config.ClientRefreshCompletion{RequestID: request.ID, Failed: true}))
					return
				}
				refreshes <- request
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type result struct {
				runtime InstalledRuntime
				err     error
			}
			done := make(chan result, 1)
			go func() { runtime, err := coord.refreshAdmittedRuntime(ctx, admitted); done <- result{runtime, err} }()
			var request config.ClientRefreshRequest
			select {
			case request = <-refreshes:
			case <-ctx.Done():
				t.Fatal("expiry did not request owning-client refresh")
			}
			fresh := *proposal.Credentials[0].Account
			fresh.AccessToken, fresh.RefreshToken, fresh.ExpiresAt = "synthetic-fresh", "fresh-refresh", time.Now().Add(time.Hour).UnixMilli()
			proposal.Revision, proposal.Credentials[0].Generation, proposal.Credentials[0].Account = 2, 2, &fresh
			proposal.Controls.AnalysisEffort, proposal.Controls.DisableAutoSummarize, proposal.Controls.SummarizationMaxTokens = "low", false, 2048
			next := selected
			next.MaxTokens = 2048
			if changeSelection {
				next.Model = "new-model"
				proposal.Models[config.SelectedModelTypeSmall] = next
			}
			proposal.Models[config.SelectedModelTypeLarge] = next
			if name == "other-auxiliary" {
				proposal.Credentials[1].Generation, proposal.Credentials[1].APIKey = 2, "auxiliary-new"
			}
			sealClientResponsesProposal(t, &proposal)
			_, err = store.ReplaceRemoteRuntime(ctx, proposal, principal, 1)
			require.NoError(t, err)
			require.NoError(t, store.CompleteClientRefresh(principal, config.ClientRefreshCompletion{RequestID: request.ID, Revision: 2, Digest: proposal.Digest, CredentialID: accounts.CredentialID(fresh)}))
			var refreshed InstalledRuntime
			select {
			case result := <-done:
				require.NoError(t, result.err)
				refreshed = result.runtime
			case <-ctx.Done():
				t.Fatal("refresh did not finish")
			}
			require.Equal(t, selected, refreshed.LargeModel.ModelCfg)
			require.Equal(t, small, refreshed.SmallModel.ModelCfg)
			require.True(t, refreshed.DisableAutoSummarize)
			require.EqualValues(t, 1024, refreshed.SummarizationMaxTokens)
			require.Equal(t, "high", refreshed.Snapshot.Config().Options.AnalysisEffort)
			require.Equal(t, "low", store.Config().Options.AnalysisEffort)
			if name == "fresh-rejected" {
				for _, model := range []Model{refreshed.LargeModel, refreshed.SmallModel} {
					agent := fantasy.NewAgent(model.Model)
					_, err := agent.Stream(ctx, auxiliaryStreamCall("reject fresh token", nil, model, func() Model { return model }, fantasy.Instructions{}))
					require.ErrorContains(t, err, "fresh credential rejected")
				}
				require.EqualValues(t, 1, refreshCount.Load(), "proactive rotation consumes the authentication refresh for both captured models")
				require.Empty(t, store.PendingClientRefreshes())
				later, _, err := coord.buildAgentModelsWithSnapshot(ctx, config.Agent{Model: config.SelectedModelTypeLarge}, false, store.RuntimeSnapshot())
				require.NoError(t, err)
				agent := fantasy.NewAgent(later.Model)
				_, err = agent.Stream(ctx, auxiliaryStreamCall("independent later call", nil, later, func() Model { return later }, fantasy.Instructions{}))
				require.ErrorContains(t, err, "fresh credential rejected")
				require.EqualValues(t, 2, refreshCount.Load(), "a later independent operation retains its own refresh allowance")
				mu.Lock()
				defer mu.Unlock()
				require.Equal(t, []string{"Bearer synthetic-fresh", "Bearer synthetic-fresh", "Bearer synthetic-fresh"}, credentials)
				return
			}
			later, laterSmall, err := coord.buildAgentModelsWithSnapshot(ctx, config.Agent{Model: config.SelectedModelTypeLarge}, false, store.RuntimeSnapshot())
			require.NoError(t, err)
			models := []Model{refreshed.LargeModel, refreshed.SmallModel, later}
			if name == "other-auxiliary" {
				models = append(models, laterSmall)
			}
			for _, model := range models {
				response, err := model.Model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("return fixture")}, MaxOutputTokens: &model.ModelCfg.MaxTokens, ProviderOptions: model.ProviderOptions})
				require.NoError(t, err)
				require.Contains(t, response.Content.Text(), "captured integrated response")
			}
			mu.Lock()
			defer mu.Unlock()
			expectedCredentials := []string{"Bearer synthetic-fresh", "Bearer synthetic-fresh", "Bearer synthetic-fresh"}
			expectedModels := []string{selected.Model, small.Model, next.Model}
			expectedLimits := []int64{1024, 128, 2048}
			if name == "other-auxiliary" {
				expectedCredentials[1] = "Bearer auxiliary-old"
				expectedCredentials = append(expectedCredentials, "Bearer auxiliary-new")
				expectedModels = append(expectedModels, small.Model)
				expectedLimits = append(expectedLimits, 128)
			}
			require.Equal(t, expectedCredentials, credentials)
			require.Len(t, requests, len(expectedModels))
			for index, body := range requests {
				require.Equal(t, expectedModels[index], gjson.Get(body, "model").String())
				limit := gjson.Get(body, "request.generationConfig.maxOutputTokens")
				if !limit.Exists() {
					limit = gjson.Get(body, "max_tokens")
				}
				require.Equal(t, expectedLimits[index], limit.Int())
			}
		})
	}
}
