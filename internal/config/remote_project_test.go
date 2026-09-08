package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/redact"
	"github.com/stretchr/testify/require"
)

func TestCollectRemoteGeminiProjectUsesCapturedEnvironment(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("GEMINI_PROJECT_ID", "client-project")
	selected := SelectedModel{Provider: gemini.ID, Model: "fixture"}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: selected, SelectedModelTypeSmall: selected}}
	cfg.setDefaults(t.TempDir(), t.TempDir())
	cfg.Providers.Set(gemini.ID, ProviderConfig{ID: gemini.ID, APIKey: "synthetic-access", BaseURL: gemini.APIEndpoint, Type: catalog.TypeOpenAICompat,
		Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity}, Models: []catalog.Model{{ID: selected.Model, Name: "Fixture"}},
	})
	store := NewTestStore(cfg)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderGemini, accounts.Entry{ID: "selected", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
	capture := store.RuntimeSnapshot()
	t.Setenv("GEMINI_PROJECT_ID", "later-process-project")
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Equal(t, "client-project", *proposal.Providers[0].GeminiProjectID)
	require.Empty(t, proposal.Providers[0].Config.Configuration, "metadata must not be injected into provider configuration")
	original, _, err := capture.ClientProviderDefinition(gemini.ID)
	require.NoError(t, err)
	require.Equal(t, proposal.Providers[0], original)
	later, _, err := NewTestStore(cfg).RuntimeSnapshot().ClientProviderDefinition(gemini.ID)
	require.NoError(t, err)
	require.Equal(t, "later-process-project", *later.GeminiProjectID)
	originalDigest, err := original.Digest()
	require.NoError(t, err)
	laterDigest, err := later.Digest()
	require.NoError(t, err)
	require.NotEqual(t, originalDigest, laterDigest, "project authority participates in the provider-definition refresh fence")
	root := t.TempDir()
	principal := strings.Repeat("a", 64)
	remote, err := CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{"GEMINI_PROJECT_ID": "execution-host-project"}))
	require.NoError(t, err)
	admitted := remote.RuntimeSnapshot()
	value, err := admitted.ClientGeminiProjectID(gemini.ID)
	require.NoError(t, err)
	require.Equal(t, "client-project", value)
	remote.RegisterRemoteRuntimeSecrets()
	require.Equal(t, redact.Replacement, redact.String(value))
	publicConfig, err := json.Marshal(remote.Config())
	require.NoError(t, err)
	require.NotContains(t, string(publicConfig), "client-project")
	acknowledgement, err := json.Marshal(remote.RemoteAuthority())
	require.NoError(t, err)
	require.NotContains(t, string(acknowledgement), "client-project")
	proposal.Revision = 2
	proposal.Providers[0].GeminiProjectID = nil
	proposal = sealRemoteRuntime(t, proposal)
	_, err = remote.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.ErrorContains(t, err, "captured gemini_project_id")
	require.Same(t, admitted.Config(), remote.Config())
	require.Equal(t, admitted.RemoteAuthority(), remote.RemoteAuthority())
	proposal.Providers[0].GeminiProjectID = new("")
	proposal = sealRemoteRuntime(t, proposal)
	_, err = remote.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	value, err = remote.RuntimeSnapshot().ClientGeminiProjectID(gemini.ID)
	require.NoError(t, err)
	require.Empty(t, value, "explicit empty project metadata is accepted")
	value, err = admitted.ClientGeminiProjectID(gemini.ID)
	require.NoError(t, err)
	require.Equal(t, "client-project", value)
}

func TestRemoteProjectMetadataRejectsUnrelatedConstruction(t *testing.T) {
	proposal := remoteRuntimeFixture(t, "minimal.plugin")
	proposal.Providers[0].GeminiProjectID = new("unrelated-project")
	proposal = sealRemoteRuntime(t, proposal)
	_, err := CompileRemoteRuntime(t.TempDir(), t.TempDir(), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
	require.ErrorContains(t, err, "project metadata does not match")
}

func TestCollectRemoteGeminiProjectPreservesCompatibilityPluginSchema(t *testing.T) {
	root := t.TempDir()
	dataRoot, cacheRoot := filepath.Join(root, "data"), filepath.Join(root, "cache")
	t.Setenv("CRUX_GLOBAL_DATA", dataRoot)
	t.Setenv("CRUX_CACHE_DIR", cacheRoot)
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfilePluginCompat))
	t.Setenv("GEMINI_PROJECT_ID", "compatibility-client-project")
	bundle := providerClaimBundle(t, root, gemini.ID)
	bundle = providerCompatibilityClaimBundle(t, bundle, accounts.ProviderGemini, providerregistry.ConstructionGeminiAntigravity)
	installTrustedProviderBundle(t, dataRoot, cacheRoot, bundle)
	scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
	require.NoError(t, err)
	registered, ok := scan.Registry.Lookup(gemini.ID)
	require.True(t, ok)
	metadata, ok := lookupProvider(scan.Providers, gemini.ID)
	require.True(t, ok)
	require.NotEmpty(t, metadata.Models)
	selected := SelectedModel{Provider: gemini.ID, Model: metadata.Models[0].ID}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: selected, SelectedModelTypeSmall: selected}}
	cfg.setDefaults(root, filepath.Join(root, "local"))
	cfg.Providers.Set(gemini.ID, ProviderConfig{ID: gemini.ID, APIKey: "synthetic-key", BaseURL: metadata.APIEndpoint, Type: metadata.Type, Models: metadata.Models,
		Owner: providerOwnerReferenceForRegistration(registered), Plugin: &ProviderPluginReference{ID: registered.Manifest.ID, Version: registered.Manifest.Version},
	})
	cfg.bindProviderScan(scan)
	store := NewTestStore(cfg)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Bundles, 1)
	require.Equal(t, "compatibility-client-project", *proposal.Providers[0].GeminiProjectID)
	require.Empty(t, proposal.Providers[0].Config.Configuration)
	remote, err := CompileRemoteRuntime(root, filepath.Join(root, "remote"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{}))
	require.NoError(t, err)
	project, err := remote.RuntimeSnapshot().ClientGeminiProjectID(gemini.ID)
	require.NoError(t, err)
	require.Equal(t, "compatibility-client-project", project)
}
