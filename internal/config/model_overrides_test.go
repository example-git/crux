package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func TestOverrideModelsForOwnersRejectsWholeRequest(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*ConfigStore, *AgentModelState)
	}{
		{"unavailable second model", func(_ *ConfigStore, state *AgentModelState) { state.Small.Model.Model = "missing" }},
		{"empty second model", func(_ *ConfigStore, state *AgentModelState) { state.Small.Model.Model = "" }},
		{"empty second provider", func(_ *ConfigStore, state *AgentModelState) { state.Small.Model.Provider = "" }},
		{"stale second owner", func(_ *ConfigStore, state *AgentModelState) { state.Small.Owner.ManifestVersion = "2.0.0" }},
		{"mismatched second owner", func(_ *ConfigStore, state *AgentModelState) { state.Small.Owner.ProviderID = "different" }},
		{"missing second owner", func(_ *ConfigStore, state *AgentModelState) { state.Small.Owner.ProviderID = "" }},
		{"disabled provider", func(store *ConfigStore, _ *AgentModelState) {
			provider, _ := store.Config().Providers.Get("owner-test")
			provider.Disable = true
			store.Config().Providers.Set("owner-test", provider)
		}},
		{"empty request", func(_ *ConfigStore, state *AgentModelState) { *state = AgentModelState{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := ownerTestRegistration("owner-test", "plugin.one")
			store, path := newOwnerMutationTestStore(t, registration)
			store.overrides.Models = map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: store.Config().Models[SelectedModelTypeLarge]}
			state := AgentModelState{
				Large: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
				Small: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
			}
			test.change(store, &state)
			before := store.Config()
			modelsBefore := store.RuntimeSnapshot().AgentModelState()
			explicitBefore := map[SelectedModelType]bool{
				SelectedModelTypeLarge: before.modelExplicit(SelectedModelTypeLarge),
				SelectedModelTypeSmall: before.modelExplicit(SelectedModelTypeSmall),
			}
			overridesBefore := store.Overrides()
			diskBefore := readOwnerMutationTestFile(t, path)

			result, err := store.OverrideModelsForOwners(state)
			require.Error(t, err)
			require.Equal(t, AgentModelState{}, result)
			require.Same(t, before, store.Config())
			require.Equal(t, modelsBefore, store.RuntimeSnapshot().AgentModelState())
			require.Equal(t, overridesBefore, store.Overrides())
			for modelType, explicit := range explicitBefore {
				require.Equal(t, explicit, store.Config().modelExplicit(modelType))
			}
			require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
		})
	}
}

func TestOverrideModelsForOwnersValidatesGenerationUnderLock(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store, path := newOwnerMutationTestStore(t, registration)
	diskBefore := readOwnerMutationTestFile(t, path)
	state := AgentModelState{
		Large: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
		Small: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	store.writeMu.Lock()
	go func() {
		close(started)
		_, err := store.OverrideModelsForOwners(state)
		done <- err
	}()
	<-started
	replacement := replaceOwnerMutationTestGenerationLocked(t, store, ownerTestRegistration("owner-test", "plugin.two"))
	store.writeMu.Unlock()

	require.ErrorContains(t, <-done, "active owner for provider owner-test changed")
	require.Same(t, replacement, store.Config())
	require.Empty(t, store.Overrides().Models)
	require.Equal(t, "old-model", store.Config().Models[SelectedModelTypeLarge].Model)
	require.Equal(t, "old-model", store.Config().Models[SelectedModelTypeSmall].Model)
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestOverrideModelsForOwnersRejectsCancellationWhileWaitingForLock(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store, path := newOwnerMutationTestStore(t, registration)
	before := store.Config()
	overridesBefore := store.Overrides()
	diskBefore := readOwnerMutationTestFile(t, path)
	requested := AgentModelState{
		Large: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
		Small: &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()},
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	store.writeMu.Lock()
	go func() {
		close(started)
		_, err := store.OverrideModelsForOwnersContext(ctx, requested)
		done <- err
	}()
	<-started
	cancel()
	store.writeMu.Unlock()
	require.ErrorIs(t, <-done, context.Canceled)
	require.Same(t, before, store.Config())
	require.Equal(t, overridesBefore, store.Overrides())
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestOverrideModelsForOwnersPreservesOmittedSelections(t *testing.T) {
	for _, modelType := range []SelectedModelType{SelectedModelTypeLarge, SelectedModelTypeSmall} {
		t.Run(string(modelType), func(t *testing.T) {
			registration := ownerTestRegistration("owner-test", "plugin.one")
			store, path := newOwnerMutationTestStore(t, registration)
			// An inferred small model must also remain unchanged when omitted.
			delete(store.Config().explicitModels, SelectedModelTypeSmall)
			before := store.RuntimeSnapshot().AgentModelState()
			diskBefore := readOwnerMutationTestFile(t, path)
			selected := &OwnedSelectedModel{Model: SelectedModel{Provider: "owner-test", Model: "new-model"}, Owner: registration.Owner()}
			requested := AgentModelState{}
			if modelType == SelectedModelTypeLarge {
				requested.Large = selected
			} else {
				requested.Small = selected
			}
			result, err := store.OverrideModelsForOwners(requested)
			require.NoError(t, err)
			require.Equal(t, store.RuntimeSnapshot().AgentModelState(), result)
			require.Len(t, store.Overrides().Models, 2)
			require.Equal(t, selected.Model, store.Overrides().Models[modelType])
			require.True(t, store.Config().modelExplicit(modelType))
			if modelType == SelectedModelTypeLarge {
				require.Equal(t, before.Small, result.Small)
				require.False(t, store.Config().modelExplicit(SelectedModelTypeSmall))
			} else {
				require.Equal(t, before.Large, result.Large)
			}
			require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
		})
	}
}

func TestOverrideModelsForOwnersCopiesInputsResultsAndPins(t *testing.T) {
	registration := ownerTestRegistration("owner-test", "plugin.one")
	store, path := newOwnerMutationTestStore(t, registration)
	before := store.Config()
	diskBefore := readOwnerMutationTestFile(t, path)
	temperature := 0.25
	model := SelectedModel{
		Provider: "owner-test", Model: "new-model", Temperature: &temperature,
		ProviderOptions: map[string]any{"nested": map[string]any{"enabled": true}, "items": []any{"original"}},
	}
	requested := AgentModelState{
		Large: &OwnedSelectedModel{Model: model, Owner: registration.Owner()},
		Small: &OwnedSelectedModel{Model: model, Owner: registration.Owner()},
	}
	expected := AgentModelState{
		Large: &OwnedSelectedModel{Model: cloneSelectedModel(model), Owner: registration.Owner()},
		Small: &OwnedSelectedModel{Model: cloneSelectedModel(model), Owner: registration.Owner()},
	}
	result, err := store.OverrideModelsForOwners(requested)
	require.NoError(t, err)
	require.Equal(t, expected, result)
	require.NotSame(t, before, store.Config())
	require.Equal(t, "old-model", before.Models[SelectedModelTypeLarge].Model)
	require.Equal(t, "old-model", before.Models[SelectedModelTypeSmall].Model)

	mutate := func(model SelectedModel) {
		*model.Temperature = 0.9
		model.ProviderOptions["nested"].(map[string]any)["enabled"] = false
		model.ProviderOptions["items"].([]any)[0] = "changed"
	}
	mutate(requested.Large.Model)
	mutate(result.Small.Model)
	mutate(store.Overrides().Models[SelectedModelTypeLarge])
	require.Equal(t, expected, store.RuntimeSnapshot().AgentModelState())
	require.Equal(t, expected.Large.Model, store.Overrides().Models[SelectedModelTypeLarge])
	require.Equal(t, expected.Small.Model, store.Overrides().Models[SelectedModelTypeSmall])
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestOverrideModelsForOwnersSurvivesLocalReload(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	workingDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(workingDir, 0o700))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "CRUX_GLOBAL_CONFIG": configDir,
		"CRUX_GLOBAL_DATA":      filepath.Join(root, "global-data"),
		"CRUX_CACHE_DIR":        filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(ProviderProfileCoreOnly),
	})
	fileConfig := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"fixture": {
				ID: "fixture", APIKey: "synthetic-key", BaseURL: "https://fixture.example.test/v1", Type: catalog.TypeOpenAICompat,
				Models: []catalog.Model{{ID: "old"}, {ID: "new-large"}, {ID: "new-small"}},
			},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "fixture", Model: "old"},
			SelectedModelTypeSmall: {Provider: "fixture", Model: "old"},
		},
	}
	path := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(path, mustMarshalConfig(fileConfig), 0o600))
	store, err := LoadIsolated(workingDir, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	requested := AgentModelState{
		Large: &OwnedSelectedModel{Model: SelectedModel{Provider: "fixture", Model: "new-large", MaxTokens: 123}, Owner: owner},
		Small: &OwnedSelectedModel{Model: SelectedModel{Provider: "fixture", Model: "new-small", MaxTokens: 45}, Owner: owner},
	}
	filesBefore := remoteBaselineTree(t, root)
	result, err := store.OverrideModelsForOwners(requested)
	require.NoError(t, err)
	require.Equal(t, requested, result)
	require.Equal(t, filesBefore, remoteBaselineTree(t, root))

	// A sibling's disk selection must not displace either per-run pin.
	fileConfig.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "fixture", Model: "new-small"}
	fileConfig.Models[SelectedModelTypeSmall] = SelectedModel{Provider: "fixture", Model: "new-large"}
	require.NoError(t, os.WriteFile(path, mustMarshalConfig(fileConfig), 0o600))
	diskBefore := readOwnerMutationTestFile(t, path)
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, requested, store.RuntimeSnapshot().AgentModelState())
	require.True(t, store.Config().modelExplicit(SelectedModelTypeLarge))
	require.True(t, store.Config().modelExplicit(SelectedModelTypeSmall))
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestOverrideModelsForOwnersPreservesImplicitSmallAcrossReload(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	workingDir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(configDir, 0o700))
	require.NoError(t, os.MkdirAll(workingDir, 0o700))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "CRUX_GLOBAL_CONFIG": configDir,
		"CRUX_GLOBAL_DATA":      filepath.Join(root, "global-data"),
		"CRUX_CACHE_DIR":        filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(ProviderProfileCoreOnly),
	})
	fileConfig := &Config{
		Options: &Options{DisableDefaultProviders: true},
		Providers: csync.NewMapFrom(map[string]ProviderConfig{
			"alpha": {
				ID: "alpha", APIKey: "synthetic-alpha", BaseURL: "https://alpha.example.test/v1", Type: catalog.TypeOpenAICompat,
				Models: []catalog.Model{{ID: "alpha-small"}, {ID: "alpha-large"}},
			},
			"beta": {
				ID: "beta", APIKey: "synthetic-beta", BaseURL: "https://beta.example.test/v1", Type: catalog.TypeOpenAICompat,
				Models: []catalog.Model{{ID: "beta-small"}, {ID: "beta-large"}},
			},
		}),
		Models: map[SelectedModelType]SelectedModel{
			SelectedModelTypeLarge: {Provider: "alpha", Model: "alpha-large"},
		},
	}
	path := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(path, mustMarshalConfig(fileConfig), 0o600))
	store, err := LoadIsolated(workingDir, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	require.False(t, store.Config().modelExplicit(SelectedModelTypeSmall))
	smallBefore := store.RuntimeSnapshot().AgentModelState().Small
	require.Equal(t, "alpha", smallBefore.Model.Provider)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("beta")
	require.True(t, ok)
	requested := AgentModelState{
		Large: &OwnedSelectedModel{Model: SelectedModel{Provider: "beta", Model: "beta-large"}, Owner: owner},
	}
	result, err := store.OverrideModelsForOwners(requested)
	require.NoError(t, err)
	require.Equal(t, smallBefore, result.Small)
	require.False(t, store.Config().modelExplicit(SelectedModelTypeSmall))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, result, store.RuntimeSnapshot().AgentModelState(), "reload must not recompute small from the new large provider")

	fileConfig.Models[SelectedModelTypeLarge] = SelectedModel{Provider: "alpha", Model: "alpha-small"}
	fileConfig.Models[SelectedModelTypeSmall] = SelectedModel{Provider: "beta", Model: "beta-small"}
	require.NoError(t, os.WriteFile(path, mustMarshalConfig(fileConfig), 0o600))
	diskBefore := readOwnerMutationTestFile(t, path)
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, result, store.RuntimeSnapshot().AgentModelState(), "reload must not import either sibling selection")
	require.Equal(t, diskBefore, readOwnerMutationTestFile(t, path))
}

func TestOverrideModelsForOwnersRejectsDetachedClientRuntime(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	root := t.TempDir()
	store, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	before := store.RuntimeSnapshot()
	filesBefore := remoteBaselineTree(t, root)
	overridesBefore := store.Overrides()
	requested := before.AgentModelState()
	requested.Large.Model.MaxTokens++
	result, err := store.OverrideModelsForOwners(requested)
	require.ErrorIs(t, err, ErrClientRuntimeManaged)
	require.Equal(t, AgentModelState{}, result)
	require.Same(t, before.Config(), store.Config())
	require.Equal(t, before.RemoteAuthority(), store.RemoteAuthority())
	require.Equal(t, overridesBefore, store.Overrides())
	require.Equal(t, filesBefore, remoteBaselineTree(t, root))
}
