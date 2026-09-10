package cmd

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerplugin/manifest/manifesttest"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type pluginUpdateFixture struct {
	root, workspace, source string
	values                  map[string]string
	declaration             manifest.Manifest
	account                 accounts.Entry
	command                 *cobra.Command
}

func newPluginUpdateFixture(t *testing.T, providerID string) *pluginUpdateFixture {
	t.Helper()
	root := t.TempDir()
	f := &pluginUpdateFixture{root: root, workspace: filepath.Join(root, "workspace"), source: filepath.Join(root, "source.plugin"), declaration: manifesttest.Delegated(providerID)}
	f.values = map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "XDG_CONFIG_HOME": filepath.Join(root, "xdg"),
		"CRUX_PROVIDER_PROFILE": "plugin-compat", "CRUX_PROVIDER_PLUGINS": "", "CRUX_DISABLE_AUTO_MEMORY": "true",
	}
	for key, value := range f.values {
		t.Setenv(key, value)
	}
	require.NoError(t, os.MkdirAll(f.workspace, 0o700))
	t.Chdir(f.workspace)
	f.command = &cobra.Command{}
	f.command.SetContext(t.Context())
	f.command.SetOut(io.Discard)
	f.command.Flags().String("data-dir", "", "")
	oldUpdate, oldNoTrust, oldRef, oldJSON := pluginInstallUpdate, pluginInstallNoTrust, pluginInstallRef, pluginOutputJSON
	t.Cleanup(func() {
		pluginInstallUpdate, pluginInstallNoTrust, pluginInstallRef, pluginOutputJSON = oldUpdate, oldNoTrust, oldRef, oldJSON
	})
	pluginInstallUpdate, pluginInstallNoTrust, pluginInstallRef, pluginOutputJSON = false, false, "", true
	f.writeBundle(t, "1.0.0")
	require.NoError(t, f.install(false, false))
	f.account = accounts.Entry{
		ID: "saved-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
	}
	require.NoError(t, accounts.Save(t.Context(), f.declaration.Provider.AccountNamespace, f.account))
	return f
}

func (f *pluginUpdateFixture) writeBundle(t *testing.T, version string) {
	t.Helper()
	f.declaration.Version = version
	for _, file := range registrytest.Bundle(f.declaration).Files {
		path := filepath.Join(f.source, file.Path)
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, file.Data, 0o600))
	}
}

func (f *pluginUpdateFixture) install(update, noTrust bool) error {
	pluginInstallUpdate, pluginInstallNoTrust = update, noTrust
	return pluginsInstallCmd.RunE(f.command, []string{f.source})
}

func (f *pluginUpdateFixture) configData(t *testing.T) []byte {
	t.Helper()
	id, model := f.declaration.Provider.ID, f.declaration.Models[0].ID
	choice := config.SelectedModel{Provider: id, Model: model, MaxTokens: 777, ReasoningEffort: f.declaration.Models[0].Reasoning.Levels[0]}
	data, err := json.Marshal(map[string]any{
		"providers": map[string]any{id: map[string]any{
			"plugin":  config.ProviderPluginReference{ID: f.declaration.ID, Version: "1.0.0"},
			"api_key": f.account.AccessToken,
			"oauth":   f.account.Token(),
		}},
		"models":  map[string]any{"large": choice, "small": choice},
		"options": map[string]any{"notifications": "disabled", "disable_auto_summarize": true},
	})
	require.NoError(t, err)
	return data
}

func writePluginUpdateConfig(t *testing.T, path string, data []byte) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

func TestPluginUpdateRefreshesConfiguredVersionAndLoads(t *testing.T) {
	for _, id := range []string{"codex", "gemini-ag"} {
		t.Run(id, func(t *testing.T) {
			f := newPluginUpdateFixture(t, id)
			before := f.configData(t)
			paths := []string{filepath.Join(f.values["CRUX_GLOBAL_CONFIG"], "crux.json"), filepath.Join(f.values["CRUX_GLOBAL_DATA"], "crux.json"), filepath.Join(f.workspace, "crux.json"), filepath.Join(f.workspace, ".crux", "crux.json")}
			for _, path := range paths {
				writePluginUpdateConfig(t, path, before)
			}
			accountPath := filepath.Join(f.values["AI_CLI_DIR"], "accounts.json")
			accountBefore, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			f.writeBundle(t, "1.1.0")
			require.NoError(t, f.install(true, false))
			versionPath := "providers." + id + ".plugin.version"
			for _, path := range paths {
				after, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "1.1.0", gjson.GetBytes(after, versionPath).String(), path)
				restored, err := sjson.SetBytes(after, versionPath, "1.0.0")
				require.NoError(t, err)
				require.Equal(t, before, restored, "only the version field changes")
			}
			// Repair the exact failure state reported by the user: new installed
			// bytes, but configuration still pinned to the previous version.
			writePluginUpdateConfig(t, paths[1], before)
			require.NoError(t, f.install(true, false))
			store, err := config.LoadIsolated(f.workspace, "", false, env.NewFromMap(f.values))
			require.NoError(t, err)
			require.True(t, store.Config().IsProviderAvailable(id))
			require.Empty(t, store.Config().ProviderLoadIssues())
			registration, ok := store.RuntimeSnapshot().ProviderRegistration(id)
			require.True(t, ok)
			require.Equal(t, "1.1.0", registration.Manifest.Version)
			require.Equal(t, config.SelectedModel{Provider: id, Model: f.declaration.Models[0].ID, MaxTokens: 777, ReasoningEffort: f.declaration.Models[0].Reasoning.Levels[0]}, store.Config().Models[config.SelectedModelTypeLarge])
			accountBytes, err := os.ReadFile(accountPath)
			require.NoError(t, err)
			require.Equal(t, accountBefore, accountBytes)
			f.writeBundle(t, "1.0.0")
			require.NoError(t, f.install(true, false), "an explicit downgrade updates the stored version too")
			store, err = config.LoadIsolated(f.workspace, "", false, env.NewFromMap(f.values))
			require.NoError(t, err)
			require.True(t, store.Config().IsProviderAvailable(id))
			registration, ok = store.RuntimeSnapshot().ProviderRegistration(id)
			require.True(t, ok)
			require.Equal(t, "1.0.0", registration.Manifest.Version)
		})
	}
}

func TestPluginUpdateRejectsFailedInstallWithoutChangingReferences(t *testing.T) {
	f := newPluginUpdateFixture(t, "codex")
	path := filepath.Join(f.values["CRUX_GLOBAL_DATA"], "crux.json")
	before := f.configData(t)
	writePluginUpdateConfig(t, path, before)
	f.writeBundle(t, "1.1.0")
	require.ErrorContains(t, f.install(false, false), "--update")
	f.declaration.Capabilities.Compatibility.Endpoints = nil
	f.writeBundle(t, "1.1.0")
	require.Error(t, f.install(true, false))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

func TestPluginUpdateReportsConfigFailureAndCanRetry(t *testing.T) {
	f := newPluginUpdateFixture(t, "codex")
	path := filepath.Join(f.values["CRUX_GLOBAL_DATA"], "crux.json")
	writePluginUpdateConfig(t, path, []byte("invalid JSON"))
	f.writeBundle(t, "1.1.0")
	err := f.install(true, false)
	require.ErrorContains(t, err, "was installed, but updating its configured references failed")
	require.ErrorContains(t, err, "retry --update")
	writePluginUpdateConfig(t, path, f.configData(t))
	require.NoError(t, f.install(true, false))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "1.1.0", gjson.GetBytes(after, "providers.codex.plugin.version").String())
}

func TestPluginUpdateWithoutTrustSynchronizesButDoesNotActivate(t *testing.T) {
	f := newPluginUpdateFixture(t, "codex")
	path := filepath.Join(f.values["CRUX_GLOBAL_DATA"], "crux.json")
	writePluginUpdateConfig(t, path, f.configData(t))
	f.writeBundle(t, "1.1.0")
	require.NoError(t, f.install(true, true))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "1.1.0", gjson.GetBytes(after, "providers.codex.plugin.version").String())
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(f.values["CRUX_GLOBAL_DATA"], f.values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	defer manager.Close()
	require.Equal(t, providerplugin.StateUntrusted, manager.Snapshot().Plugins[0].State)
	store, err := config.LoadIsolated(f.workspace, "", false, env.NewFromMap(f.values))
	require.NoError(t, err)
	require.False(t, store.Config().IsProviderAvailable("codex"))
}
