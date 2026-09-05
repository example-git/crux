package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	pluginmanifest "github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func setupReloadPluginStore(t *testing.T) (*ConfigStore, string, providerplugin.Paths, providerplugin.Status) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	workspaceData := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	resetProviderState()
	t.Cleanup(resetProviderState)

	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
	require.NoError(t, err)
	paths := providerplugin.DefaultPaths(dataRoot, cacheRoot)
	manager, err := providerplugin.NewManager(t.Context(), paths)
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
		Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	status := snapshot.Plugins[0]
	manager.Close()

	configPath := filepath.Join(configDir, "crux.json")
	require.NoError(t, writeReloadPluginConfig(configPath, "1.0.0", false))
	store, err := Load(configDir, workspaceData, false)
	require.NoError(t, err)
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", registration.Manifest.Version)
	provider, ok := store.Config().Providers.Get("example-echo")
	require.True(t, ok)
	require.Equal(t, providerOwnerReferenceForRegistration(registration), provider.Owner)
	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, "plugin", gjson.GetBytes(data, "providers.example-echo.owner.type").String())
	require.Equal(t, string(registration.Construction), gjson.GetBytes(data, "providers.example-echo.owner.construction").String())
	require.Equal(t, string(registration.CompatibilityAdapter), gjson.GetBytes(data, "providers.example-echo.owner.compatibility_adapter").String())
	require.Equal(t, "example.echo", gjson.GetBytes(data, "providers.example-echo.plugin.id").String())
	require.Equal(t, "1.0.0", gjson.GetBytes(data, "providers.example-echo.plugin.version").String())
	return store, configPath, paths, status
}

func writeReloadPluginConfig(path, version string, debug bool) error {
	value := fmt.Sprintf(`{
		"options":{"debug":%t},
		"env":{"CRUX_RELOAD_TRANSACTION_VALUE":"candidate"},
		"providers":{"example-echo":{
			"api_key":"test-key",
			"plugin":{"id":"example.echo","version":%q},
			"models":[{"id":"echo-1","name":"Echo 1"}]
		}},
		"models":{
			"large":{"provider":"example-echo","model":"echo-1","max_tokens":8192,"reasoning_effort":"high","think":true,"temperature":0.21,"top_p":0.81,"top_k":17,"frequency_penalty":0.31,"presence_penalty":0.41,"provider_options":{"mode":"large","nested":{"enabled":true}}},
			"small":{"provider":"example-echo","model":"echo-1","max_tokens":4096,"reasoning_effort":"medium","think":true,"temperature":0.22,"top_p":0.82,"top_k":18,"frequency_penalty":0.32,"presence_penalty":0.42,"provider_options":{"mode":"small","nested":{"enabled":false}}}
		}
	}`, debug, version)
	return os.WriteFile(path, []byte(value), 0o600)
}

func replaceReloadPluginVersion(t *testing.T, paths providerplugin.Paths, installed providerplugin.Status, version string) providerplugin.Status {
	t.Helper()
	require.NoError(t, os.RemoveAll(filepath.Join(paths.Bundles, installed.BundleName)))
	sourceManifest, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	require.NoError(t, err)
	value, err := os.ReadFile(sourceManifest)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "version", version)
	require.NoError(t, err)
	source := filepath.Join(t.TempDir(), "replacement.plugin")
	require.NoError(t, os.MkdirAll(source, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), value, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), paths)
	require.NoError(t, err)
	defer manager.Close()
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
		Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	return snapshot.Plugins[0]
}

func setupReloadPresetStore(t *testing.T) (*ConfigStore, string, providerplugin.Paths, providerplugin.Status) {
	t.Helper()
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	resetProviderState()
	t.Cleanup(resetProviderState)

	source, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin"))
	require.NoError(t, err)
	paths := providerplugin.DefaultPaths(dataRoot, cacheRoot)
	manager, err := providerplugin.NewManager(t.Context(), paths)
	require.NoError(t, err)
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
		Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision,
	})
	require.NoError(t, err)
	require.Len(t, snapshot.Plugins, 1)
	status := snapshot.Plugins[0]
	manager.Close()

	configPath := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"providers":{"deepseek":{"api_key":"test-key","preset":{"id":"crux.catwalk.deepseek"}}},
		"models":{
			"large":{"provider":"deepseek","model":"deepseek-v4-pro","max_tokens":12288,"reasoning_effort":"high","think":true,"temperature":0.31,"top_p":0.71,"top_k":27,"frequency_penalty":0.41,"presence_penalty":0.51,"provider_options":{"mode":"large-preset","nested":{"enabled":true}}},
			"small":{"provider":"deepseek","model":"deepseek-v4-flash","max_tokens":6144,"reasoning_effort":"medium","think":true,"temperature":0.32,"top_p":0.72,"top_k":28,"frequency_penalty":0.42,"presence_penalty":0.52,"provider_options":{"mode":"small-preset","nested":{"enabled":false}}}
		}
	}`), 0o600))
	store, err := Load(configDir, filepath.Join(root, "workspace"), false)
	require.NoError(t, err)
	return store, configPath, paths, status
}

type reloadRuntimeCandidateState struct {
	committed bool
	aborted   bool
}

func setReloadBoundaryMutation(store *ConfigStore, mutate func()) *reloadRuntimeCandidateState {
	state := &reloadRuntimeCandidateState{}
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		mutate()
		return RuntimeGenerationCandidate{
			Commit: func() { state.committed = true },
			Abort: func() {
				if !state.committed {
					state.aborted = true
				}
			},
		}, nil
	})
	return state
}

func requireReloadRevalidationFailure(t *testing.T, store *ConfigStore, previous *Config, state *reloadRuntimeCandidateState, err error, message string) {
	t.Helper()
	require.ErrorContains(t, err, "revalidate reloaded configuration generation")
	require.ErrorContains(t, err, message)
	require.Same(t, previous, store.Config())
	require.True(t, state.aborted)
	require.False(t, state.committed)
}

func TestStartupFailureDoesNotPublishProviderGeneration(t *testing.T) {
	for _, test := range []struct {
		name       string
		config     string
		prepareEnv func(*testing.T)
		errorText  string
	}{
		{
			name:       "scan failure",
			config:     `{}`,
			prepareEnv: func(t *testing.T) { t.Setenv("CRUX_PROVIDER_PROFILE", "invalid-profile") },
			errorText:  "failed to scan providers",
		},
		{
			name:       "selected model failure",
			config:     `{"options":{"disable_default_providers":true},"providers":{"available":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"key","models":[{"id":"model","name":"Model"}]}},"models":{"large":{"provider":"missing","model":"missing"},"small":{"provider":"available","model":"model"}}}`,
			prepareEnv: func(*testing.T) {},
			errorText:  "failed to configure selected models",
		},
		{
			name:       "conflicting provider ID",
			config:     `{"options":{"disable_default_providers":true},"providers":{"configured":{"id":"other","disable":true}}}`,
			prepareEnv: func(*testing.T) {},
			errorText:  `declares conflicting ID "other"`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configDir := filepath.Join(root, "config")
			require.NoError(t, os.MkdirAll(configDir, 0o755))
			t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
			t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
			t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
			t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
			resetProviderState()
			t.Cleanup(resetProviderState)
			priorRegistry, err := providerregistry.New(providerregistry.Registration{ProviderID: "prior"})
			require.NoError(t, err)
			prior := ProviderScan{Providers: []catalog.Provider{{ID: "prior", Name: "Prior"}}, Registry: priorRegistry}
			providerOnce.Do(func() {})
			publishProviderScan(prior, nil)
			require.NoError(t, os.WriteFile(filepath.Join(configDir, "crux.json"), []byte(test.config), 0o600))
			test.prepareEnv(t)

			_, err = Load(root, filepath.Join(root, "workspace"), false)
			require.ErrorContains(t, err, test.errorText)
			published, publishedErr := Providers(nil)
			require.NoError(t, publishedErr)
			require.Equal(t, prior.Providers, published)
			registration, found := ProviderRegistry().Lookup("prior")
			require.True(t, found)
			require.Equal(t, "prior", registration.ProviderID)
		})
	}
}

func TestStartupCorrectionFailureRestoresFilesAndPublishedGeneration(t *testing.T) {
	store, configPath, _, _ := setupReloadPluginStore(t)
	dataPath := GlobalConfigData()
	dataOriginal := []byte(`{"sentinel":"original"}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
	require.NoError(t, os.WriteFile(dataPath, dataOriginal, 0o600))
	candidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	candidate, err = sjson.DeleteBytes(candidate, "providers.example-echo.plugin")
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "models.large.model", "missing-model")
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "models.small.model", "missing-model")
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "options.disable_notifications", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
	priorRegistry, err := providerregistry.New(providerregistry.Registration{ProviderID: "prior"})
	require.NoError(t, err)
	prior := ProviderScan{Providers: []catalog.Provider{{ID: "prior", Name: "Prior"}}, Registry: priorRegistry}
	providerOnce.Do(func() {})
	publishProviderScan(prior, nil)
	originalWriter := writeStartupConfigFile
	writeStartupConfigFile = func(path string, data []byte, perm os.FileMode) error {
		if path == dataPath {
			return fmt.Errorf("correction blocked")
		}
		return originalWriter(path, data, perm)
	}
	t.Cleanup(func() { writeStartupConfigFile = originalWriter })

	_, err = Load(filepath.Dir(configPath), store.Config().Options.DataDirectory, false)
	require.ErrorContains(t, err, "commit startup config corrections")
	sourceAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, candidate, sourceAfter)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataOriginal, dataAfter)
	published, publishedErr := Providers(nil)
	require.NoError(t, publishedErr)
	require.Equal(t, prior.Providers, published)
	registration, found := ProviderRegistry().Lookup("prior")
	require.True(t, found)
	require.Equal(t, "prior", registration.ProviderID)
}

func TestStartupOwnershipInspectionFailureRestoresFilesAndPublishedGeneration(t *testing.T) {
	store, configPath, _, _ := setupReloadPluginStore(t)
	dataPath := GlobalConfigData()
	dataOriginal := []byte(`{"sentinel":"original"}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(dataPath), 0o755))
	require.NoError(t, os.WriteFile(dataPath, dataOriginal, 0o600))
	candidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	candidate, err = sjson.DeleteBytes(candidate, "providers.example-echo.plugin")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
	priorRegistry, err := providerregistry.New(providerregistry.Registration{ProviderID: "prior"})
	require.NoError(t, err)
	prior := ProviderScan{Providers: []catalog.Provider{{ID: "prior", Name: "Prior"}}, Registry: priorRegistry}
	providerOnce.Do(func() {})
	publishProviderScan(prior, nil)
	originalInspector := inspectStartupConfigPreimageChanged
	inspected := false
	inspectStartupConfigPreimageChanged = func(preimages []configPreimage, path string) (bool, error) {
		inspected = true
		return false, fmt.Errorf("inspection blocked")
	}
	t.Cleanup(func() { inspectStartupConfigPreimageChanged = originalInspector })

	_, err = Load(filepath.Dir(configPath), store.Config().Options.DataDirectory, false)
	require.ErrorContains(t, err, "inspect provider ownership migration")
	require.True(t, inspected)
	sourceAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, candidate, sourceAfter)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataOriginal, dataAfter)
	published, publishedErr := Providers(nil)
	require.NoError(t, publishedErr)
	require.Equal(t, prior.Providers, published)
	registration, found := ProviderRegistry().Lookup("prior")
	require.True(t, found)
	require.Equal(t, "prior", registration.ProviderID)
}

func TestReloadFreshScanObservesBundleLifecycle(t *testing.T) {
	t.Run("removal", func(t *testing.T) {
		store, _, paths, status := setupReloadPluginStore(t)
		largeBefore := store.Config().Models[SelectedModelTypeLarge]
		smallBefore := store.Config().Models[SelectedModelTypeSmall]
		require.NoError(t, os.RemoveAll(filepath.Join(paths.Bundles, status.BundleName)))

		require.NoError(t, store.ReloadFromDisk(t.Context()))
		require.Equal(t, largeBefore, store.Config().Models[SelectedModelTypeLarge])
		require.Equal(t, smallBefore, store.Config().Models[SelectedModelTypeSmall])
		_, ok := store.ProviderRegistration("example-echo")
		require.False(t, ok)
		providers, err := Providers(store.Config())
		require.NoError(t, err)
		require.NotContains(t, providerIDs(providers), catalog.ProviderID("example-echo"))
		provider, ok := store.Config().Providers.Get("example-echo")
		require.True(t, ok)
		require.Equal(t, "1.0.0", provider.Plugin.Version)
	})

	t.Run("trust revocation", func(t *testing.T) {
		store, _, paths, status := setupReloadPluginStore(t)
		largeBefore := store.Config().Models[SelectedModelTypeLarge]
		smallBefore := store.Config().Models[SelectedModelTypeSmall]
		manager, err := providerplugin.NewManager(t.Context(), paths)
		require.NoError(t, err)
		snapshot := manager.Snapshot()
		_, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{
			Digest: status.Digest, Trusted: false, ExpectedRevision: snapshot.Revision,
		})
		require.NoError(t, err)
		manager.Close()

		require.NoError(t, store.ReloadFromDisk(t.Context()))
		require.Equal(t, largeBefore, store.Config().Models[SelectedModelTypeLarge])
		require.Equal(t, smallBefore, store.Config().Models[SelectedModelTypeSmall])
		_, ok := store.ProviderRegistration("example-echo")
		require.False(t, ok)
		published, _, found := ProviderPluginAvailability(status.ID)
		require.True(t, found)
		require.Equal(t, providerplugin.StateQuarantined, published.State)
		require.Equal(t, providerplugin.TrustRevoked, published.Trust)
	})

	t.Run("version replacement", func(t *testing.T) {
		store, _, paths, status := setupReloadPluginStore(t)
		largeBefore := store.Config().Models[SelectedModelTypeLarge]
		smallBefore := store.Config().Models[SelectedModelTypeSmall]
		replacement := replaceReloadPluginVersion(t, paths, status, "2.0.0")

		require.NoError(t, store.ReloadFromDisk(t.Context()))
		require.Equal(t, largeBefore, store.Config().Models[SelectedModelTypeLarge])
		require.Equal(t, smallBefore, store.Config().Models[SelectedModelTypeSmall])
		_, ok := store.ProviderRegistration("example-echo")
		require.False(t, ok)
		registration, ok := ProviderRegistry().Lookup("example-echo")
		require.True(t, ok)
		require.Equal(t, "2.0.0", registration.Manifest.Version)
		published, _, found := ProviderPluginAvailability(replacement.ID)
		require.True(t, found)
		require.Equal(t, "2.0.0", published.Version)
	})
}

func TestConstructionGateUsesOneExactOwnerGenerationDuringReload(t *testing.T) {
	store, configPath, paths, current := setupReloadPluginStore(t)
	dataPath := GlobalConfigData()
	stop := make(chan struct{})
	done := make(chan struct{})
	errors := make(chan error, 1)
	var attempts atomic.Int64
	report := func(err error) {
		select {
		case errors <- err:
		default:
		}
	}
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			snapshot := store.RuntimeSnapshot()
			cfg := snapshot.Config()
			provider, ok := cfg.Providers.Get("example-echo")
			if !ok {
				report(fmt.Errorf("provider missing from captured generation"))
				return
			}
			resolved, registration, registered, err := snapshot.ProviderForConstruction("example-echo", provider)
			if err != nil {
				report(err)
				return
			}
			if !registered || resolved.Owner == nil || resolved.Plugin == nil || registration.Manifest == nil {
				report(fmt.Errorf("captured generation has incomplete construction ownership"))
				return
			}
			if resolved.Plugin.ID != registration.Manifest.ID || resolved.Plugin.Version != registration.Manifest.Version ||
				resolved.Owner.Construction != registration.Construction || resolved.Owner.CompatibilityAdapter != registration.CompatibilityAdapter {
				report(fmt.Errorf("captured provider and registration owners differ"))
				return
			}
			attempts.Add(1)
			runtime.Gosched()
		}
	}()

	for index := 0; index < 8; index++ {
		version := "2.0.0"
		if index%2 == 1 {
			version = "1.0.0"
		}
		current = replaceReloadPluginVersion(t, paths, current, version)
		require.NoError(t, writeReloadPluginConfig(configPath, version, false))
		data, err := os.ReadFile(dataPath)
		require.NoError(t, err)
		data, err = sjson.SetBytes(data, "providers.example-echo.plugin.version", version)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dataPath, data, 0o600))
		require.NoError(t, store.ReloadFromDisk(t.Context()))
		registration, ok := store.ProviderRegistration("example-echo")
		require.True(t, ok)
		require.Equal(t, version, registration.Manifest.Version)
	}

	close(stop)
	<-done
	select {
	case err := <-errors:
		require.NoError(t, err)
	default:
	}
	require.Positive(t, attempts.Load())
}

func TestLoadAndReloadRequireExactPresetOwnerTuple(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	resetProviderState()
	t.Cleanup(resetProviderState)

	source, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin"))
	require.NoError(t, err)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, source)
	configPath := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{
		"providers":{"deepseek":{"api_key":"test-key","preset":{"id":"crux.catwalk.deepseek"}}},
		"models":{
			"large":{"provider":"deepseek","model":"deepseek-v4-pro","max_tokens":12288,"reasoning_effort":"high","think":true,"temperature":0.31,"top_p":0.71,"top_k":27,"frequency_penalty":0.41,"presence_penalty":0.51,"provider_options":{"mode":"large-preset","nested":{"enabled":true}}},
			"small":{"provider":"deepseek","model":"deepseek-v4-flash","max_tokens":6144,"reasoning_effort":"medium","think":true,"temperature":0.32,"top_p":0.72,"top_k":28,"frequency_penalty":0.42,"presence_penalty":0.52,"provider_options":{"mode":"small-preset","nested":{"enabled":false}}}
		}
	}`), 0o600))

	store, err := Load(configDir, filepath.Join(root, "workspace"), false)
	require.NoError(t, err)
	active, ok := ActiveProviderPreset("deepseek")
	require.True(t, ok)
	require.Equal(t, "crux.catwalk.deepseek", active.ID)
	require.Equal(t, "0.51.23", active.Version)
	require.NotEmpty(t, active.Digest)
	provider, ok := store.Config().Providers.Get("deepseek")
	require.True(t, ok)
	require.Equal(t, &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}, provider.Owner)
	require.Equal(t, &active, provider.Preset)
	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, "preset", gjson.GetBytes(data, "providers.deepseek.owner.type").String())
	require.Equal(t, "openai-compat", gjson.GetBytes(data, "providers.deepseek.owner.construction").String())
	require.Equal(t, active.ID, gjson.GetBytes(data, "providers.deepseek.preset.id").String())
	require.Equal(t, active.Version, gjson.GetBytes(data, "providers.deepseek.preset.version").String())
	require.Equal(t, active.Digest, gjson.GetBytes(data, "providers.deepseek.preset.digest").String())

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	provider, ok = store.Config().Providers.Get("deepseek")
	require.True(t, ok)
	require.Equal(t, &active, provider.Preset)
	require.True(t, store.Config().IsProviderIntegrationAvailable("deepseek"))
	largeBefore := store.Config().Models[SelectedModelTypeLarge]
	smallBefore := store.Config().Models[SelectedModelTypeSmall]

	data, err = sjson.SetBytes(data, "providers.deepseek.preset.digest", "sha256:mismatch")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(GlobalConfigData(), data, 0o600))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Equal(t, largeBefore, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, smallBefore, store.Config().Models[SelectedModelTypeSmall])
	provider, ok = store.Config().Providers.Get("deepseek")
	require.True(t, ok)
	require.Equal(t, &ProviderOwnerReference{Type: ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}, provider.Owner)
	require.Equal(t, "sha256:mismatch", provider.Preset.Digest)
	require.False(t, store.Config().IsProviderIntegrationAvailable("deepseek"))
}

func TestLoadAndReloadRequireExactCoreOwnerTuple(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "config")
	dataRoot := filepath.Join(root, "data")
	require.NoError(t, os.MkdirAll(configDir, 0o755))
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("AI_CLI_DIR", filepath.Join(root, "accounts"))
	resetProviderState()
	t.Cleanup(resetProviderState)

	configPath := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"providers":{"copilot":{"disable":true}}}`), 0o600))
	store, err := Load(configDir, filepath.Join(root, "workspace"), false)
	require.NoError(t, err)
	provider, ok := store.Config().Providers.Get("copilot")
	require.True(t, ok)
	expected := &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCopilot}
	require.Equal(t, expected, provider.Owner)
	require.Nil(t, provider.Plugin)
	require.Nil(t, provider.Preset)
	registration, ok := store.ProviderRegistration("copilot")
	require.True(t, ok)
	require.Equal(t, providerregistry.ConstructionCopilot, registration.Construction)
	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, "core", gjson.GetBytes(data, "providers.copilot.owner.type").String())
	require.Equal(t, "integrated-copilot", gjson.GetBytes(data, "providers.copilot.owner.construction").String())

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	provider, ok = store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, expected, provider.Owner)
	published := store.Config()

	data, err = sjson.SetBytes(data, "providers.copilot.owner.construction", "integrated-gemini-antigravity")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(GlobalConfigData(), data, 0o600))
	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, `provider "copilot" core owner requires construction "integrated-copilot"`)
	require.Same(t, published, store.Config())
	provider, ok = store.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, expected, provider.Owner)
}

func TestReloadRejectsConflictingProviderLogicalIDWithoutPublishing(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	published := store.Config()
	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "providers.example-echo.id", "other")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(GlobalConfigData(), data, 0o600))

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, `provider "example-echo" declares conflicting ID "other"`)
	require.Same(t, published, store.Config())
	provider, ok := store.Config().Providers.Get("example-echo")
	require.True(t, ok)
	require.Equal(t, "example-echo", provider.ID)
}

func TestReloadScanFailureDoesNotPublish(t *testing.T) {
	store, configPath, _, status := setupReloadPluginStore(t)
	oldConfig := store.Config()
	oldCatalog := store.KnownProviders()
	oldRegistration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	resolvedBefore, err := store.Resolve("$CRUX_RELOAD_TRANSACTION_VALUE")
	require.NoError(t, err)
	require.Equal(t, "candidate", resolvedBefore)
	t.Setenv("CRUX_RELOAD_TRANSACTION_VALUE", "published")
	require.NoError(t, writeReloadPluginConfig(configPath, "1.0.0", true))
	baseEnvironment := environmentValues(store.baseEnvironment)
	baseEnvironment["CRUX_PROVIDER_PROFILE"] = "invalid-profile"
	store.baseEnvironment = env.NewFromMap(baseEnvironment)

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "failed to scan providers during reload")
	require.Same(t, oldConfig, store.Config())
	require.False(t, store.Config().Options.Debug)
	require.Equal(t, oldCatalog, store.KnownProviders())
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, oldRegistration.Owner(), registration.Owner())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
	require.Equal(t, "published", os.Getenv("CRUX_RELOAD_TRANSACTION_VALUE"))
	resolvedAfter, resolveErr := store.Resolve("$CRUX_RELOAD_TRANSACTION_VALUE")
	require.NoError(t, resolveErr)
	require.Equal(t, "candidate", resolvedAfter)
}

func TestReloadRevalidatesPluginTrustBeforePublishing(t *testing.T) {
	store, _, paths, status := setupReloadPluginStore(t)
	previous := store.Config()
	dataPath := GlobalConfigData()
	dataBefore, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataBefore, err = sjson.DeleteBytes(dataBefore, "providers.example-echo.owner")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataBefore, 0o600))
	state := setReloadBoundaryMutation(store, func() {
		manager, err := providerplugin.NewManager(t.Context(), paths)
		require.NoError(t, err)
		snapshot := manager.Snapshot()
		_, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{
			Digest: status.Digest, Trusted: false, ExpectedRevision: snapshot.Revision,
		})
		require.NoError(t, err)
		manager.Close()
	})

	err = store.ReloadFromDisk(t.Context())
	requireReloadRevalidationFailure(t, store, previous, state, err, "provider trust, compatibility, or exact owner generation changed before publication")
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataBefore, dataAfter)
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", registration.Manifest.Version)
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, providerplugin.TrustTrusted, published.Trust)
	require.Equal(t, providerplugin.StateRegistered, published.State)
}

func TestReloadRevalidatesPluginCompatibilityBeforePublishing(t *testing.T) {
	store, _, paths, status := setupReloadPluginStore(t)
	previous := store.Config()
	state := setReloadBoundaryMutation(store, func() {
		manifestPath := filepath.Join(paths.Bundles, status.BundleName, "manifest.json")
		value, err := os.ReadFile(manifestPath)
		require.NoError(t, err)
		value, err = sjson.SetBytes(value, "compatibility.host_api.min", pluginmanifest.HostAPIVersion+1)
		require.NoError(t, err)
		value, err = sjson.SetBytes(value, "compatibility.host_api.max", pluginmanifest.HostAPIVersion+1)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(manifestPath, value, 0o600))
	})

	err := store.ReloadFromDisk(t.Context())
	requireReloadRevalidationFailure(t, store, previous, state, err, "provider trust, compatibility, or exact owner generation changed before publication")
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", registration.Manifest.Version)
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, providerplugin.CompatibilityCompatible, published.Compatibility)
	require.Equal(t, providerplugin.StateRegistered, published.State)
}

func TestReloadRevalidatesPluginVersionBeforePublishing(t *testing.T) {
	store, _, paths, status := setupReloadPluginStore(t)
	previous := store.Config()
	state := setReloadBoundaryMutation(store, func() {
		replaceReloadPluginVersion(t, paths, status, "2.0.0")
	})

	err := store.ReloadFromDisk(t.Context())
	requireReloadRevalidationFailure(t, store, previous, state, err, "provider trust, compatibility, or exact owner generation changed before publication")
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", registration.Manifest.Version)
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
}

func TestReloadRevalidatesPresetDigestBeforePublishing(t *testing.T) {
	store, _, _, _ := setupReloadPresetStore(t)
	previous := store.Config()
	active, ok := ActiveProviderPreset("deepseek")
	require.True(t, ok)
	dataPath := GlobalConfigData()
	dataBefore, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	state := setReloadBoundaryMutation(store, func() {
		value, err := sjson.SetBytes(dataBefore, "providers.deepseek.preset.digest", "sha256:replacement")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(dataPath, value, 0o600))
	})

	err = store.ReloadFromDisk(t.Context())
	requireReloadRevalidationFailure(t, store, previous, state, err, "persisted provider owner changed before publication")
	published, found := ActiveProviderPreset("deepseek")
	require.True(t, found)
	require.Equal(t, active, published)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataBefore, dataAfter)
}

func TestReloadRevalidatesCompleteSelectedValuesBeforePublishing(t *testing.T) {
	store, configPath, _, _ := setupReloadPluginStore(t)
	previous := store.Config()
	largeBefore := previous.Models[SelectedModelTypeLarge]
	smallBefore := previous.Models[SelectedModelTypeSmall]
	configBefore, err := os.ReadFile(configPath)
	require.NoError(t, err)
	state := setReloadBoundaryMutation(store, func() {
		value, err := sjson.SetBytes(configBefore, "models.large.provider_options.nested.enabled", false)
		require.NoError(t, err)
		value, err = sjson.SetBytes(value, "models.small.max_tokens", 7777)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, value, 0o600))
	})

	err = store.ReloadFromDisk(t.Context())
	requireReloadRevalidationFailure(t, store, previous, state, err, "selected model values changed before publication")
	require.Equal(t, largeBefore, store.Config().Models[SelectedModelTypeLarge])
	require.Equal(t, smallBefore, store.Config().Models[SelectedModelTypeSmall])
	configAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, configBefore, configAfter)
}

func TestReloadRevalidatesAfterPublicationWaitAndPreservesLaterEdit(t *testing.T) {
	store, configPath, _, status := setupReloadPluginStore(t)
	previous := store.Config()
	oldCatalog := store.KnownProviders()
	oldRegistration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	dataPath := GlobalConfigData()
	dataBefore, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataBefore, err = sjson.SetBytes(dataBefore, "options.disable_notifications", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataBefore, 0o600))

	state := &reloadRuntimeCandidateState{}
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{
			Commit: func() { state.committed = true },
			Abort: func() {
				if !state.committed {
					state.aborted = true
				}
			},
		}, nil
	})
	publicationReached := make(chan struct{})
	var publicationBlocked atomic.Bool
	originalPublish := publishReloadProviderScan
	publishReloadProviderScan = func(scan ProviderScan, publish func(ProviderScan) error) error {
		providerStateMu.Lock()
		publicationBlocked.Store(true)
		close(publicationReached)
		return originalPublish(scan, publish)
	}
	t.Cleanup(func() {
		publishReloadProviderScan = originalPublish
		if publicationBlocked.CompareAndSwap(true, false) {
			providerStateMu.Unlock()
		}
	})

	reloadErr := make(chan error, 1)
	go func() { reloadErr <- store.ReloadFromDisk(t.Context()) }()
	select {
	case <-publicationReached:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not reach provider publication")
	}
	corrected, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(corrected, "options.disable_notifications").Exists())
	require.Equal(t, "disabled", gjson.GetBytes(corrected, "options.notifications").String())
	later, err := sjson.SetBytes(corrected, "later_user_edit", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, later, 0o600))
	source, err := os.ReadFile(configPath)
	require.NoError(t, err)
	sourceLater, err := sjson.SetBytes(source, "models.large.provider_options.nested.enabled", false)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, sourceLater, 0o600))
	require.True(t, publicationBlocked.CompareAndSwap(true, false))
	providerStateMu.Unlock()

	select {
	case err = <-reloadErr:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not finish after provider publication resumed")
	}
	requireReloadRevalidationFailure(t, store, previous, state, err, "selected model values changed before publication")
	require.Equal(t, oldCatalog, store.KnownProviders())
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, oldRegistration.Owner(), registration.Owner())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, later, dataAfter)
	sourceAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, sourceLater, sourceAfter)
}

func TestReloadRejectsLaterEditBeforeOwnerMigration(t *testing.T) {
	store, _, _, status := setupReloadPluginStore(t)
	previous := store.Config()
	oldCatalog := store.KnownProviders()
	oldRegistration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	dataPath := GlobalConfigData()
	dataBefore, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataBefore, err = sjson.DeleteBytes(dataBefore, "providers.example-echo.owner")
	require.NoError(t, err)
	dataBefore, err = sjson.SetBytes(dataBefore, "options.disable_notifications", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataBefore, 0o600))
	journalBefore, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)

	state := &reloadRuntimeCandidateState{}
	store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{
			Commit: func() { state.committed = true },
			Abort: func() {
				if !state.committed {
					state.aborted = true
				}
			},
		}, nil
	})
	postimagesCaptured := make(chan struct{})
	releaseCapture := make(chan struct{})
	var captureReleased atomic.Bool
	originalCapture := captureReloadConfigPostimages
	captureReloadConfigPostimages = func(preimages []configPreimage) error {
		err := originalCapture(preimages)
		close(postimagesCaptured)
		<-releaseCapture
		return err
	}
	t.Cleanup(func() {
		captureReloadConfigPostimages = originalCapture
		if captureReleased.CompareAndSwap(false, true) {
			close(releaseCapture)
		}
	})

	reloadErr := make(chan error, 1)
	go func() { reloadErr <- store.ReloadFromDisk(t.Context()) }()
	select {
	case <-postimagesCaptured:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not capture correction postimages")
	}
	corrected, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(corrected, "options.disable_notifications").Exists())
	require.Equal(t, "disabled", gjson.GetBytes(corrected, "options.notifications").String())
	require.False(t, gjson.GetBytes(corrected, "providers.example-echo.owner").Exists())
	later, err := sjson.SetBytes(corrected, "later_user_edit", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, later, 0o600))
	require.True(t, captureReleased.CompareAndSwap(false, true))
	close(releaseCapture)

	select {
	case err = <-reloadErr:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not finish after correction capture resumed")
	}
	require.ErrorContains(t, err, "migrate provider ownership during reload")
	require.ErrorContains(t, err, "provider ownership migration source changed before mutation")
	require.Same(t, previous, store.Config())
	require.True(t, state.aborted)
	require.False(t, state.committed)
	require.Equal(t, oldCatalog, store.KnownProviders())
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, oldRegistration.Owner(), registration.Owner())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, later, dataAfter)
	journalAfter, readErr := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, readErr)
	require.Equal(t, journalBefore, journalAfter)
}

func TestReloadPublicationFailureRestoresFilesAndGeneration(t *testing.T) {
	store, configPath, _, status := setupReloadPluginStore(t)
	oldConfig := store.Config()
	oldCatalog := store.KnownProviders()
	oldRegistration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	sourceCandidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	sourceCandidate, err = sjson.SetBytes(sourceCandidate, "options.debug", true)
	require.NoError(t, err)
	sourceCandidate, err = sjson.SetBytes(sourceCandidate, "options.notification_style", "bell")
	require.NoError(t, err)
	sourceCandidate, err = sjson.SetBytes(sourceCandidate, "env.CRUX_RELOAD_TRANSACTION_VALUE", "replacement")
	require.NoError(t, err)
	sourceCandidate, err = sjson.SetBytes(sourceCandidate, "env.T3_9_ENV_ROLLBACK_FAILURE", "replacement")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, sourceCandidate, 0o600))
	dataPath := GlobalConfigData()
	dataCandidate, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataCandidate, err = sjson.DeleteBytes(dataCandidate, "providers.example-echo.owner")
	require.NoError(t, err)
	dataCandidate, err = sjson.SetBytes(dataCandidate, "options.disable_notifications", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataCandidate, 0o600))
	t.Setenv("CRUX_RELOAD_TRANSACTION_VALUE", "published")
	store.setEnvironment = func(key, value string) error {
		if key == "T3_9_ENV_ROLLBACK_FAILURE" {
			return fmt.Errorf("set blocked")
		}
		return os.Setenv(key, value)
	}

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, `apply environment variable "T3_9_ENV_ROLLBACK_FAILURE"`)
	require.Same(t, oldConfig, store.Config())
	require.Equal(t, oldCatalog, store.KnownProviders())
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, oldRegistration.Owner(), registration.Owner())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
	require.Equal(t, "published", os.Getenv("CRUX_RELOAD_TRANSACTION_VALUE"))
	resolved, resolveErr := store.Resolve("$CRUX_RELOAD_TRANSACTION_VALUE")
	require.NoError(t, resolveErr)
	require.Equal(t, "candidate", resolved)
	sourceAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, sourceCandidate, sourceAfter)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataCandidate, dataAfter)
}

func TestReloadSetupFailureDoesNotPublish(t *testing.T) {
	store, configPath, _, status := setupReloadPluginStore(t)
	oldConfig := store.Config()
	oldCatalog := store.KnownProviders()
	oldRegistration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	value, err := os.ReadFile(configPath)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "options.debug", true)
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "models.large.provider", "missing-provider")
	require.NoError(t, err)
	value, err = sjson.SetBytes(value, "models.large.model", "missing-model")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, value, 0o600))
	t.Setenv("CRUX_RELOAD_TRANSACTION_VALUE", "published")

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "selected provider missing-provider is not available")
	require.Same(t, oldConfig, store.Config())
	require.False(t, store.Config().Options.Debug)
	require.Equal(t, oldCatalog, store.KnownProviders())
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, oldRegistration.Owner(), registration.Owner())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
	require.Equal(t, "published", os.Getenv("CRUX_RELOAD_TRANSACTION_VALUE"))
}

func TestReloadFailureLeavesMigrationFilesUnchanged(t *testing.T) {
	store, configPath, _, _ := setupReloadPluginStore(t)
	dataPath := GlobalConfigData()
	dataOriginal, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataOriginal, err = sjson.SetBytes(dataOriginal, "options.disable_notifications", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataOriginal, 0o600))
	sourceOriginal, err := os.ReadFile(configPath)
	require.NoError(t, err)
	sourceOriginal, err = sjson.SetBytes(sourceOriginal, "options.notification_style", "bell")
	require.NoError(t, err)
	sourceOriginal, err = sjson.DeleteBytes(sourceOriginal, "providers.example-echo.plugin")
	require.NoError(t, err)
	sourceOriginal, err = sjson.SetBytes(sourceOriginal, "models.large.provider", "missing-provider")
	require.NoError(t, err)
	sourceOriginal, err = sjson.SetBytes(sourceOriginal, "models.large.model", "missing-model")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, sourceOriginal, 0o600))

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "selected provider missing-provider is not available")
	sourceAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, sourceOriginal, sourceAfter)
	require.Equal(t, dataOriginal, dataAfter)
}

func TestReloadOwnershipMigrationCommitFailureLeavesFilesAndGenerationUnchanged(t *testing.T) {
	store, configPath, _, status := setupReloadPluginStore(t)
	oldConfig := store.Config()
	oldCatalog := store.KnownProviders()
	accountFile := filepath.Join(os.Getenv("AI_CLI_DIR"), "accounts.json")
	accountData := []byte(`{"account":{"access":"unchanged"}}`)
	require.NoError(t, os.MkdirAll(filepath.Dir(accountFile), 0o755))
	require.NoError(t, os.WriteFile(accountFile, accountData, 0o600))
	candidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "options.debug", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
	dataPath := GlobalConfigData()
	dataCandidate, err := os.ReadFile(dataPath)
	require.NoError(t, err)
	dataCandidate, err = sjson.DeleteBytes(dataCandidate, "providers.example-echo.owner")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(dataPath, dataCandidate, 0o600))
	journalBefore, err := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, err)
	originalWriter := writeProviderMigrationConfig
	writeProviderMigrationConfig = func(string, []byte, os.FileMode) error { return fmt.Errorf("commit blocked") }
	t.Cleanup(func() { writeProviderMigrationConfig = originalWriter })

	err = store.ReloadFromDisk(t.Context())
	require.ErrorContains(t, err, "commit provider ownership migration")
	configAfter, readErr := os.ReadFile(configPath)
	require.NoError(t, readErr)
	require.Equal(t, candidate, configAfter)
	dataAfter, readErr := os.ReadFile(dataPath)
	require.NoError(t, readErr)
	require.Equal(t, dataCandidate, dataAfter)
	accountsAfter, readErr := os.ReadFile(accountFile)
	require.NoError(t, readErr)
	require.Equal(t, accountData, accountsAfter)
	journalAfter, readErr := os.ReadFile(providerMigrationJournalPath())
	require.NoError(t, readErr)
	require.Equal(t, journalBefore, journalAfter)
	require.Same(t, oldConfig, store.Config())
	require.Equal(t, oldCatalog, store.KnownProviders())
	published, _, found := ProviderPluginAvailability(status.ID)
	require.True(t, found)
	require.Equal(t, "1.0.0", published.Version)
}

func TestReloadPublishesCoherentProviderGeneration(t *testing.T) {
	store, configPath, paths, status := setupReloadPluginStore(t)
	oldConfig := store.Config()
	replacement := replaceReloadPluginVersion(t, paths, status, "2.0.0")
	ownerData, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	ownerData, err = sjson.SetBytes(ownerData, "providers.example-echo.plugin.version", "2.0.0")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(GlobalConfigData(), ownerData, 0o600))
	require.NoError(t, writeReloadPluginConfig(configPath, "2.0.0", true))
	candidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "env.CRUX_RELOAD_TRANSACTION_VALUE", "replacement")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, candidate, 0o600))

	stop := make(chan struct{})
	observations := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			cfg := store.Config()
			if cfg.Options.Debug {
				if len(cfg.Agents) == 0 {
					select {
					case observations <- "new config was visible before agent setup":
					default:
					}
					return
				}
				registration, ok := cfg.ProviderRegistration("example-echo")
				if !ok || registration.Manifest == nil || registration.Manifest.Version != "2.0.0" {
					select {
					case observations <- "new config was paired with the wrong registry generation":
					default:
					}
					return
				}
				catalog, err := Providers(cfg)
				if err != nil || !containsProvider(catalog, "example-echo") {
					select {
					case observations <- "new config was paired with the wrong catalog generation":
					default:
					}
					return
				}
			}
			runtime.Gosched()
		}
	}()

	require.NoError(t, store.ReloadFromDisk(t.Context()))
	close(stop)
	<-done
	select {
	case observation := <-observations:
		t.Fatal(observation)
	default:
	}

	cfg := store.Config()
	require.True(t, cfg.Options.Debug)
	require.NotEmpty(t, cfg.Agents)
	require.Equal(t, "example-echo", cfg.Models[SelectedModelTypeLarge].Provider)
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "2.0.0", registration.Manifest.Version)
	provider, ok := cfg.Providers.Get("example-echo")
	require.True(t, ok)
	require.Equal(t, providerOwnerReferenceForRegistration(registration), provider.Owner)
	data, err := os.ReadFile(GlobalConfigData())
	require.NoError(t, err)
	require.Equal(t, "plugin", gjson.GetBytes(data, "providers.example-echo.owner.type").String())
	require.Equal(t, string(registration.Construction), gjson.GetBytes(data, "providers.example-echo.owner.construction").String())
	require.Equal(t, string(registration.CompatibilityAdapter), gjson.GetBytes(data, "providers.example-echo.owner.compatibility_adapter").String())
	require.Equal(t, "example.echo", gjson.GetBytes(data, "providers.example-echo.plugin.id").String())
	require.Equal(t, "2.0.0", gjson.GetBytes(data, "providers.example-echo.plugin.version").String())
	published, mode, found := ProviderPluginAvailability(replacement.ID)
	require.True(t, found)
	require.Equal(t, "2.0.0", published.Version)
	require.Equal(t, providerregistry.OwnerPluginNative, mode)
	require.Equal(t, "replacement", os.Getenv("CRUX_RELOAD_TRANSACTION_VALUE"))
	resolvedEnvironment, resolveErr := store.Resolve("$CRUX_RELOAD_TRANSACTION_VALUE")
	require.NoError(t, resolveErr)
	require.Equal(t, "replacement", resolvedEnvironment)
	oldRegistration, ok := oldConfig.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", oldRegistration.Manifest.Version)
}

func TestReloadUsesCapturedHostEnvironment(t *testing.T) {
	capturedHome := filepath.Join(t.TempDir(), "captured-home")
	capturedSkills := filepath.Join(t.TempDir(), "captured-skills")
	t.Setenv("HOME", capturedHome)
	t.Setenv("CRUX_SKILLS_DIR", capturedSkills)
	t.Setenv("T3_9_SHELL_HOST", "captured-shell")
	store, configPath, _, _ := setupReloadPluginStore(t)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(configPath), "cruxrc"), []byte("option initialize-as \"$T3_9_SHELL_HOST\"\n"), 0o600))
	oldSkills := append([]string(nil), store.Config().Options.SkillsPaths...)
	oldDisableDefaultProviders := store.Config().Options.DisableDefaultProviders

	replacementRoot := t.TempDir()
	replacementConfig := filepath.Join(replacementRoot, "config")
	replacementData := filepath.Join(replacementRoot, "data")
	require.NoError(t, os.MkdirAll(replacementConfig, 0o755))
	require.NoError(t, os.MkdirAll(replacementData, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(replacementConfig, "crux.json"), []byte(`{invalid`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(replacementData, "crux.json"), []byte(`{invalid`), 0o600))
	t.Setenv("CRUX_GLOBAL_CONFIG", replacementConfig)
	t.Setenv("CRUX_GLOBAL_DATA", replacementData)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(replacementRoot, "cache"))
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileCoreOnly))
	t.Setenv("CRUX_PROVIDER_PLUGINS", "")
	t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")
	t.Setenv("CRUX_DISABLE_DEFAULT_PROVIDERS", "true")
	t.Setenv("CRUX_SKILLS_DIR", filepath.Join(replacementRoot, "skills"))
	t.Setenv("HOME", filepath.Join(replacementRoot, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(replacementRoot, "xdg-config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(replacementRoot, "xdg-data"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(replacementRoot, "xdg-cache"))
	t.Setenv("AI_CLI_DIR", filepath.Join(replacementRoot, "accounts"))
	t.Setenv("T3_9_SHELL_HOST", "replacement-shell")

	candidate, err := os.ReadFile(configPath)
	require.NoError(t, err)
	candidate, err = sjson.SetBytes(candidate, "options.debug", true)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
	require.NoError(t, store.ReloadFromDisk(t.Context()))

	cfg := store.Config()
	require.True(t, cfg.Options.Debug)
	require.Equal(t, "captured-shell", cfg.Options.InitializeAs)
	require.Equal(t, oldSkills, cfg.Options.SkillsPaths)
	require.Equal(t, oldDisableDefaultProviders, cfg.Options.DisableDefaultProviders)
	registration, ok := store.ProviderRegistration("example-echo")
	require.True(t, ok)
	require.Equal(t, "1.0.0", registration.Manifest.Version)
	require.NotEqual(t, replacementData, filepath.Dir(store.globalDataPath))
}

func TestReloadRejectsConfiguredHostEnvironmentWithoutPublishing(t *testing.T) {
	store, configPath, _, _ := setupReloadPluginStore(t)
	published := store.Config()
	original, err := os.ReadFile(configPath)
	require.NoError(t, err)

	for key := range immutableHostEnvironmentVariables {
		t.Run(key, func(t *testing.T) {
			candidate, setErr := sjson.SetBytes(original, "env."+key, "candidate")
			require.NoError(t, setErr)
			require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
			reloadErr := store.ReloadFromDisk(t.Context())
			require.ErrorContains(t, reloadErr, fmt.Sprintf("environment variable %q is a startup-only host setting", key))
			require.Same(t, published, store.Config())
		})
	}
	require.NoError(t, os.WriteFile(configPath, original, 0o600))
}

func TestReloadRemovedEnvironmentOverlayRestoresCapturedBase(t *testing.T) {
	const key = "CRUX_RELOAD_TRANSACTION_VALUE"
	t.Run("present", func(t *testing.T) {
		t.Setenv(key, "base")
		store, configPath, _, _ := setupReloadPluginStore(t)
		require.Equal(t, "candidate", os.Getenv(key))
		candidate, err := os.ReadFile(configPath)
		require.NoError(t, err)
		candidate, err = sjson.DeleteBytes(candidate, "env."+key)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
		require.NoError(t, store.ReloadFromDisk(t.Context()))
		require.Equal(t, "base", os.Getenv(key))
	})

	t.Run("absent", func(t *testing.T) {
		original, existed := os.LookupEnv(key)
		require.NoError(t, os.Unsetenv(key))
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(key, original)
			} else {
				_ = os.Unsetenv(key)
			}
		})
		store, configPath, _, _ := setupReloadPluginStore(t)
		require.Equal(t, "candidate", os.Getenv(key))
		candidate, err := os.ReadFile(configPath)
		require.NoError(t, err)
		candidate, err = sjson.DeleteBytes(candidate, "env."+key)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(configPath, candidate, 0o600))
		require.NoError(t, store.ReloadFromDisk(t.Context()))
		_, exists := os.LookupEnv(key)
		require.False(t, exists)
	})
}

func TestReloadOwnershipMigrationUsesCapturedGlobalDataRoot(t *testing.T) {
	store, _, _, _ := setupReloadPluginStore(t)
	capturedDataPath := store.globalDataPath
	candidate, err := os.ReadFile(capturedDataPath)
	require.NoError(t, err)
	candidate, err = sjson.DeleteBytes(candidate, "providers.example-echo.owner")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(capturedDataPath, candidate, 0o600))

	replacementData := filepath.Join(t.TempDir(), "replacement-data")
	t.Setenv("CRUX_GLOBAL_DATA", replacementData)
	require.NoError(t, store.ReloadFromDisk(t.Context()))

	migrated, err := os.ReadFile(capturedDataPath)
	require.NoError(t, err)
	require.Equal(t, "plugin", gjson.GetBytes(migrated, "providers.example-echo.owner.type").String())
	require.FileExists(t, providerMigrationJournalPathForConfig(capturedDataPath))
	_, err = os.Stat(filepath.Join(replacementData, "provider-migrations", "v1", "journal.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func containsProvider(providers []catalog.Provider, providerID string) bool {
	for _, provider := range providers {
		if string(provider.ID) == providerID {
			return true
		}
	}
	return false
}
