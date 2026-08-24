package config

import (
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func resetProviderState() {
	providerOnce = sync.Once{}
	providerList = nil
	providerRegistry = nil
	providerPluginStatuses = nil
	providerOwnerModes = nil
	providerErr = nil
	catwalkSyncer = &catwalkSync{}
}

func TestProviderRolloutPolicyProfilesAndIndependentGates(t *testing.T) {
	integrated := []providerregistry.Registration{{ProviderID: "compat", Construction: "integrated-compat"}, {ProviderID: "copilot", Construction: providerregistry.ConstructionCopilot}}
	plugins := []providerregistry.Registration{
		{ProviderID: "compat", CompatibilityAdapter: "integrated-compat"},
		{ProviderID: "native"},
	}

	policy := providerRolloutPolicy{Profile: ProviderProfilePluginCompat, ExplicitProfile: true, Enabled: map[string]bool{"compat": true}}
	modes := rolloutOwnerModes(policy, integrated, plugins)
	require.Equal(t, providerregistry.OwnerPluginCompat, modes["compat"])
	require.Equal(t, providerregistry.OwnerIntegrated, modes["copilot"])
	require.Equal(t, providerregistry.OwnerDisabled, modes["native"])

	modes = rolloutOwnerModes(providerRolloutPolicy{Profile: ProviderProfilePluginCompat}, integrated, nil)
	require.Equal(t, providerregistry.OwnerDisabled, modes["compat"], "compatibility targets require an active plugin")
	require.Equal(t, providerregistry.OwnerIntegrated, modes["copilot"], "core-owned Copilot remains active")

	policy = providerRolloutPolicy{Profile: ProviderProfilePluginNative}
	modes = rolloutOwnerModes(policy, integrated, plugins)
	require.Equal(t, providerregistry.OwnerDisabled, modes["compat"])
	require.Equal(t, providerregistry.OwnerPluginNative, modes["native"])

	policy = providerRolloutPolicy{Profile: ProviderProfileCoreOnly}
	modes = rolloutOwnerModes(policy, integrated, plugins)
	require.Equal(t, providerregistry.OwnerDisabled, modes["compat"])
	require.Equal(t, providerregistry.OwnerDisabled, modes["native"])
}

func TestParseProviderRolloutPolicyRejectsUnknownProfile(t *testing.T) {
	t.Setenv("CRUX_PROVIDER_PROFILE", "surprise")
	_, err := parseProviderRolloutPolicy()
	require.EqualError(t, err, `unknown provider rollout profile "surprise"`)
}

func TestCompiledProviderProfileIsACeiling(t *testing.T) {
	previous := DefaultProviderProfile
	DefaultProviderProfile = string(ProviderProfileCoreOnly)
	t.Cleanup(func() { DefaultProviderProfile = previous })
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfilePluginNative))
	_, err := parseProviderRolloutPolicy()
	require.EqualError(t, err, `provider rollout profile "plugin-native" exceeds compiled release profile "core-only"`)
}

func TestProviderBehaviorCapabilitiesPreserveInactiveIntegratedBehavior(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfileCoreOnly))

	_, active := ProviderCapabilities().Lookup("codex")
	require.False(t, active)
	registration, ok := ProviderBehaviorCapabilities("codex")
	require.True(t, ok)
	require.NotNil(t, registration.Runtime)
	require.NotNil(t, registration.Instructions)
	require.NotNil(t, registration.Images)
}

func TestProviders_Integration_AutoUpdateDisabled(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	originalCatwalkSyncer := catwalkSyncer
	defer func() {
		catwalkSyncer = originalCatwalkSyncer
	}()
	catwalkSyncer = &catwalkSync{}

	resetProviderState()
	defer resetProviderState()

	providers, err := Providers(&Config{
		Options: &Options{DisableProviderAutoUpdate: true},
	})
	require.NoError(t, err)
	require.NotNil(t, providers)
	require.Greater(t, len(providers), 3, "expected embedded providers")
}

func TestCache_StoreAndGet(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/test.json"

	cache := newCache[[]catwalk.Provider](cachePath)

	providers := []catwalk.Provider{
		{Name: "Provider1", ID: "p1"},
		{Name: "Provider2", ID: "p2"},
	}

	err := cache.Store(providers)
	require.NoError(t, err)

	result, etag, err := cache.Get()
	require.NoError(t, err)
	require.Len(t, result, 2)
	require.Equal(t, "Provider1", result[0].Name)
	require.NotEmpty(t, etag)
}

func TestCache_GetNonExistent(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/nonexistent.json"

	cache := newCache[[]catwalk.Provider](cachePath)

	_, _, err := cache.Get()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to read provider cache file")
}

func TestCache_GetInvalidJSON(t *testing.T) {
	t.Parallel()

	tmpDir := t.TempDir()
	cachePath := tmpDir + "/invalid.json"

	require.NoError(t, os.WriteFile(cachePath, []byte("invalid json"), 0o644))

	cache := newCache[[]catwalk.Provider](cachePath)

	_, _, err := cache.Get()
	require.Error(t, err)
	require.Contains(t, err.Error(), "failed to unmarshal provider data from cache")
}

func TestCachePathFor(t *testing.T) {
	tests := []struct {
		name        string
		xdgDataHome string
		expected    string
	}{
		{
			name:        "with XDG_DATA_HOME",
			xdgDataHome: "/custom/data",
			expected:    "/custom/data/crux/providers.json",
		},
		{
			name:        "without XDG_DATA_HOME",
			xdgDataHome: "",
			expected:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.xdgDataHome != "" {
				t.Setenv("XDG_DATA_HOME", tt.xdgDataHome)
			} else {
				t.Setenv("XDG_DATA_HOME", "")
			}

			result := cachePathFor("providers")
			if tt.expected != "" {
				require.Equal(t, tt.expected, filepath.ToSlash(result))
			} else {
				require.Contains(t, result, "crux")
				require.Contains(t, result, "providers.json")
			}
		})
	}
}

func TestProvidersUsesCachedCatalog(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cached := []catwalk.Provider{{Name: "Provider1", ID: "p1", Type: catwalk.TypeOpenAICompat}}
	require.NoError(t, newCache[[]catwalk.Provider](cachePathFor("providers")).Store(cached))

	resetProviderState()
	defer resetProviderState()

	providers, err := Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	require.Len(t, providers, 1, "plugin-compat must not add Codex or Gemini without their plugins")
	require.Equal(t, catwalk.InferenceProvider("p1"), providers[0].ID)
}

func TestProviders_HonorsDisableDefaultProviders(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	resetProviderState()
	defer resetProviderState()

	providers, err := Providers(&Config{
		Options: &Options{DisableDefaultProviders: true},
	})
	require.NoError(t, err)
	require.Empty(t, providers)
}

func TestCacheStore_ReplacesFileInsteadOfRewritingIt(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "providers.json")
	c := newCache[[]catwalk.Provider](path)

	require.NoError(t, c.Store([]catwalk.Provider{{ID: "first", Name: "First"}}))
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, c.Store([]catwalk.Provider{{ID: "second", Name: "Second"}}))
	after, err := os.Stat(path)
	require.NoError(t, err)

	if runtime.GOOS != "windows" {
		require.False(t, os.SameFile(before, after),
			"the cache should be replaced by a rename, not rewritten in place")
	}

	got, _, err := c.Get()
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, catwalk.InferenceProvider("second"), got[0].ID)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "only the cache file should remain")
	require.Equal(t, "providers.json", entries[0].Name())
}
