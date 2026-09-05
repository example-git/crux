package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

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
