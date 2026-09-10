package config_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

// Production discovery contains registered integrations/plugins and installed
// presets, not an ownerless base catalog. Preview metadata must not become a
// substitute credential authority merely because its model rows are visible.
type catalogOwnerFixture struct {
	root, workspace, sourceConfig, workspaceConfig, globalConfig string
	base                                                         env.Env
}

func newCatalogOwnerFixture(t *testing.T, profile config.ProviderProfile, allowlist string, document *config.Config, installPreset bool) catalogOwnerFixture {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "XDG_CONFIG_HOME": filepath.Join(root, "xdg"),
		"AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"),
		"CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"),
		"CRUX_PROVIDER_PROFILE": string(profile), "CRUX_PROVIDER_PLUGINS": allowlist,
		"CRUX_PROVIDER_PLUGIN_COMPAT": "", "DEEPSEEK_API_KEY": "", "CRUX_DISABLE_AUTO_MEMORY": "true",
	}
	for key, value := range values {
		t.Setenv(key, value)
	}
	f := catalogOwnerFixture{root: root, workspace: filepath.Join(root, "workspace"),
		sourceConfig: filepath.Join(root, "workspace", "crux.json"), workspaceConfig: filepath.Join(root, "workspace-data", "crux.json"),
		globalConfig: filepath.Join(root, "data", "crux.json"), base: env.NewFromMap(values)}
	require.NoError(t, os.MkdirAll(f.workspace, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Dir(f.workspaceConfig), 0o700))
	require.NoError(t, os.WriteFile(f.workspaceConfig, []byte("{}"), 0o600))
	if document == nil {
		document = &config.Config{}
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.sourceConfig, data, 0o600))

	// The checked-in minimal plugin is anonymous. Require a declared API key so
	// LoadIsolated must leave this installed plugin unconfigured until key entry.
	data, err = os.ReadFile("../../docs/provider-plugins/examples/minimal.plugin/manifest.json")
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	declaration.Capabilities.Credentials = []manifest.Credential{{ID: "access", Kind: "api-key", Audience: []string{"api"}}}
	declaration.Capabilities.Endpoints[0].Credential = "access"
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	plugin := filepath.Join(root, "api-key.plugin")
	require.NoError(t, os.MkdirAll(plugin, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(plugin, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	defer manager.Close()
	sources := []string{plugin}
	if installPreset {
		preset, err := filepath.Abs("../../plugins/provider-presets/deepseek.plugin")
		require.NoError(t, err)
		sources = append(sources, preset)
	}
	for _, source := range sources {
		_, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
		require.NoError(t, err)
	}
	return f
}

func (f catalogOwnerFixture) load(t *testing.T) *config.ConfigStore {
	t.Helper()
	store, err := config.LoadIsolated(f.workspace, filepath.Join(f.root, "workspace-data"), false, f.base)
	require.NoError(t, err)
	return store
}

func catalogOwnerStatuses(t *testing.T, service *providerauth.Service) providerauth.Snapshot {
	t.Helper()
	snapshot, err := service.Status(t.Context())
	require.NoError(t, err)
	require.NoError(t, snapshot.Validate())
	return snapshot
}

func catalogOwnerStatus(t *testing.T, snapshot providerauth.Snapshot, providerID string) providerauth.Status {
	t.Helper()
	for _, status := range snapshot.Providers {
		if status.Owner.ProviderID == providerID {
			return status
		}
	}
	t.Fatalf("provider %q has no authentication status", providerID)
	return providerauth.Status{}
}

func catalogOwnerRegistrations(cfg *config.Config) []providerregistry.RegistrationOwner {
	var owners []providerregistry.RegistrationOwner
	for _, registration := range cfg.ProviderRegistrations() {
		owners = append(owners, registration.Owner())
	}
	slices.SortFunc(owners, func(a, b providerregistry.RegistrationOwner) int { return strings.Compare(a.ProviderID, b.ProviderID) })
	return owners
}

func TestCatalogOwnerInvariantProductionProfiles(t *testing.T) {
	for _, test := range []struct {
		name               string
		profile            config.ProviderProfile
		allowlist          string
		plugin, integrated bool
	}{
		{"core", config.ProviderProfileCoreOnly, "", false, false},
		{"integrated", config.ProviderProfileIntegrated, "", false, false},
		{"compat", config.ProviderProfilePluginCompat, "", true, false},
		{"native", config.ProviderProfilePluginNative, "", true, false},
		{"filtered plugin", config.ProviderProfilePluginNative, "other-provider", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := newCatalogOwnerFixture(t, test.profile, test.allowlist, nil, true).load(t)
			service := providerauth.New(store, "catalog-owner-fixture")
			before := store.RuntimeSnapshot()
			registrations := catalogOwnerRegistrations(store.Config())
			status := catalogOwnerStatuses(t, service)
			surfaces := config.ProviderSurfaces(store.Config())
			catalogIDs := map[string]bool{}
			for _, provider := range store.KnownProviders() {
				id := string(provider.ID)
				catalogIDs[id] = true
				owner, ok := before.ProviderOwner(id)
				require.True(t, ok, "production catalog row %q needs its actual captured owner", id)
				surface, ok := providerregistry.LookupSurface(surfaces, id)
				require.True(t, ok)
				require.NotNil(t, surface.Owner)
				require.Equal(t, owner, *surface.Owner)
				require.True(t, surface.Available)
				public := catalogOwnerStatus(t, status, id)
				require.Equal(t, providerauth.PublicOwner(owner), public.Owner)
				require.False(t, public.Configured)
				require.False(t, public.Disabled)
			}
			require.True(t, catalogIDs["copilot"])
			require.True(t, catalogIDs["deepseek"], "installed presets retain their independent profile behavior")
			require.Equal(t, test.plugin, catalogIDs["example-echo"])
			require.Equal(t, test.integrated, catalogIDs["codex"])
			require.Equal(t, test.integrated, catalogIDs["gemini-ag"])
			require.Len(t, catalogIDs, 2+2*catalogOwnerBoolCount(test.integrated)+catalogOwnerBoolCount(test.plugin), "no implicit base catalog may appear")
			_, registered := store.ProviderRegistration("deepseek")
			require.False(t, registered, "a preset is not an executable registration")
			require.Equal(t, registrations, catalogOwnerRegistrations(store.Config()))
			require.True(t, before.SamePublication(store.RuntimeSnapshot()), "status and surface reads must not publish")
		})
	}
}

func catalogOwnerBoolCount(value bool) int {
	if value {
		return 1
	}
	return 0
}

func TestCatalogOwnerInvariantUnconfiguredCredentialCommit(t *testing.T) {
	for _, providerID := range []string{"deepseek", "example-echo"} {
		for _, scope := range []config.Scope{config.ScopeWorkspace, config.ScopeGlobal} {
			t.Run(providerID+" "+scope.String(), func(t *testing.T) {
				fixture := newCatalogOwnerFixture(t, config.ProviderProfilePluginNative, "", nil, true)
				store := fixture.load(t)
				service := providerauth.New(store, "catalog-owner-fixture")
				before := catalogOwnerStatuses(t, service)
				status := catalogOwnerStatus(t, before, providerID)
				require.False(t, status.Configured)
				owner, ok := store.RuntimeSnapshot().ProviderOwner(providerID)
				require.True(t, ok)
				registrations := catalogOwnerRegistrations(store.Config())
				selected := store.Config().Models
				sourceBefore, err := os.ReadFile(fixture.sourceConfig)
				require.NoError(t, err)
				workspaceBefore, err := os.ReadFile(fixture.workspaceConfig)
				require.NoError(t, err)
				globalBefore, globalErr := os.ReadFile(fixture.globalConfig)
				require.True(t, globalErr == nil || os.IsNotExist(globalErr))
				key := "synthetic-catalog-owner-key"
				require.NoError(t, store.SetProviderAPIKey(scope, providerID, config.ProviderAPIKeyCredential{Owner: owner, APIKey: key}))
				provider, configured := store.Config().Providers.Get(providerID)
				require.True(t, configured)
				require.Equal(t, key, provider.APIKey)
				current, ok := store.RuntimeSnapshot().ProviderOwner(providerID)
				require.True(t, ok)
				require.Equal(t, owner, current)
				surface, ok := providerregistry.LookupSurface(config.ProviderSurfaces(store.Config()), providerID)
				require.True(t, ok)
				require.Equal(t, owner, *surface.Owner)
				after := catalogOwnerStatuses(t, service)
				accepted := catalogOwnerStatus(t, after, providerID)
				require.Equal(t, status.Owner, accepted.Owner)
				require.True(t, accepted.Configured)
				require.Contains(t, accepted.Credentials, providerauth.CredentialStatus{Kind: "api-key", State: "configured"})
				require.Greater(t, after.Generation.Sequence, before.Generation.Sequence)
				require.Equal(t, registrations, catalogOwnerRegistrations(store.Config()))
				require.Equal(t, selected, store.Config().Models, "credential entry does not choose a model")
				path := fixture.workspaceConfig
				if scope == config.ScopeGlobal {
					path = fixture.globalConfig
				}
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				var persisted config.Config
				require.NoError(t, json.Unmarshal(data, &persisted))
				require.NotNil(t, persisted.Providers)
				written, ok := persisted.Providers.Get(providerID)
				require.True(t, ok)
				require.Equal(t, key, written.APIKey)
				require.Equal(t, provider.Owner, written.Owner)
				require.Equal(t, provider.Preset, written.Preset)
				require.Equal(t, provider.Plugin, written.Plugin)
				if scope == config.ScopeGlobal {
					unchanged, err := os.ReadFile(fixture.workspaceConfig)
					require.NoError(t, err)
					require.Equal(t, workspaceBefore, unchanged)
				} else {
					unchanged, err := os.ReadFile(fixture.globalConfig)
					if os.IsNotExist(globalErr) {
						require.True(t, os.IsNotExist(err))
					} else {
						require.NoError(t, err)
						require.Equal(t, globalBefore, unchanged)
					}
				}
				sourceAfter, err := os.ReadFile(fixture.sourceConfig)
				require.NoError(t, err)
				require.Equal(t, sourceBefore, sourceAfter, "credential entry must not rewrite the project source")
				public, err := json.Marshal(after)
				require.NoError(t, err)
				require.NotContains(t, string(public), key)
				require.NotContains(t, string(public), "account_namespace")
			})
		}
	}
}

func TestCatalogOwnerInvariantRejectsOwnerlessMetadataAndUnavailableOwners(t *testing.T) {
	for _, test := range []struct {
		name, id                                   string
		profile                                    config.ProviderProfile
		allowlist                                  string
		installPreset, disableDefaults, configured bool
		digest                                     string
	}{
		{name: "unknown", id: "unknown", profile: config.ProviderProfilePluginNative, installPreset: true},
		{name: "profile disabled plugin", id: "example-echo", profile: config.ProviderProfileCoreOnly, installPreset: true},
		{name: "allowlist disabled plugin", id: "example-echo", profile: config.ProviderProfilePluginNative, allowlist: "other", installPreset: true},
		{name: "default disabled", id: "deepseek", profile: config.ProviderProfilePluginNative, installPreset: true, disableDefaults: true},
		{name: "missing preset", id: "deepseek", profile: config.ProviderProfilePluginNative, configured: true, digest: strings.Repeat("a", 64)},
		{name: "mismatched preset", id: "deepseek", profile: config.ProviderProfilePluginNative, installPreset: true, configured: true, digest: strings.Repeat("a", 64)},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := &config.Config{Options: &config.Options{DisableDefaultProviders: test.disableDefaults}}
			if test.disableDefaults {
				document.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{"custom": {
					ID: "custom", Type: catalog.TypeOpenAICompat, BaseURL: "https://example.invalid/v1", APIKey: "synthetic-custom-key",
					Models: []catalog.Model{{ID: "static-model"}},
				}})
				document.Models = map[config.SelectedModelType]config.SelectedModel{
					config.SelectedModelTypeLarge: {Provider: "custom", Model: "static-model"},
					config.SelectedModelTypeSmall: {Provider: "custom", Model: "static-model"},
				}
			}
			if test.configured {
				document.Providers = csync.NewMapFrom(map[string]config.ProviderConfig{"deepseek": {ID: "deepseek", Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat}, Preset: &config.ProviderPresetReference{ID: "crux.catwalk.deepseek", Version: "0.51.23", Digest: test.digest}, Models: []catalog.Model{{ID: "retained"}}}})
			}
			fixture := newCatalogOwnerFixture(t, test.profile, test.allowlist, document, test.installPreset)
			store := fixture.load(t)
			_, owned := store.RuntimeSnapshot().ProviderOwner(test.id)
			require.False(t, owned)
			before := store.RuntimeSnapshot()
			status := catalogOwnerStatuses(t, providerauth.New(store, "catalog-owner-fixture"))
			for _, provider := range status.Providers {
				require.NotEqual(t, test.id, provider.Owner.ProviderID)
			}
			if surface, found := providerregistry.LookupSurface(config.ProviderSurfaces(store.Config()), test.id); found {
				require.Nil(t, surface.Owner)
				require.False(t, surface.Available)
			}
			bytesBefore, err := os.ReadFile(fixture.workspaceConfig)
			require.NoError(t, err)
			err = store.SetProviderAPIKey(config.ScopeWorkspace, test.id, config.ProviderAPIKeyCredential{Owner: providerregistry.RegistrationOwner{ProviderID: test.id}, APIKey: "synthetic-rejected"})
			require.Error(t, err)
			require.True(t, before.SamePublication(store.RuntimeSnapshot()))
			bytesAfter, err := os.ReadFile(fixture.workspaceConfig)
			require.NoError(t, err)
			require.Equal(t, bytesBefore, bytesAfter)
			if test.disableDefaults {
				require.Empty(t, store.KnownProviders())
				require.Empty(t, store.Config().ProviderRegistrations())
				custom, owned := store.RuntimeSnapshot().ProviderOwner("custom")
				require.True(t, owned)
				require.Equal(t, providerregistry.RegistrationOwner{ProviderID: "custom"}, custom)
				require.True(t, catalogOwnerStatus(t, status, "custom").Configured)
			}
		})
	}
	preview := &config.Config{Providers: csync.NewMap[string, config.ProviderConfig](), Options: &config.Options{}}
	preview.BindPreviewProviders([]catalog.Provider{{ID: "preview-only", Type: catalog.TypeOpenAICompat, APIEndpoint: "https://example.invalid", Models: []catalog.Model{{ID: "display-model"}}}})
	surface, found := providerregistry.LookupSurface(config.ProviderSurfaces(preview), "preview-only")
	require.True(t, found)
	require.Nil(t, surface.Owner)
	_, owned := preview.ProviderOwner("preview-only")
	require.False(t, owned)
	require.Empty(t, preview.ProviderRegistrations())
}
