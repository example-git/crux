package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func resetProviderState() {
	providerStateMu.Lock()
	defer providerStateMu.Unlock()
	providerOnce = sync.Once{}
	providerList = nil
	providerRegistry = nil
	providerPluginStatuses = nil
	providerPresetReferences = nil
	providerOwnerModes = nil
	providerErr = nil
}

func TestProviderRolloutPolicyProfilesAndIndependentGates(t *testing.T) {
	integratedConstruction := providerregistry.Construction("integrated-protected")
	integrated := []providerregistry.Registration{
		{ProviderID: "protected", Construction: integratedConstruction},
		{ProviderID: "copilot", Construction: providerregistry.ConstructionCopilot},
	}
	compatibility := providerregistry.Registration{ProviderID: "protected", Construction: integratedConstruction, CompatibilityAdapter: integratedConstruction}
	native := providerregistry.Registration{ProviderID: "protected", Construction: providerregistry.ConstructionGenericJSON}
	ordinary := providerregistry.Registration{ProviderID: "ordinary", Construction: providerregistry.ConstructionGenericJSON}
	copilotPlugin := providerregistry.Registration{ProviderID: "copilot", Construction: providerregistry.ConstructionGenericJSON}

	for _, test := range []struct {
		name     string
		profile  ProviderProfile
		enabled  map[string]bool
		plugin   providerregistry.Registration
		expected map[string]providerregistry.OwnerMode
	}{
		{
			name: "core only", profile: ProviderProfileCoreOnly, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "core only explicit allowlist", profile: ProviderProfileCoreOnly, enabled: map[string]bool{"protected": true, "copilot": true, "ordinary": true}, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "integrated", profile: ProviderProfileIntegrated, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerIntegrated, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "integrated explicit allowlist", profile: ProviderProfileIntegrated, enabled: map[string]bool{"protected": true, "copilot": true, "ordinary": true}, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerIntegrated, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "plugin compat empty allowlist", profile: ProviderProfilePluginCompat, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerPluginCompat, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
		{
			name: "plugin compat includes protected", profile: ProviderProfilePluginCompat, enabled: map[string]bool{"protected": true}, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerPluginCompat, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "plugin compat includes copilot", profile: ProviderProfilePluginCompat, enabled: map[string]bool{"copilot": true}, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "plugin compat includes ordinary", profile: ProviderProfilePluginCompat, enabled: map[string]bool{"ordinary": true}, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
		{
			name: "plugin compat accepts native", profile: ProviderProfilePluginCompat, plugin: native,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerPluginNative, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
		{
			name: "plugin native rejects compatibility", profile: ProviderProfilePluginNative, plugin: compatibility,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
		{
			name: "plugin native empty allowlist", profile: ProviderProfilePluginNative, plugin: native,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerPluginNative, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
		{
			name: "plugin native includes protected", profile: ProviderProfilePluginNative, enabled: map[string]bool{"protected": true}, plugin: native,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerPluginNative, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "plugin native includes copilot", profile: ProviderProfilePluginNative, enabled: map[string]bool{"copilot": true}, plugin: native,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerDisabled},
		},
		{
			name: "plugin native includes ordinary", profile: ProviderProfilePluginNative, enabled: map[string]bool{"ordinary": true}, plugin: native,
			expected: map[string]providerregistry.OwnerMode{"protected": providerregistry.OwnerDisabled, "copilot": providerregistry.OwnerIntegrated, "ordinary": providerregistry.OwnerPluginNative},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy := providerRolloutPolicy{Profile: test.profile, Enabled: test.enabled, ExplicitProfile: true}
			modes := rolloutOwnerModes(policy, integrated, []providerregistry.Registration{test.plugin, ordinary, copilotPlugin})
			require.Equal(t, test.expected, modes)
		})
	}
}

func TestParseProviderRolloutPolicyRejectsUnknownProfile(t *testing.T) {
	t.Setenv("CRUX_PROVIDER_PROFILE", "surprise")
	_, err := parseProviderRolloutPolicy()
	require.EqualError(t, err, `unknown provider rollout profile "surprise"`)
}

func TestParseProviderRolloutPolicyProfilesAndAllowlist(t *testing.T) {
	previous := DefaultProviderProfile
	DefaultProviderProfile = string(ProviderProfilePluginCompat)
	t.Cleanup(func() { DefaultProviderProfile = previous })

	for _, profile := range []ProviderProfile{
		ProviderProfileCoreOnly,
		ProviderProfileIntegrated,
		ProviderProfilePluginCompat,
		ProviderProfilePluginNative,
	} {
		t.Run(string(profile), func(t *testing.T) {
			policy, err := parseProviderRolloutPolicyFromEnvironment(env.NewFromMap(map[string]string{
				"CRUX_PROVIDER_PROFILE": string(profile),
				"CRUX_PROVIDER_PLUGINS": " codex, gemini-ag, codex ",
			}))
			require.NoError(t, err)
			require.Equal(t, profile, policy.Profile)
			require.True(t, policy.ExplicitProfile)
			require.Equal(t, map[string]bool{"codex": true, "gemini-ag": true}, policy.Enabled)
		})
	}
}

func TestParseProviderRolloutPolicyRejectsLegacyCompatAllowlist(t *testing.T) {
	_, err := parseProviderRolloutPolicyFromEnvironment(env.NewFromMap(map[string]string{
		"CRUX_PROVIDER_PLUGIN_COMPAT": "codex",
	}))
	require.EqualError(t, err, "CRUX_PROVIDER_PLUGIN_COMPAT is unsupported; use CRUX_PROVIDER_PROFILE=plugin-compat with CRUX_PROVIDER_PLUGINS")
}

func TestPublishedProviderStatusIsDetached(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	status := providerplugin.Status{
		ID:           "plugin",
		ProviderID:   "provider",
		Capabilities: []string{"inference"},
		Diagnostics:  []providerplugin.Diagnostic{{Code: "ready", Message: "ready"}},
	}
	publishProviderScan(ProviderScan{
		pluginStatuses: map[string]providerplugin.Status{"plugin": status},
		ownerModes:     map[string]providerregistry.OwnerMode{"provider": providerregistry.OwnerPluginNative},
	}, nil)
	status.Capabilities[0] = "mutated-candidate"
	status.Diagnostics[0].Code = "mutated-candidate"

	published, _, found := ProviderPluginAvailability("plugin")
	require.True(t, found)
	require.Equal(t, []string{"inference"}, published.Capabilities)
	require.Equal(t, "ready", published.Diagnostics[0].Code)
	published.Capabilities[0] = "mutated-caller"
	published.Diagnostics[0].Code = "mutated-caller"
	retained, _, found := ProviderPluginAvailability("plugin")
	require.True(t, found)
	require.Equal(t, []string{"inference"}, retained.Capabilities)
	require.Equal(t, "ready", retained.Diagnostics[0].Code)
}

func TestFailedLazyProviderScanRetainsPublishedGeneration(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	priorRegistry, err := providerregistry.New(providerregistry.Registration{ProviderID: "prior"})
	require.NoError(t, err)
	providerStateMu.Lock()
	providerList = []catalog.Provider{{ID: "prior", Name: "Prior"}}
	providerRegistry = priorRegistry
	providerPluginStatuses = map[string]providerplugin.Status{"prior.plugin": {ID: "prior.plugin", ProviderID: "prior"}}
	providerPresetReferences = map[string]ProviderPresetReference{"prior": {ID: "prior.preset", Version: "1.0.0", Digest: strings.Repeat("a", 64)}}
	providerOwnerModes = map[string]providerregistry.OwnerMode{"prior": providerregistry.OwnerPluginNative}
	providerStateMu.Unlock()
	t.Setenv("CRUX_PROVIDER_PROFILE", "invalid-profile")

	providers, err := Providers(&Config{Options: &Options{}})
	require.ErrorContains(t, err, `unknown provider rollout profile "invalid-profile"`)
	require.Equal(t, []catalog.Provider{{ID: "prior", Name: "Prior"}}, providers)
	registration, found := ProviderRegistry().Lookup("prior")
	require.True(t, found)
	require.Equal(t, "prior", registration.ProviderID)
	status, mode, found := ProviderPluginAvailability("prior.plugin")
	require.True(t, found)
	require.Equal(t, "prior", status.ProviderID)
	require.Equal(t, providerregistry.OwnerPluginNative, mode)
	preset, found := ActiveProviderPreset("prior")
	require.True(t, found)
	require.Equal(t, "prior.preset", preset.ID)
}

func TestFreshProviderScanUsesHostEnvironmentWithoutMutatingProcess(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileCoreOnly))
	host := env.NewFromMap(map[string]string{
		"CRUX_GLOBAL_DATA":      filepath.Join(root, "data"),
		"CRUX_CACHE_DIR":        filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": "invalid-host",
	})

	_, err := freshProviderScan(t.Context(), &Config{Options: &Options{}}, host)
	require.ErrorContains(t, err, `unknown provider rollout profile "invalid-host"`)
	require.Equal(t, string(ProviderProfileCoreOnly), os.Getenv("CRUX_PROVIDER_PROFILE"))
}

func TestFreshProviderScanValidatesHostProfileWhenDefaultsDisabled(t *testing.T) {
	host := env.NewFromMap(map[string]string{"CRUX_PROVIDER_PROFILE": "invalid-host"})

	_, err := freshProviderScan(t.Context(), &Config{Options: &Options{DisableDefaultProviders: true}}, host)
	require.ErrorContains(t, err, `unknown provider rollout profile "invalid-host"`)
}

func TestFreshProviderScanUsesCapturedHostPaths(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "captured-data")
	cacheRoot := filepath.Join(root, "captured-cache")
	source, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
	require.NoError(t, err)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, source)
	host := env.NewFromMap(map[string]string{
		"CRUX_GLOBAL_DATA":      dataRoot,
		"CRUX_CACHE_DIR":        cacheRoot,
		"CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginNative),
	})
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "replacement-data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "replacement-cache"))
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileCoreOnly))

	scan, err := freshProviderScan(t.Context(), &Config{Options: &Options{}}, host)
	require.NoError(t, err)
	require.True(t, containsProvider(scan.Providers, "example-echo"))
}

func TestBuildEnvironmentResolvesCandidateValuesWithoutMutatingProcess(t *testing.T) {
	t.Setenv("T3_9_ENV_FIRST", "published-first")
	t.Setenv("T3_9_ENV_SECOND", "published-second")
	cfg := &Config{Env: map[string]string{
		"T3_9_ENV_FIRST":  "candidate-first",
		"T3_9_ENV_SECOND": "$T3_9_ENV_FIRST-second",
	}}

	candidate, resolver, resolved, err := cfg.buildEnvironment()
	require.NoError(t, err)
	require.Equal(t, "candidate-first", candidate.Get("T3_9_ENV_FIRST"))
	require.Equal(t, "candidate-first-second", candidate.Get("T3_9_ENV_SECOND"))
	value, err := resolver.ResolveValue("$T3_9_ENV_SECOND")
	require.NoError(t, err)
	require.Equal(t, "candidate-first-second", value)
	require.Equal(t, "candidate-first", resolved["T3_9_ENV_FIRST"])
	require.Equal(t, "candidate-first-second", resolved["T3_9_ENV_SECOND"])
	require.Equal(t, "published-first", os.Getenv("T3_9_ENV_FIRST"))
	require.Equal(t, "published-second", os.Getenv("T3_9_ENV_SECOND"))
}

func TestBuildEnvironmentRejectsInvalidValueWithoutMutatingProcess(t *testing.T) {
	t.Setenv("T3_9_INVALID_ENV", "published")
	cfg := &Config{Env: map[string]string{"T3_9_INVALID_ENV": "$"}}

	_, _, _, err := cfg.buildEnvironment()
	require.EqualError(t, err, `resolve environment variable "T3_9_INVALID_ENV"`)
	require.Equal(t, "published", os.Getenv("T3_9_INVALID_ENV"))
}

func TestApplyEnvironmentRollsBackEarlierValuesOnFailure(t *testing.T) {
	const key = "T3_9_ENV_APPLIED"
	const failingKey = "T3_9_ENV_ROLLBACK_FAILURE"
	t.Setenv(key, "published")
	setenv := func(name, value string) error {
		if name == failingKey {
			return fmt.Errorf("set blocked")
		}
		return os.Setenv(name, value)
	}

	err := applyEnvironmentWith(env.NewFromMap(nil), nil, map[string]string{
		key:        "candidate",
		failingKey: "invalid",
	}, setenv, os.Unsetenv)
	require.ErrorContains(t, err, `apply environment variable "T3_9_ENV_ROLLBACK_FAILURE"`)
	require.Equal(t, "published", os.Getenv(key))
}

func TestBuildEnvironmentRejectsStartupOnlyHostSettings(t *testing.T) {
	for key := range immutableHostEnvironmentVariables {
		t.Run(key, func(t *testing.T) {
			cfg := &Config{Env: map[string]string{key: "candidate"}}
			_, _, _, err := cfg.buildEnvironment()
			require.EqualError(t, err, fmt.Sprintf("environment variable %q is a startup-only host setting", key))
		})
	}
}

func TestCompiledProviderProfileIsACeiling(t *testing.T) {
	previous := DefaultProviderProfile
	DefaultProviderProfile = string(ProviderProfileCoreOnly)
	t.Cleanup(func() { DefaultProviderProfile = previous })
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfilePluginNative))
	_, err := parseProviderRolloutPolicy()
	require.EqualError(t, err, `provider rollout profile "plugin-native" exceeds compiled release profile "core-only"`)
}

func TestProvidersIgnoreHistoricalCatalog(t *testing.T) {
	root := t.TempDir()
	xdgData := filepath.Join(root, "xdg")
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("XDG_DATA_HOME", xdgData)
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)

	historical := []catalog.Provider{
		{ID: "historical", Name: "Historical", Type: catalog.TypeOpenAICompat},
		{ID: catalog.ProviderCopilot, Name: "Historical Copilot", Type: catalog.TypeOpenAICompat},
	}
	data, err := json.Marshal(historical)
	require.NoError(t, err)
	catalogPath := filepath.Join(xdgData, "crux", "providers.json")
	require.NoError(t, os.MkdirAll(filepath.Dir(catalogPath), 0o755))
	require.NoError(t, os.WriteFile(catalogPath, data, 0o644))

	resetProviderState()
	t.Cleanup(resetProviderState)
	providers, err := Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	require.Equal(t, []catalog.ProviderID{catalog.ProviderCopilot}, providerIDs(providers))
	require.Equal(t, copilot.CatalogProvider().Name, providers[0].Name)
}

func TestProviders_HonorsDisableDefaultProviders(t *testing.T) {
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(t.TempDir(), "data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(t.TempDir(), "cache"))

	resetProviderState()
	t.Cleanup(resetProviderState)

	providers, err := Providers(&Config{
		Options: &Options{DisableDefaultProviders: true},
	})
	require.NoError(t, err)
	require.Empty(t, providers)
}
