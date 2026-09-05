package app

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type recordingModelCoordinator struct {
	agent.Coordinator
	mu     sync.Mutex
	states []config.AgentModelState
}

func (c *recordingModelCoordinator) UpdateModelsForState(_ context.Context, state config.AgentModelState) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.states = append(c.states, state)
	return nil
}

func (c *recordingModelCoordinator) States() []config.AgentModelState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]config.AgentModelState(nil), c.states...)
}

func newAgentRecoveryTestApp(t *testing.T) (*App, config.AgentModelState) {
	t.Helper()
	const providerID = "available"
	selected := config.SelectedModel{Provider: providerID, Model: "model"}
	cfg := &config.Config{
		Agents: map[string]config.Agent{
			config.AgentCoder: {ID: config.AgentCoder},
		},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			providerID: {
				ID: providerID, Type: catalog.TypeOpenAICompat,
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
				Models: []catalog.Model{{ID: selected.Model}},
			},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: selected,
			config.SelectedModelTypeSmall: selected,
		},
	}
	store := config.NewTestStoreWithRegistrations(cfg)
	expected := store.RuntimeSnapshot().AgentModelState()
	require.NoError(t, expected.Validate())
	application := &App{config: store}
	application.taskNotificationsOnce.Do(func() {})
	return application, expected
}

func TestUpdateAgentModelInitializesMissingCoordinator(t *testing.T) {
	application, expected := newAgentRecoveryTestApp(t)
	recorder := &recordingModelCoordinator{}
	created := 0
	application.newCoordinator = func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error) {
		created++
		return recorder, nil
	}

	require.NoError(t, application.UpdateAgentModel(t.Context(), expected))
	require.Same(t, recorder, application.CurrentAgentCoordinator())
	require.Equal(t, 1, created)
	require.Equal(t, []config.AgentModelState{expected}, recorder.States())

	require.NoError(t, application.UpdateAgentModel(t.Context(), expected))
	require.Equal(t, 1, created)
	require.Equal(t, []config.AgentModelState{expected, expected}, recorder.States())
}

func TestUpdateAgentModelInitializesMissingCoordinatorConcurrently(t *testing.T) {
	application, expected := newAgentRecoveryTestApp(t)
	recorder := &recordingModelCoordinator{}
	var created atomic.Int32
	factoryStarted := make(chan struct{})
	releaseFactory := make(chan struct{})
	application.newCoordinator = func(context.Context, agent.CoordinatorOptions) (agent.Coordinator, error) {
		if created.Add(1) == 1 {
			close(factoryStarted)
		}
		<-releaseFactory
		return recorder, nil
	}

	const calls = 16
	start := make(chan struct{})
	var ready sync.WaitGroup
	ready.Add(calls)
	errs := make(chan error, calls)
	for range calls {
		go func() {
			<-start
			ready.Done()
			ready.Wait()
			errs <- application.UpdateAgentModel(t.Context(), expected)
		}()
	}
	close(start)
	<-factoryStarted
	close(releaseFactory)
	for range calls {
		require.NoError(t, <-errs)
	}

	require.Equal(t, int32(1), created.Load())
	require.Same(t, recorder, application.CurrentAgentCoordinator())
	require.Len(t, recorder.States(), calls)
}

func TestParseModelStr(t *testing.T) {
	tests := []struct {
		name            string
		modelStr        string
		expectedFilter  string
		expectedModelID string
		setupProviders  func() map[string]config.ProviderConfig
	}{
		{
			name:            "simple model with no slashes",
			modelStr:        "gpt-4o",
			expectedFilter:  "",
			expectedModelID: "gpt-4o",
			setupProviders:  setupMockProviders,
		},
		{
			name:            "valid provider and model",
			modelStr:        "openai/gpt-4o",
			expectedFilter:  "openai",
			expectedModelID: "gpt-4o",
			setupProviders:  setupMockProviders,
		},
		{
			name:            "model with multiple slashes and first part is invalid provider",
			modelStr:        "moonshot/kimi-k2",
			expectedFilter:  "",
			expectedModelID: "moonshot/kimi-k2",
			setupProviders:  setupMockProviders,
		},
		{
			name:            "full path with valid provider and model with slashes",
			modelStr:        "synthetic/moonshot/kimi-k2",
			expectedFilter:  "synthetic",
			expectedModelID: "moonshot/kimi-k2",
			setupProviders:  setupMockProvidersWithSlashes,
		},
		{
			name:            "empty model string",
			modelStr:        "",
			expectedFilter:  "",
			expectedModelID: "",
			setupProviders:  setupMockProviders,
		},
		{
			name:            "model with trailing slash but valid provider",
			modelStr:        "openai/",
			expectedFilter:  "openai",
			expectedModelID: "",
			setupProviders:  setupMockProviders,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := tt.setupProviders()
			filter, modelID := parseModelStr(providers, tt.modelStr)

			require.Equal(t, tt.expectedFilter, filter, "provider filter mismatch")
			require.Equal(t, tt.expectedModelID, modelID, "model ID mismatch")
		})
	}
}

func setupMockProviders() map[string]config.ProviderConfig {
	return map[string]config.ProviderConfig{
		"openai": {
			ID:     "openai",
			Name:   "OpenAI",
			Models: []catalog.Model{{ID: "gpt-4o"}, {ID: "gpt-4o-mini"}},
		},
		"anthropic": {
			ID:     "anthropic",
			Name:   "Anthropic",
			Models: []catalog.Model{{ID: "claude-3-sonnet"}, {ID: "claude-3-opus"}},
		},
	}
}

func setupMockProvidersWithSlashes() map[string]config.ProviderConfig {
	return map[string]config.ProviderConfig{
		"synthetic": {
			ID:   "synthetic",
			Name: "Synthetic",
			Models: []catalog.Model{
				{ID: "moonshot/kimi-k2"},
				{ID: "deepseek/deepseek-chat"},
			},
		},
		"openai": {
			ID:     "openai",
			Name:   "OpenAI",
			Models: []catalog.Model{{ID: "gpt-4o"}},
		},
	}
}

func TestFindModels(t *testing.T) {
	tests := []struct {
		name             string
		modelStr         string
		expectedProvider string
		expectedModelID  string
		expectError      bool
		errorContains    string
		setupProviders   func() map[string]config.ProviderConfig
	}{
		{
			name:             "simple model found in one provider",
			modelStr:         "gpt-4o",
			expectedProvider: "openai",
			expectedModelID:  "gpt-4o",
			expectError:      false,
			setupProviders:   setupMockProviders,
		},
		{
			name:             "model with slashes in ID",
			modelStr:         "moonshot/kimi-k2",
			expectedProvider: "synthetic",
			expectedModelID:  "moonshot/kimi-k2",
			expectError:      false,
			setupProviders:   setupMockProvidersWithSlashes,
		},
		{
			name:             "provider and model with slashes in ID",
			modelStr:         "synthetic/moonshot/kimi-k2",
			expectedProvider: "synthetic",
			expectedModelID:  "moonshot/kimi-k2",
			expectError:      false,
			setupProviders:   setupMockProvidersWithSlashes,
		},
		{
			name:           "model not found",
			modelStr:       "nonexistent-model",
			expectError:    true,
			errorContains:  "not found",
			setupProviders: setupMockProviders,
		},
		{
			name:           "invalid provider specified",
			modelStr:       "nonexistent-provider/gpt-4o",
			expectError:    true,
			errorContains:  "provider",
			setupProviders: setupMockProviders,
		},
		{
			name:          "model found in multiple providers without provider filter",
			modelStr:      "shared-model",
			expectError:   true,
			errorContains: "multiple providers",
			setupProviders: func() map[string]config.ProviderConfig {
				return map[string]config.ProviderConfig{
					"openai": {
						ID:     "openai",
						Models: []catalog.Model{{ID: "shared-model"}},
					},
					"anthropic": {
						ID:     "anthropic",
						Models: []catalog.Model{{ID: "shared-model"}},
					},
				}
			},
		},
		{
			name:           "empty model string",
			modelStr:       "",
			expectError:    true,
			errorContains:  "not found",
			setupProviders: setupMockProviders,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			providers := tt.setupProviders()
			cfg := &config.Config{Providers: csync.NewMapFrom(providers)}

			// Use findModels with the model as "large" and empty "small".
			matches, _, err := findModels(cfg, tt.modelStr, "")
			if err != nil {
				if tt.expectError {
					require.Contains(t, err.Error(), tt.errorContains)
				} else {
					require.NoError(t, err)
				}
				return
			}

			// Validate the matches.
			match, err := validateMatches(matches, tt.modelStr, "large")

			if tt.expectError {
				require.Error(t, err)
				require.Contains(t, err.Error(), tt.errorContains)
			} else {
				require.NoError(t, err)
				require.Equal(t, tt.expectedProvider, match.provider)
				require.Equal(t, tt.expectedModelID, match.modelID)
			}
		})
	}
}

func TestFindModelsRejectsUnavailableOwners(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"available": {
			ID: "available", Models: []catalog.Model{{ID: "shared-model"}},
		},
		"unavailable": {
			ID: "unavailable", Models: []catalog.Model{{ID: "shared-model"}},
			Plugin: &config.ProviderPluginReference{ID: "missing.plugin", Version: "1"},
		},
	})}

	matches, _, err := findModels(cfg, "shared-model", "")
	require.NoError(t, err)
	require.Equal(t, []modelMatch{{provider: "available", modelID: "shared-model"}}, matches)

	_, _, err = findModels(cfg, "unavailable/shared-model", "")
	require.ErrorContains(t, err, `provider "unavailable" is not available`)
}

func TestNonInteractiveOverridesValidateAllExplicitModelsBeforeMutation(t *testing.T) {
	currentLarge := config.SelectedModel{Provider: "available", Model: "current-large"}
	currentSmall := config.SelectedModel{Provider: "available", Model: "current-small"}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"available": {
				ID: "available", Models: []catalog.Model{{ID: "current-large"}, {ID: "current-small"}, {ID: "next-large"}},
			},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: currentLarge,
			config.SelectedModelTypeSmall: currentSmall,
		},
	}
	application := &App{config: config.NewTestStoreWithRegistrations(cfg)}

	err := application.overrideModelsForNonInteractive("available/next-large", "available/missing-small")
	require.ErrorContains(t, err, "not found")
	require.Equal(t, currentLarge, application.config.Config().Models[config.SelectedModelTypeLarge])
	require.Equal(t, currentSmall, application.config.Config().Models[config.SelectedModelTypeSmall])
}

func TestNonInteractiveOverridesPrecedeCoordinatorInitialization(t *testing.T) {
	currentLarge := config.SelectedModel{Provider: "available", Model: "current-large"}
	currentSmall := config.SelectedModel{Provider: "available", Model: "current-small"}
	nextLarge := config.SelectedModel{Provider: "available", Model: "next-large"}
	nextSmall := config.SelectedModel{Provider: "available", Model: "next-small"}
	cfg := &config.Config{
		Agents: map[string]config.Agent{config.AgentCoder: {ID: config.AgentCoder}},
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"available": {
				ID: "available", Type: catalog.TypeOpenAICompat,
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
				Models: []catalog.Model{{ID: currentLarge.Model}, {ID: currentSmall.Model}, {ID: nextLarge.Model}, {ID: nextSmall.Model}},
			},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: currentLarge,
			config.SelectedModelTypeSmall: currentSmall,
		},
	}
	application := &App{config: config.NewTestStoreWithRegistrations(cfg)}
	stop := errors.New("stop after coordinator initialization")
	var observedLarge, observedSmall config.SelectedModel
	application.newCoordinator = func(_ context.Context, options agent.CoordinatorOptions) (agent.Coordinator, error) {
		models := options.Config.Config().Models
		observedLarge = models[config.SelectedModelTypeLarge]
		observedSmall = models[config.SelectedModelTypeSmall]
		return nil, stop
	}

	err := application.RunNonInteractive(t.Context(), io.Discard, "prompt", "available/next-large", "available/next-small", true, "", false, false)
	require.ErrorIs(t, err, stop)
	require.Equal(t, nextLarge, observedLarge)
	require.Equal(t, nextSmall, observedSmall)
}

func TestDefaultSmallModelStaysWithinRequestedProvider(t *testing.T) {
	alphaModels := []catalog.Model{
		{ID: "alpha-first", DefaultMaxTokens: 100},
		{ID: "alpha-small", DefaultMaxTokens: 200, DefaultReasoningEffort: "low"},
	}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"alpha": {ID: "alpha", Models: alphaModels},
			"beta":  {ID: "beta", Models: []catalog.Model{{ID: "beta-large"}}},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{
			config.SelectedModelTypeLarge: {Provider: "beta", Model: "beta-large", MaxTokens: 999},
		},
	}
	knownProviders := []catalog.Provider{{
		ID: "alpha", DefaultSmallModelID: "alpha-small", Models: alphaModels,
	}}

	selected, err := defaultSmallModel(cfg, "alpha", knownProviders)
	require.NoError(t, err)
	require.Equal(t, config.SelectedModel{
		Provider: "alpha", Model: "alpha-small", MaxTokens: 200, ReasoningEffort: "low",
	}, selected)

	selected, err = defaultSmallModel(cfg, "alpha", nil)
	require.NoError(t, err)
	require.Equal(t, "alpha", selected.Provider)
	require.Equal(t, "alpha-first", selected.Model)
}

func TestDefaultSmallModelUsesOnlySameProviderLargeSelection(t *testing.T) {
	large := config.SelectedModel{Provider: "alpha", Model: "alpha-large", MaxTokens: 777, ReasoningEffort: "high"}
	cfg := &config.Config{
		Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
			"alpha": {ID: "alpha", Models: []catalog.Model{{ID: "alpha-first"}, {ID: "alpha-large"}}},
		}),
		Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: large},
	}

	selected, err := defaultSmallModel(cfg, "alpha", nil)
	require.NoError(t, err)
	require.Equal(t, large, selected)
}

func TestDefaultSmallModelRejectsUnavailableOwner(t *testing.T) {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{
		"alpha": {
			ID: "alpha", Models: []catalog.Model{{ID: "alpha-model"}},
			Plugin: &config.ProviderPluginReference{ID: "unavailable", Version: "1"},
		},
	})}

	_, err := defaultSmallModel(cfg, "alpha", nil)
	require.ErrorContains(t, err, "provider alpha is not available")
}
