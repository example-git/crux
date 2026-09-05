package config

import (
	"path/filepath"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/codex"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type rolloutScanOwner struct {
	mode          providerregistry.OwnerMode
	construction  providerregistry.Construction
	compatibility providerregistry.Construction
	manifestID    string
}

func requireRolloutScanOwner(t *testing.T, scan ProviderScan, providerID string, expected rolloutScanOwner) {
	t.Helper()
	require.NotNil(t, scan.Registry)
	_, cataloged := lookupProvider(scan.Providers, providerID)
	registration, registered := scan.Registry.Lookup(providerID)
	require.Equal(t, expected.mode, scan.ownerModes[providerID])
	if expected.mode == providerregistry.OwnerDisabled {
		require.False(t, cataloged)
		require.False(t, registered)
		return
	}
	require.True(t, cataloged)
	require.True(t, registered)
	require.Equal(t, expected.construction, registration.Construction)
	require.Equal(t, expected.compatibility, registration.CompatibilityAdapter)
	if expected.manifestID == "" {
		require.Nil(t, registration.Manifest)
		return
	}
	require.NotNil(t, registration.Manifest)
	require.Equal(t, expected.manifestID, registration.Manifest.ID)
}

func TestFreshProviderScanProtectedAndOrdinaryRolloutMatrix(t *testing.T) {
	codexCore := rolloutScanOwner{mode: providerregistry.OwnerIntegrated, construction: providerregistry.ConstructionCodex}
	geminiCore := rolloutScanOwner{mode: providerregistry.OwnerIntegrated, construction: providerregistry.ConstructionGeminiAntigravity}
	copilotCore := rolloutScanOwner{mode: providerregistry.OwnerIntegrated, construction: providerregistry.ConstructionCopilot}
	codexCompat := rolloutScanOwner{mode: providerregistry.OwnerPluginCompat, construction: providerregistry.ConstructionCodex, compatibility: providerregistry.ConstructionCodex, manifestID: "test.claim." + codex.ID}
	geminiCompat := rolloutScanOwner{mode: providerregistry.OwnerPluginCompat, construction: providerregistry.ConstructionGeminiAntigravity, compatibility: providerregistry.ConstructionGeminiAntigravity, manifestID: "test.claim." + gemini.ID}
	codexNative := rolloutScanOwner{mode: providerregistry.OwnerPluginNative, construction: providerregistry.ConstructionGenericJSON, manifestID: "test.claim." + codex.ID}
	geminiNative := rolloutScanOwner{mode: providerregistry.OwnerPluginNative, construction: providerregistry.ConstructionGenericJSON, manifestID: "test.claim." + gemini.ID}
	ordinaryNative := rolloutScanOwner{mode: providerregistry.OwnerPluginNative, construction: providerregistry.ConstructionGenericJSON, manifestID: "example.echo"}
	disabled := rolloutScanOwner{mode: providerregistry.OwnerDisabled}

	for _, test := range []struct {
		name          string
		profile       ProviderProfile
		allowlist     string
		compatibility bool
		codex         rolloutScanOwner
		gemini        rolloutScanOwner
		ordinary      rolloutScanOwner
	}{
		{name: "core only", profile: ProviderProfileCoreOnly, compatibility: true, codex: disabled, gemini: disabled, ordinary: disabled},
		{name: "core only allow plugins", profile: ProviderProfileCoreOnly, allowlist: string(catalog.ProviderCopilot) + "," + codex.ID + "," + gemini.ID + ",example-echo", compatibility: true, codex: disabled, gemini: disabled, ordinary: disabled},
		{name: "integrated", profile: ProviderProfileIntegrated, compatibility: true, codex: codexCore, gemini: geminiCore, ordinary: disabled},
		{name: "integrated allow plugins", profile: ProviderProfileIntegrated, allowlist: string(catalog.ProviderCopilot) + "," + codex.ID + "," + gemini.ID + ",example-echo", compatibility: true, codex: codexCore, gemini: geminiCore, ordinary: disabled},
		{name: "plugin compat all compatibility", profile: ProviderProfilePluginCompat, compatibility: true, codex: codexCompat, gemini: geminiCompat, ordinary: ordinaryNative},
		{name: "plugin compat allow copilot", profile: ProviderProfilePluginCompat, allowlist: string(catalog.ProviderCopilot), compatibility: true, codex: disabled, gemini: disabled, ordinary: disabled},
		{name: "plugin compat allow codex", profile: ProviderProfilePluginCompat, allowlist: codex.ID, compatibility: true, codex: codexCompat, gemini: disabled, ordinary: disabled},
		{name: "plugin compat allow gemini", profile: ProviderProfilePluginCompat, allowlist: gemini.ID, compatibility: true, codex: disabled, gemini: geminiCompat, ordinary: disabled},
		{name: "plugin compat allow ordinary", profile: ProviderProfilePluginCompat, allowlist: "example-echo", compatibility: true, codex: disabled, gemini: disabled, ordinary: ordinaryNative},
		{name: "plugin compat all native", profile: ProviderProfilePluginCompat, codex: codexNative, gemini: geminiNative, ordinary: ordinaryNative},
		{name: "plugin native rejects compatibility", profile: ProviderProfilePluginNative, compatibility: true, codex: disabled, gemini: disabled, ordinary: ordinaryNative},
		{name: "plugin native all native", profile: ProviderProfilePluginNative, codex: codexNative, gemini: geminiNative, ordinary: ordinaryNative},
		{name: "plugin native allow copilot", profile: ProviderProfilePluginNative, allowlist: string(catalog.ProviderCopilot), codex: disabled, gemini: disabled, ordinary: disabled},
		{name: "plugin native allow codex", profile: ProviderProfilePluginNative, allowlist: codex.ID, codex: codexNative, gemini: disabled, ordinary: disabled},
		{name: "plugin native allow gemini", profile: ProviderProfilePluginNative, allowlist: gemini.ID, codex: disabled, gemini: geminiNative, ordinary: disabled},
		{name: "plugin native allow ordinary", profile: ProviderProfilePluginNative, allowlist: "example-echo", codex: disabled, gemini: disabled, ordinary: ordinaryNative},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			dataRoot := filepath.Join(root, "data")
			cacheRoot := filepath.Join(root, "cache")
			t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
			t.Setenv("CRUX_CACHE_DIR", cacheRoot)
			t.Setenv("CRUX_PROVIDER_PROFILE", string(test.profile))
			t.Setenv("CRUX_PROVIDER_PLUGINS", test.allowlist)
			t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")

			copilotBundle := providerClaimBundle(t, root, string(catalog.ProviderCopilot))
			installTrustedProviderBundle(t, dataRoot, cacheRoot, copilotBundle)
			codexBundle := providerClaimBundle(t, root, codex.ID)
			geminiBundle := providerClaimBundle(t, root, gemini.ID)
			if test.compatibility {
				codexBundle = providerCompatibilityClaimBundle(t, codexBundle, accounts.ProviderCodex, providerregistry.ConstructionCodex)
				geminiBundle = providerCompatibilityClaimBundle(t, geminiBundle, accounts.ProviderGemini, providerregistry.ConstructionGeminiAntigravity)
			}
			installTrustedProviderBundle(t, dataRoot, cacheRoot, codexBundle)
			installTrustedProviderBundle(t, dataRoot, cacheRoot, geminiBundle)
			ordinaryBundle, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
			require.NoError(t, err)
			installTrustedProviderBundle(t, dataRoot, cacheRoot, ordinaryBundle)

			scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
			require.ErrorContains(t, err, `provider plugin claim "copilot" conflicts with a core provider`)
			requireRolloutScanOwner(t, scan, string(catalog.ProviderCopilot), copilotCore)
			requireRolloutScanOwner(t, scan, codex.ID, test.codex)
			requireRolloutScanOwner(t, scan, gemini.ID, test.gemini)
			requireRolloutScanOwner(t, scan, "example-echo", test.ordinary)
		})
	}
}

func TestFreshProviderScanMigratedPresetRolloutMatrix(t *testing.T) {
	profiles := []ProviderProfile{
		ProviderProfileCoreOnly,
		ProviderProfileIntegrated,
		ProviderProfilePluginCompat,
		ProviderProfilePluginNative,
	}
	allowlists := []struct {
		name  string
		value string
	}{
		{name: "empty"},
		{name: "include", value: "deepseek"},
		{name: "deny", value: "other"},
	}
	for _, profile := range profiles {
		for _, allowlist := range allowlists {
			t.Run(string(profile)+" "+allowlist.name, func(t *testing.T) {
				root := t.TempDir()
				dataRoot := filepath.Join(root, "data")
				cacheRoot := filepath.Join(root, "cache")
				t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
				t.Setenv("CRUX_CACHE_DIR", cacheRoot)
				t.Setenv("CRUX_PROVIDER_PROFILE", string(profile))
				t.Setenv("CRUX_PROVIDER_PLUGINS", allowlist.value)
				t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")

				presetBundle, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin"))
				require.NoError(t, err)
				installTrustedProviderBundle(t, dataRoot, cacheRoot, presetBundle)
				installTrustedProviderBundle(t, dataRoot, cacheRoot, providerClaimBundle(t, root, "deepseek"))

				cfg := &Config{Options: &Options{}}
				scan, err := FreshProviderScan(t.Context(), cfg)
				require.NoError(t, err)
				presetStatus := scan.pluginStatuses["crux.catwalk.deepseek"]
				require.Equal(t, providerplugin.StateRegistered, presetStatus.State)
				claimStatus := scan.pluginStatuses["test.claim.deepseek"]
				require.Equal(t, providerplugin.StateQuarantined, claimStatus.State)
				require.Contains(t, claimStatus.Diagnostics, providerplugin.Diagnostic{Code: "migrated-provider-plugin-conflict", Message: "provider plugin cannot claim an ID reserved for its canonical migrated preset"})
				reference, active := scan.presetReferences["deepseek"]
				require.True(t, active)
				_, cataloged := lookupProvider(scan.Providers, "deepseek")
				require.True(t, cataloged)
				_, registered := scan.Registry.Lookup("deepseek")
				require.False(t, registered)

				provider := ProviderConfig{
					ID: "deepseek", Owner: providerPresetOwnerReference(),
					Preset: &ProviderPresetReference{ID: reference.ID, Version: reference.Version, Digest: reference.Digest},
				}
				cfg.Providers = csync.NewMapFrom(map[string]ProviderConfig{"deepseek": provider})
				cfg.bindProviderScan(scan)
				snapshot := RuntimeSnapshot{config: cfg, registry: scan.Registry}
				owner, owned := snapshot.ProviderOwner("deepseek")
				require.True(t, owned)
				require.Equal(t, providerregistry.RegistrationOwner{
					ProviderID: "deepseek", HasPreset: true,
					PresetID: reference.ID, PresetVersion: reference.Version, PresetDigest: reference.Digest,
				}, owner)
				require.True(t, cfg.IsProviderIntegrationAvailable("deepseek"))
				resolved, registration, constructionRegistered, err := snapshot.ProviderForConstruction("deepseek", provider)
				require.NoError(t, err)
				require.Equal(t, "deepseek", resolved.ID)
				require.Empty(t, registration.ProviderID)
				require.False(t, constructionRegistered)
			})
		}
	}
}

func TestProvidersPublishesCanonicalMigratedPresetAgainstSameIDPlugin(t *testing.T) {
	resetProviderState()
	t.Cleanup(resetProviderState)
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	cacheRoot := filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfilePluginCompat))
	t.Setenv("CRUX_PROVIDER_PLUGINS", "deepseek")
	t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")

	presetBundle, err := filepath.Abs(filepath.Join("..", "..", "plugins", "provider-presets", "deepseek.plugin"))
	require.NoError(t, err)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, presetBundle)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, providerClaimBundle(t, root, "deepseek"))

	providers, err := Providers(&Config{Options: &Options{}})
	require.NoError(t, err)
	_, cataloged := lookupProvider(providers, "deepseek")
	require.True(t, cataloged)
	reference, active := ActiveProviderPreset("deepseek")
	require.True(t, active)
	require.True(t, providerplugin.IsCanonicalMigratedProviderPresetBundle("deepseek", reference.ID, reference.Version, reference.Digest))
	_, registered := ProviderRegistry().Lookup("deepseek")
	require.False(t, registered)
	status, _, found := ProviderPluginAvailability("test.claim.deepseek")
	require.True(t, found)
	require.Equal(t, providerplugin.StateQuarantined, status.State)
	require.Contains(t, status.Diagnostics, providerplugin.Diagnostic{Code: "migrated-provider-plugin-conflict", Message: "provider plugin cannot claim an ID reserved for its canonical migrated preset"})
}

func TestFreshProviderScanCustomOwnerRolloutMatrix(t *testing.T) {
	profiles := []ProviderProfile{
		ProviderProfileCoreOnly,
		ProviderProfileIntegrated,
		ProviderProfilePluginCompat,
		ProviderProfilePluginNative,
	}
	allowlists := []struct {
		name  string
		value string
	}{
		{name: "empty"},
		{name: "include", value: "example-echo"},
		{name: "deny", value: "other"},
	}
	for _, profile := range profiles {
		for _, allowlist := range allowlists {
			t.Run(string(profile)+" "+allowlist.name, func(t *testing.T) {
				root := t.TempDir()
				dataRoot := filepath.Join(root, "data")
				cacheRoot := filepath.Join(root, "cache")
				t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
				t.Setenv("CRUX_CACHE_DIR", cacheRoot)
				t.Setenv("CRUX_PROVIDER_PROFILE", string(profile))
				t.Setenv("CRUX_PROVIDER_PLUGINS", allowlist.value)
				t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")
				bundle, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin"))
				require.NoError(t, err)
				installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)

				provider := ProviderConfig{
					ID: "example-echo", Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat},
					Type: catalog.TypeOpenAICompat, BaseURL: "https://custom.example.invalid/v1",
					Models: []catalog.Model{{ID: "custom-model"}},
				}
				cfg := &Config{Options: &Options{}, Providers: csync.NewMapFrom(map[string]ProviderConfig{"example-echo": provider})}
				scan, err := FreshProviderScan(t.Context(), cfg)
				require.NoError(t, err)
				_, cataloged := lookupProvider(scan.Providers, "example-echo")
				require.False(t, cataloged)
				_, registered := scan.Registry.Lookup("example-echo")
				require.False(t, registered)

				cfg.bindProviderScan(scan)
				snapshot := RuntimeSnapshot{config: cfg, registry: scan.Registry}
				owner, owned := snapshot.ProviderOwner("example-echo")
				require.True(t, owned)
				require.Equal(t, providerregistry.RegistrationOwner{ProviderID: "example-echo"}, owner)
				require.True(t, cfg.IsProviderIntegrationAvailable("example-echo"))
				resolved, registration, constructionRegistered, err := snapshot.ProviderForConstruction("example-echo", provider)
				require.NoError(t, err)
				require.Equal(t, "example-echo", resolved.ID)
				require.Empty(t, registration.ProviderID)
				require.False(t, constructionRegistered)
			})
		}
	}
}
