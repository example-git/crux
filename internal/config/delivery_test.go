package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestDeliveryPreferenceAcrossProjectsAndReload(t *testing.T) {
	isolateReloadProviderScan(t)
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	global := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", global)
	resetProviderState()
	t.Cleanup(resetProviderState)
	first, err := Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	require.Equal(t, "queue", first.Config().Options.TUI.DeliveryMode)
	for _, mode := range []string{"steer", "queue"} {
		require.NoError(t, first.SetConfigField(ScopeGlobal, "options.tui.delivery_mode", mode))
		data, err := os.ReadFile(filepath.Join(global, "crux.json"))
		require.NoError(t, err)
		require.Equal(t, mode, gjson.GetBytes(data, "options.tui.delivery_mode").String())
		project, workspace := t.TempDir(), t.TempDir()
		opposite := "queue"
		if mode == "queue" {
			opposite = "steer"
		}
		local := []byte(`{"options":{"tui":{"delivery_mode":"` + opposite + `"}}}`)
		require.NoError(t, os.WriteFile(filepath.Join(project, "crux.json"), local, 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(workspace, "crux.json"), local, 0o600))
		restarted, err := Load(project, workspace, false)
		require.NoError(t, err)
		require.Equal(t, mode, restarted.Config().Options.TUI.DeliveryMode)
		require.NoError(t, restarted.ReloadFromDisk(context.Background()))
		require.Equal(t, mode, restarted.Config().Options.TUI.DeliveryMode)
	}
}

func TestDeliveryPreferenceRejectsInvalidWrites(t *testing.T) {
	isolateReloadProviderScan(t)
	t.Setenv("CRUX_GLOBAL_CONFIG", t.TempDir())
	global := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", global)
	resetProviderState()
	t.Cleanup(resetProviderState)
	store, err := Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	require.NoError(t, store.SetConfigField(ScopeGlobal, "options.tui.delivery_mode", "steer"))
	path := filepath.Join(global, "crux.json")
	before, err := os.ReadFile(path)
	require.NoError(t, err)
	for _, value := range []any{"", "invalid", true, 42, nil} {
		require.Error(t, store.SetConfigField(ScopeGlobal, "options.tui.delivery_mode", value))
		require.Error(t, store.SetConfigFields(ScopeGlobal, map[string]any{"options.tui.delivery_mode": value}))
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		require.Equal(t, before, after)
		require.Equal(t, "steer", store.Config().Options.TUI.DeliveryMode)
	}
	for _, value := range []string{`"invalid"`, `true`, `null`} {
		require.NoError(t, os.WriteFile(path, []byte(`{"options":{"tui":{"delivery_mode":`+value+`}}}`), 0o600))
		_, err := Load(t.TempDir(), t.TempDir(), false)
		require.Error(t, err)
		require.Error(t, store.ReloadFromDisk(context.Background()))
	}
}
