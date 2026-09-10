package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

func TestRemoteImageDependencyStaysUnloadedDuringStartup(t *testing.T) {
	root := t.TempDir()
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "test.startup-images", Version: "1.0.0", Name: "Startup images", Description: "Synthetic image dependency",
		Publisher: manifest.Publisher{ID: "test", Name: "Test"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "startup-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Credentials: []manifest.ImageCredential{{ID: "access", Source: "provider", Provider: "codex"}},
		Origins:     []manifest.ImageOrigin{{URL: "https://images.example.invalid", Credentials: []string{"access"}}},
		Models:      []manifest.ImageModel{{ID: "model", Name: "Model"}}, DefaultModel: "model",
		Options:  manifest.ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:   manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 10},
		Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {Steps: []manifest.ImageStep{{ID: "value", Value: &manifest.ImageValue{Object: map[string]manifest.ImageValue{}}}}, Result: manifest.ImageValue{Array: []manifest.ImageValue{}}}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source := filepath.Join(root, "source")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	bundles, err := manager.ExportRegisteredBundles(manager.Snapshot().Revision, map[string]string{owner.PluginID: owner.Digest})
	require.NoError(t, err)
	credential := providerregistry.RegistrationOwner{ProviderID: "codex", Construction: providerregistry.ConstructionCodex}
	proposal := RemoteRuntimeProposal{Version: RemoteRuntimeVersion, Revision: 1, Bundles: bundles,
		Providers: []RemoteProviderDefinition{{Config: ProviderConfig{ID: "codex"}, Unloaded: &ProviderLoadIssue{ProviderID: "codex", Message: "Provider bundle requires an update."}}},
		Images:    &ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}, Providers: map[string]ImageProviderConfiguration{owner.Backend: {Owner: owner, Credentials: map[string]providerregistry.RegistrationOwner{"access": credential}}}},
	}
	proposal.Digest, err = RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	remote, err := CompileRemoteRuntime(filepath.Join(root, "remote"), filepath.Join(root, "remote-data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root}))
	require.NoError(t, err)
	require.Equal(t, proposal.Images, remote.Config().Images)
	_, handled, err := remote.RuntimeSnapshot().ClientImageBundle(owner)
	require.True(t, handled, "an unloaded client dependency must never use a server bundle")
	require.ErrorContains(t, err, "requires unloaded provider")
}

func TestImageConfigurationExactOwnerAndIsolation(t *testing.T) {
	owner := providerplugin.ImageOwner{Backend: "fixture-images", PluginID: "fixture.images", Version: "1.0.0", Digest: strings.Repeat("a", 64)}
	value := &ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}, Providers: map[string]ImageProviderConfiguration{owner.Backend: {Owner: owner, Configuration: map[string]any{"nested": map[string]any{"value": "private-image-setting"}}, Credentials: map[string]providerregistry.RegistrationOwner{"access": {ProviderID: "fixture", Construction: providerregistry.ConstructionOpenAICompat}}}}}
	require.NoError(t, value.Validate())
	original := &Config{Images: value}
	clone := original.cloneForWrite()
	clone.Images.Providers[owner.Backend].Configuration["nested"].(map[string]any)["value"] = "changed"
	clone.Images.Preferred[0].Version = "2.0.0"
	require.Equal(t, "private-image-setting", value.Providers[owner.Backend].Configuration["nested"].(map[string]any)["value"])
	require.Equal(t, owner, value.Preferred[0])
	public := original.RedactedForTransport()
	data, err := json.Marshal(public)
	require.NoError(t, err)
	require.NotContains(t, string(data), "private-image-setting")
	require.NotContains(t, string(data), "credentials")
	require.Equal(t, owner, public.Images.Providers[owner.Backend].Owner)
	for _, mutate := range []func(*ImageConfiguration){
		func(v *ImageConfiguration) { v.Preferred[0].Digest = "invalid" },
		func(v *ImageConfiguration) { v.Preferred = append(v.Preferred, owner) },
		func(v *ImageConfiguration) { v.Preferred[0].Version = "2.0.0" },
		func(v *ImageConfiguration) {
			p := v.Providers[owner.Backend]
			p.Owner.Backend = "other"
			v.Providers[owner.Backend] = p
		},
	} {
		invalid := cloneImageConfiguration(value)
		mutate(invalid)
		require.Error(t, invalid.Validate())
	}
}

func TestImageSetupConfigurationRejectsConcurrentReplacement(t *testing.T) {
	owner := providerplugin.ImageOwner{Backend: "fixture-images", PluginID: "fixture.images", Version: "1.0.0", Digest: strings.Repeat("a", 64)}
	current := &ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}}
	store := NewTestStore(&Config{Images: current})
	writes := 0
	store.writeFields = func(Scope, map[string]any) error { writes++; return nil }
	require.ErrorContains(t, store.CompareAndSetImageConfiguration(nil, current), "changed during setup")
	require.Zero(t, writes)
	require.Equal(t, current, store.ImageConfiguration())
	next := cloneImageConfiguration(current)
	next.Preferred[0].Version = "2.0.0"
	require.NoError(t, store.CompareAndSetImageConfiguration(current, next))
	require.Equal(t, 1, writes)
	next.Preferred[0].Version = "3.0.0"
	require.Equal(t, "2.0.0", store.ImageConfiguration().Preferred[0].Version)
}

func TestImagePluginPathsUseCapturedHost(t *testing.T) {
	root := t.TempDir()
	store := NewTestStore(&Config{})
	store.baseEnvironment = env.NewFromMap(map[string]string{"HOME": root, "CRUX_GLOBAL_DATA": filepath.Join(root, "host-data"), "CRUX_CACHE_DIR": filepath.Join(root, "host-cache")})
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "wrong-data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "wrong-cache"))
	paths, err := store.PluginPaths()
	require.NoError(t, err)
	require.Equal(t, providerplugin.DefaultPaths(filepath.Join(root, "host-data"), filepath.Join(root, "host-cache")), paths)
	store.baseEnvironment = nil
	_, err = store.PluginPaths()
	require.Error(t, err)
}
