package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest/manifesttest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

func TestLoadContinuesWithUnavailableDelegatedProvider(t *testing.T) {
	for _, id := range []string{"codex", "gemini-ag"} {
		for _, selected := range []string{id, "healthy"} {
			t.Run(id+"/selected-"+selected, func(t *testing.T) {
				root := t.TempDir()
				values := map[string]string{
					"HOME": root, "USERPROFILE": root, "XDG_CONFIG_HOME": filepath.Join(root, "xdg"),
					"AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"),
					"CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"),
					"CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginCompat), "CRUX_DISABLE_AUTO_MEMORY": "true",
				}
				for key, value := range values {
					t.Setenv(key, value)
				}
				workspace := filepath.Join(root, "workspace")
				require.NoError(t, os.MkdirAll(workspace, 0o700))
				model := catalog.Model{ID: "test-model", Name: "Test model", ContextWindow: 10000, DefaultMaxTokens: 1000}
				choice := SelectedModel{Provider: selected, Model: model.ID, MaxTokens: 777}
				document := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{
					id:        {ID: id, Models: []catalog.Model{model}},
					"healthy": {ID: "healthy", Type: catalog.TypeOpenAICompat, BaseURL: "https://healthy.example.invalid", APIKey: "synthetic", Models: []catalog.Model{model}},
				}), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: choice, SelectedModelTypeSmall: choice}}
				data, err := json.Marshal(document)
				require.NoError(t, err)
				path := filepath.Join(workspace, "crux.json")
				require.NoError(t, os.WriteFile(path, data, 0o600))
				store, err := LoadIsolated(workspace, filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
				require.NoError(t, err)
				cfg := store.Config()
				require.False(t, cfg.IsProviderAvailable(id))
				require.True(t, cfg.IsProviderAvailable("healthy"))
				require.Equal(t, selected == "healthy", cfg.CanInitializeAgent())
				require.Equal(t, choice, cfg.Models[SelectedModelTypeLarge])
				require.Equal(t, choice, cfg.Models[SelectedModelTypeSmall])
				issues := cfg.ProviderLoadIssues()
				require.Len(t, issues, 1)
				require.Equal(t, id, issues[0].ProviderID)
				issues[0].Message = "mutated"
				require.NotEqual(t, "mutated", cfg.ProviderLoadIssues()[0].Message)
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, data, after)
				proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
				require.NoError(t, err)
				remoteRoot := filepath.Join(root, "remote")
				remote, err := CompileRemoteRuntime(remoteRoot, filepath.Join(remoteRoot, "data"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": remoteRoot}))
				require.NoError(t, err)
				require.Equal(t, choice, remote.Config().Models[SelectedModelTypeLarge])
				if selected == id {
					require.Len(t, proposal.Providers, 1)
					require.NotNil(t, proposal.Providers[0].Unloaded)
					require.Empty(t, proposal.Providers[0].Config.BaseURL)
					require.Empty(t, proposal.Credentials)
					require.Empty(t, proposal.Bundles)
					require.False(t, remote.Config().IsProviderAvailable(id))
					require.False(t, remote.Config().CanInitializeAgent())
					require.ErrorContains(t, remote.RuntimeSnapshot().ClientProviderUnavailable(id), "was not loaded")
					require.Len(t, remote.Config().ProviderLoadIssues(), 1)
					for _, field := range []string{"endpoint", "credentials"} {
						bad := proposal
						bad.Providers = append([]RemoteProviderDefinition(nil), proposal.Providers...)
						if field == "endpoint" {
							bad.Providers[0].Config.BaseURL = "https://forbidden.example.invalid"
						} else {
							bad.Credentials = []RemoteCredentialBinding{{Owner: providerregistry.RegistrationOwner{ProviderID: id}, Generation: 1, APIKey: "must-not-be-admitted"}}
						}
						bad.Digest, err = RemoteRuntimeDigest(bad)
						require.NoError(t, err)
						_, err = CompileRemoteRuntime(remoteRoot, filepath.Join(remoteRoot, "data"), false, bad, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": remoteRoot}))
						require.Error(t, err, field)
					}
				} else {
					require.True(t, remote.Config().CanInitializeAgent())
				}
			})
		}
	}
}

func TestMissingDelegatedBundlesHaveNoCatalogOrRegistration(t *testing.T) {
	for _, profile := range []ProviderProfile{ProviderProfileCoreOnly, ProviderProfileIntegrated, ProviderProfilePluginCompat, ProviderProfilePluginNative} {
		t.Run(string(profile), func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
			t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
			t.Setenv("CRUX_PROVIDER_PROFILE", string(profile))
			scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
			require.NoError(t, err)
			for _, id := range []string{"codex", "gemini-ag"} {
				_, ok := scan.Registry.Lookup(id)
				require.False(t, ok)
				_, ok = lookupProvider(scan.Providers, id)
				require.False(t, ok)
				require.Empty(t, scan.LoadIssues)
				missing, err := FreshProviderScan(t.Context(), &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{id: {ID: id}})})
				require.NoError(t, err)
				require.Len(t, missing.LoadIssues, 1)
				require.Equal(t, id, missing.LoadIssues[0].ProviderID)
				require.Contains(t, missing.LoadIssues[0].Message, "bundle is required")
			}
		})
	}
}

func TestInstalledOldDelegatedBundleFailsActivationWithoutFallback(t *testing.T) {
	for _, id := range []string{"codex", "gemini-ag"} {
		t.Run(id, func(t *testing.T) {
			root := t.TempDir()
			data, cache := filepath.Join(root, "data"), filepath.Join(root, "cache")
			t.Setenv("CRUX_GLOBAL_DATA", data)
			t.Setenv("CRUX_CACHE_DIR", cache)
			t.Setenv("CRUX_PROVIDER_PROFILE", string(ProviderProfilePluginCompat))
			healthyID := "gemini-ag"
			if id == healthyID {
				healthyID = "codex"
			}
			healthy := manifesttest.Delegated(healthyID)
			require.NoError(t, registrytest.Install(t.Context(), data, cache, healthy))
			value := manifesttest.Delegated(id)
			value.Version = "1.0.0"
			value.Capabilities.Compatibility.Endpoints = nil
			// Simulate bytes installed by the previous host, before this validation existed.
			for _, file := range registrytest.Bundle(value).Files {
				path := filepath.Join(data, "plugins", value.ID+".plugin", file.Path)
				require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
				require.NoError(t, os.WriteFile(path, file.Data, 0o600))
			}
			manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(data, cache))
			require.NoError(t, err)
			statuses := manager.Snapshot().Plugins
			require.Len(t, statuses, 2)
			var oldStatus providerplugin.Status
			for _, status := range statuses {
				if status.ID == value.ID {
					oldStatus = status
				}
			}
			require.NotEmpty(t, oldStatus.Digest)
			_, err = manager.SetTrust(t.Context(), oldStatus.ID, providerplugin.TrustRequest{Digest: oldStatus.Digest, Trusted: true})
			require.NoError(t, err)
			manager.Close()
			scan, err := FreshProviderScan(t.Context(), &Config{Options: &Options{}})
			require.NoError(t, err)
			require.Len(t, scan.LoadIssues, 1)
			require.Contains(t, scan.LoadIssues[0].Message, "endpoint bindings")
			require.Equal(t, providerplugin.StateIncompatible, scan.pluginStatuses[value.ID].State)
			_, exported := scan.bundles[oldStatus.Digest]
			require.False(t, exported, "rejected bundle must not be forwarded to another runtime")
			_, healthyRegistered := scan.Registry.Lookup(healthyID)
			require.True(t, healthyRegistered)
			require.Equal(t, providerplugin.StateRegistered, scan.pluginStatuses[healthy.ID].State)
			_, healthyExported := scan.bundles[scan.pluginStatuses[healthy.ID].Digest]
			require.True(t, healthyExported)
			_, ok := scan.Registry.Lookup(id)
			require.False(t, ok)
			_, ok = lookupProvider(scan.Providers, id)
			require.False(t, ok)
			workspace := filepath.Join(root, "workspace")
			require.NoError(t, os.MkdirAll(workspace, 0o700))
			choice := SelectedModel{Provider: id, Model: "kept-model", MaxTokens: 777}
			document := &Config{Providers: csync.NewMapFrom(map[string]ProviderConfig{id: {ID: id, Plugin: &ProviderPluginReference{ID: value.ID, Version: value.Version}}}), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: choice, SelectedModelTypeSmall: choice}}
			encoded, err := json.Marshal(document)
			require.NoError(t, err)
			path := filepath.Join(workspace, "crux.json")
			require.NoError(t, os.WriteFile(path, encoded, 0o600))
			base := env.NewFromMap(map[string]string{"HOME": root, "XDG_CONFIG_HOME": filepath.Join(root, "xdg"), "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": data, "CRUX_CACHE_DIR": cache, "CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginCompat)})
			store, err := LoadIsolated(workspace, filepath.Join(root, "workspace-data"), false, base)
			require.NoError(t, err)
			require.Len(t, store.Config().ProviderLoadIssues(), 1)
			require.False(t, store.Config().IsProviderAvailable(id))
			require.Equal(t, choice, store.Config().Models[SelectedModelTypeLarge])
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, encoded, after)
		})
	}
}
