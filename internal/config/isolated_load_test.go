package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestIsolatedLoadAndReloadKeepEnvironmentAndProviderStateLocal(t *testing.T) {
	const environmentKey = "CRUX_T4_4_4_WORKSPACE_ENV"
	originalValue, originalExists := os.LookupEnv(environmentKey)
	require.NoError(t, os.Unsetenv(environmentKey))
	t.Cleanup(func() {
		if originalExists {
			_ = os.Setenv(environmentKey, originalValue)
		} else {
			_ = os.Unsetenv(environmentKey)
		}
	})

	resetProviderState()
	t.Cleanup(resetProviderState)
	priorRegistry, err := providerregistry.New(providerregistry.Registration{
		ProviderID:   "prior-global",
		Construction: providerregistry.ConstructionGenericJSON,
	})
	require.NoError(t, err)
	publishProviderScan(ProviderScan{
		Providers: []catalog.Provider{{ID: "prior-global", Name: "Prior Global"}},
		Registry:  priorRegistry,
	}, nil)

	root := t.TempDir()
	globalConfig := filepath.Join(root, "global-config")
	globalData := filepath.Join(root, "global-data")
	cacheDir := filepath.Join(root, "cache")
	require.NoError(t, os.MkdirAll(globalConfig, 0o755))
	require.NoError(t, os.MkdirAll(globalData, 0o755))
	baseValues := environmentValues(SnapshotEnvironment())
	baseValues["CRUX_GLOBAL_CONFIG"] = globalConfig
	baseValues["CRUX_GLOBAL_DATA"] = globalData
	baseValues["CRUX_CACHE_DIR"] = cacheDir
	baseValues["CRUX_PROVIDER_PROFILE"] = string(ProviderProfileCoreOnly)
	delete(baseValues, environmentKey)
	baseEnvironment := env.NewFromMap(baseValues)

	workspaceA := filepath.Join(root, "workspace-a")
	workspaceB := filepath.Join(root, "workspace-b")
	dataA := filepath.Join(root, "data-a")
	dataB := filepath.Join(root, "data-b")
	for _, path := range []string{workspaceA, workspaceB, dataA, dataB} {
		require.NoError(t, os.MkdirAll(path, 0o755))
	}
	configA := filepath.Join(dataA, "crux.json")
	configB := filepath.Join(dataB, "crux.json")
	writeEnvironment := func(path, value string) {
		require.NoError(t, os.WriteFile(path, []byte(`{"env":{"`+environmentKey+`":"`+value+`"}}`), 0o600))
	}
	writeEnvironment(configA, "workspace-a")
	writeEnvironment(configB, "workspace-b")

	storeA, err := LoadIsolated(workspaceA, dataA, false, baseEnvironment)
	require.NoError(t, err)
	storeB, err := LoadIsolated(workspaceB, dataB, false, baseEnvironment)
	require.NoError(t, err)
	require.Equal(t, "workspace-a", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b", storeB.RuntimeSnapshot().Getenv(environmentKey))
	_, processExists := os.LookupEnv(environmentKey)
	require.False(t, processExists)
	assertPriorGlobalProviderGeneration(t)

	writeEnvironment(configA, "workspace-a-reloaded")
	require.NoError(t, storeA.ReloadFromDisk(t.Context()))
	require.Equal(t, "workspace-a-reloaded", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b", storeB.RuntimeSnapshot().Getenv(environmentKey))
	assertPriorGlobalProviderGeneration(t)

	writeEnvironment(configB, "workspace-b-reloaded")
	require.NoError(t, storeB.ReloadFromDisk(t.Context()))
	require.Equal(t, "workspace-a-reloaded", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b-reloaded", storeB.RuntimeSnapshot().Getenv(environmentKey))
	_, processExists = os.LookupEnv(environmentKey)
	require.False(t, processExists)
	assertPriorGlobalProviderGeneration(t)

	writeEnvironment(configA, "workspace-a-concurrent")
	writeEnvironment(configB, "workspace-b-concurrent")
	start := make(chan struct{})
	var waitGroup sync.WaitGroup
	reloadErrors := make(chan error, 2)
	for _, store := range []*ConfigStore{storeA, storeB} {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			<-start
			reloadErrors <- store.ReloadFromDisk(t.Context())
		}()
	}
	close(start)
	waitGroup.Wait()
	close(reloadErrors)
	for reloadErr := range reloadErrors {
		require.NoError(t, reloadErr)
	}
	require.Equal(t, "workspace-a-concurrent", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b-concurrent", storeB.RuntimeSnapshot().Getenv(environmentKey))
	_, processExists = os.LookupEnv(environmentKey)
	require.False(t, processExists)
	assertPriorGlobalProviderGeneration(t)

	writeEnvironment(configA, "workspace-a-prepared-failure")
	preparerCalled := false
	storeA.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		preparerCalled = true
		require.Equal(t, "workspace-a-prepared-failure", snapshot.Getenv(environmentKey))
		return RuntimeGenerationCandidate{}, errors.New("synthetic runtime preparation failure")
	})
	reloadErr := storeA.ReloadFromDisk(t.Context())
	require.ErrorContains(t, reloadErr, "prepare reloaded runtime generation: synthetic runtime preparation failure")
	require.True(t, preparerCalled)
	require.Equal(t, "workspace-a-concurrent", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b-concurrent", storeB.RuntimeSnapshot().Getenv(environmentKey))
	_, processExists = os.LookupEnv(environmentKey)
	require.False(t, processExists)
	assertPriorGlobalProviderGeneration(t)
	storeA.SetRuntimeGenerationPreparer(nil)

	writeEnvironment(configA, "$")
	reloadErr = storeA.ReloadFromDisk(t.Context())
	require.ErrorContains(t, reloadErr, `resolve environment variable "CRUX_T4_4_4_WORKSPACE_ENV"`)
	require.Equal(t, "workspace-a-concurrent", storeA.RuntimeSnapshot().Getenv(environmentKey))
	require.Equal(t, "workspace-b-concurrent", storeB.RuntimeSnapshot().Getenv(environmentKey))
	_, processExists = os.LookupEnv(environmentKey)
	require.False(t, processExists)
	assertPriorGlobalProviderGeneration(t)
}

func assertPriorGlobalProviderGeneration(t *testing.T) {
	t.Helper()
	scan, found := currentProviderScan()
	require.True(t, found)
	require.Equal(t, []catalog.Provider{{ID: "prior-global", Name: "Prior Global"}}, scan.Providers)
	registration, found := scan.Registry.Lookup("prior-global")
	require.True(t, found)
	require.Equal(t, "prior-global", registration.ProviderID)
}
