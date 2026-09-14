package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestRemoteAnthropicIdentityIsResolvedAndAdmittedFromClient(t *testing.T) {
	root := t.TempDir()
	dataRoot, cacheRoot := filepath.Join(root, "data"), filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CLAUDE_CODE_VERSION", "1.2.3")
	bundle := providerClaimBundle(t, root, "claude-ai")
	manifestPath := filepath.Join(bundle, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "capabilities.operations.0.protocol", string(providerregistry.ConstructionAnthropicMessages))
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "capabilities.operations.0.transport", "sse")
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "capabilities.operations.0.path", "/v1/messages")
	require.NoError(t, err)
	data, err = sjson.SetBytes(data, "capabilities.anthropic", manifest.AnthropicPolicy{
		ClientIdentity: &manifest.ResolvedClientIdentity{
			Environment: "CLAUDE_CODE_VERSION", LatestURL: "https://latest.example.invalid/version", CacheKey: "claude-code-test",
			FallbackVersion: "0.0.1", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "claude-cli/{version} ({os}; {arch})",
			ProbeTimeoutMS: 100, ProbeMaxBytes: 1024,
		},
		MaxRequestBytes: 1 << 20, TransformFailure: "error",
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0o600))
	installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.NoError(t, err)
	registration, ok := scan.Registry.Lookup("claude-ai")
	require.True(t, ok)
	metadata, ok := lookupProvider(scan.Providers, "claude-ai")
	require.True(t, ok)
	selected := SelectedModel{Provider: "claude-ai", Model: metadata.Models[0].ID}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: selected, SelectedModelTypeSmall: selected}}
	cfg.setDefaults(root, filepath.Join(root, "local"))
	cfg.Providers.Set("claude-ai", ProviderConfig{
		ID: "claude-ai", APIKey: "synthetic-key", BaseURL: metadata.APIEndpoint, Type: metadata.Type, Models: metadata.Models,
		Owner: providerOwnerReferenceForRegistration(registration), Plugin: &ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version},
	})
	cfg.bindProviderScan(scan)
	store := NewTestStore(cfg)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Providers, 1)
	identity := proposal.Providers[0].ClientIdentity
	require.Equal(t, &ResolvedProviderClientIdentity{
		Version: "1.2.3", UserAgent: "claude-cli/1.2.3 (" + runtime.GOOS + "; " + runtime.GOARCH + ")", OS: runtime.GOOS, Arch: runtime.GOARCH,
	}, identity)

	principal := strings.Repeat("a", 64)
	remote, err := CompileRemoteRuntime(root, filepath.Join(root, "remote"), false, proposal, principal, env.NewFromMap(map[string]string{"CLAUDE_CODE_VERSION": "9.9.9"}))
	require.NoError(t, err)
	admitted, err := remote.RuntimeSnapshot().ClientProviderIdentity("claude-ai", registration.Operation.Anthropic.ClientIdentity)
	require.NoError(t, err)
	require.Equal(t, *identity, admitted)

	missing := proposal
	missing.Providers = append([]RemoteProviderDefinition(nil), proposal.Providers...)
	missing.Providers[0].ClientIdentity = nil
	missing = sealRemoteRuntime(t, missing)
	_, err = CompileRemoteRuntime(root, filepath.Join(root, "missing"), false, missing, principal, env.NewFromMap(map[string]string{}))
	require.ErrorContains(t, err, "captured client_identity")

	tampered := proposal
	encoded, err := json.Marshal(proposal)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(encoded, &tampered))
	tampered.Providers[0].ClientIdentity.UserAgent = "server-selected-agent"
	tampered = sealRemoteRuntime(t, tampered)
	_, err = CompileRemoteRuntime(root, filepath.Join(root, "tampered"), false, tampered, principal, env.NewFromMap(map[string]string{}))
	require.ErrorContains(t, err, "user agent is invalid")
}
