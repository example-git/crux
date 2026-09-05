package contracttest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

var canonicalMigratedProviderIDs = []string{
	"aihubmix",
	"alibaba-singapore",
	"alibaba-us",
	"atlascloud",
	"avian",
	"baseten",
	"cerebras",
	"chutes",
	"deepseek",
	"fireworks",
	"groq",
	"huggingface",
	"ionet",
	"moonshot",
	"nebius",
	"neuralwatt",
	"opencode-go",
	"opencode-zen",
	"qiniucloud",
	"scaleway",
	"synthetic",
	"venice",
	"xai",
	"zai",
	"zhipu",
	"zhipu-coding",
}

func TestProviderPresetCatalogMatchesCanonicalMigratedProviders(t *testing.T) {
	t.Parallel()
	root := presetCatalogRoot(t)
	paths, err := filepath.Glob(filepath.Join(root, "*.plugin", "manifest.json"))
	require.NoError(t, err)
	require.Len(t, paths, len(canonicalMigratedProviderIDs))

	dataRoot := filepath.Join(t.TempDir(), "data")
	cacheRoot := filepath.Join(t.TempDir(), "cache")
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataRoot, cacheRoot))
	require.NoError(t, err)
	defer manager.Close()

	seen := make(map[string]struct{}, len(canonicalMigratedProviderIDs))
	for _, providerID := range canonicalMigratedProviderIDs {
		path := filepath.Join(root, providerID+".plugin", "manifest.json")
		data, err := os.ReadFile(path)
		require.NoError(t, err, providerID)
		value, err := manifest.DecodePresetStrict(data)
		require.NoError(t, err, providerID)
		require.Equal(t, providerID, string(value.Preset.ID))
		require.Equal(t, foundation.ProviderTypeOpenAICompat, value.Preset.Type)
		require.NotEqual(t, catalog.ProviderCopilot, catalog.ProviderID(value.Preset.ID))

		presetID, version, digest, migrated := providerplugin.CanonicalMigratedProviderPreset(providerID)
		require.True(t, migrated, providerID)
		require.Equal(t, "crux.catwalk."+providerID, presetID)
		require.Equal(t, presetID, value.ID)
		require.Equal(t, version, value.Version)
		require.Len(t, digest, 64)

		snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{
			Source:           filepath.Dir(path),
			ExpectedRevision: manager.Snapshot().Revision,
		})
		require.NoError(t, err, providerID)
		index := slices.IndexFunc(snapshot.Plugins, func(status providerplugin.Status) bool {
			return status.ID == presetID
		})
		require.NotEqual(t, -1, index, providerID)
		status := snapshot.Plugins[index]
		require.Equal(t, providerID, status.ProviderID)
		require.Equal(t, version, status.Version)
		require.Equal(t, digest, status.Digest)

		_, duplicate := seen[providerID]
		require.False(t, duplicate, providerID)
		seen[providerID] = struct{}{}
	}
	require.Len(t, seen, len(canonicalMigratedProviderIDs))
	_, _, _, migrated := providerplugin.CanonicalMigratedProviderPreset(string(catalog.ProviderCopilot))
	require.False(t, migrated)
	_, err = os.Stat(filepath.Join(root, string(catalog.ProviderCopilot)+".plugin"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestAllCheckedInProviderBundlesValidateStrictly(t *testing.T) {
	t.Parallel()
	root := presetCatalogRoot(t)
	generated, err := filepath.Glob(filepath.Join(root, "*.plugin", "manifest.json"))
	require.NoError(t, err)
	examples, err := filepath.Glob(filepath.Join(root, "..", "..", "docs", "provider-plugins", "examples", "*.plugin", "manifest.json"))
	require.NoError(t, err)
	paths := append(generated, examples...)
	require.Len(t, paths, 29)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err, path)
		var header struct {
			PluginType string `json:"plugin_type"`
		}
		require.NoError(t, json.Unmarshal(data, &header), path)
		switch header.PluginType {
		case manifest.PluginTypeProviderPreset:
			_, err = manifest.DecodePresetStrict(data)
		case "", manifest.PluginTypeProvider:
			_, err = manifest.DecodeStrict(data)
		default:
			t.Fatalf("%s declares unsupported plugin type %q", path, header.PluginType)
		}
		require.NoError(t, err, path)
	}
}

func TestCoreCopilotCatalogIsFoundationOwned(t *testing.T) {
	t.Parallel()
	provider := copilot.CatalogProvider()
	require.Equal(t, "GitHub Copilot", provider.Name)
	require.Equal(t, catalog.ProviderCopilot, provider.ID)
	require.Equal(t, catalog.TypeOpenAICompat, provider.Type)
	require.Equal(t, "https://api.githubcopilot.com", provider.APIEndpoint)
	require.Empty(t, provider.APIKey)
	require.Empty(t, provider.DefaultHeaders)
	require.Len(t, provider.Models, 30)

	modelIDs := make(map[string]struct{}, len(provider.Models))
	for _, model := range provider.Models {
		require.NotEmpty(t, model.ID)
		require.NotEmpty(t, model.Name, model.ID)
		require.Positive(t, model.ContextWindow, model.ID)
		require.Positive(t, model.DefaultMaxTokens, model.ID)
		_, duplicate := modelIDs[model.ID]
		require.False(t, duplicate, model.ID)
		modelIDs[model.ID] = struct{}{}
	}
	require.Contains(t, modelIDs, provider.DefaultLargeModelID)
	require.Contains(t, modelIDs, provider.DefaultSmallModelID)
}

func presetCatalogRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	return filepath.Join(filepath.Dir(filepath.Dir(file)), "provider-presets")
}

func TestCanonicalMigratedProviderIDsAreSorted(t *testing.T) {
	t.Parallel()
	require.True(t, slices.IsSorted(canonicalMigratedProviderIDs))
	for _, providerID := range canonicalMigratedProviderIDs {
		require.Equal(t, providerID, strings.TrimSpace(providerID))
	}
}
