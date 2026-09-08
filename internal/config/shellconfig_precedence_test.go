package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestNetworkTracingProjectOverridesGlobal(t *testing.T) {
	for _, test := range []struct {
		name    string
		file    string
		content string
		want    bool
		invalid bool
	}{
		{name: "inherited", file: "crux.json", content: `{}`, want: true},
		{name: "shell disabled", file: "cruxrc", content: "option network-tracing false"},
		{name: "json disabled", file: "crux.json", content: `{"options":{"network_tracing":false}}`},
		{name: "json enabled", file: "crux.json", content: `{"options":{"network_tracing":true}}`, want: true},
		{name: "json invalid", file: "crux.json", content: `{"options":{"network_tracing":"invalid"}}`, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			isolated := t.TempDir()
			globalDir := t.TempDir()
			t.Setenv("HOME", isolated)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
			t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))
			t.Setenv("CRUX_GLOBAL_CONFIG", globalDir)
			t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
			require.NoError(t, os.WriteFile(filepath.Join(globalDir, "cruxrc"), []byte("option network-tracing true"), 0o600))
			workDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(workDir, test.file), []byte(test.content), 0o600))
			store, err := config.Load(workDir, t.TempDir(), false)
			if test.invalid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, test.want, store.Config().Options.NetworkTracing)
		})
	}
}

// TestShellConfigDotCruxrcTakesPrecedence verifies that a project-local
// .cruxrc overrides cruxrc in the same directory on conflicting settings.
func TestShellConfigDotCruxrcTakesPrecedence(t *testing.T) {
	isolated := t.TempDir()
	t.Setenv("HOME", isolated)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(isolated, ".config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(isolated, ".local", "share"))

	workDir := t.TempDir()
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, "cruxrc"),
		[]byte("option notifications bell\n"), 0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(workDir, ".cruxrc"),
		[]byte("option notifications osc\n"), 0o644,
	))

	store, err := config.Load(workDir, dataDir, false)
	require.NoError(t, err)
	require.Equal(t, "osc", store.Config().Options.Notifications,
		".cruxrc should win over cruxrc")
}
