package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestInstallSystemdUserService(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	var commands [][]string
	original := runManagerCommand
	runManagerCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() {
		runManagerCommand = original
	})

	result, err := installSystemd(t.Context(), "/opt/crux bin/crux", "tcp://0.0.0.0:9090", []string{"/srv/work trees", "/srv/quote'root"})
	require.NoError(t, err)
	require.Equal(t, ManagerSystemd, result.Manager)
	content, err := os.ReadFile(filepath.Join(root, "systemd", "user", "crux-server.service"))
	require.NoError(t, err)
	require.Contains(t, string(content), `ExecStart="/opt/crux bin/crux" "server" "--host" "tcp://0.0.0.0:9090" "--workspace-root" "/srv/work trees" "--workspace-root" "/srv/quote'root"`)
	require.Equal(t, [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", "crux-server.service"},
	}, commands)
}

func TestInstallOpenRCUserService(t *testing.T) {
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	var commands [][]string
	original := runManagerCommand
	runManagerCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() {
		runManagerCommand = original
	})

	result, err := installOpenRC(t.Context(), "/opt/crux", "tcp://0.0.0.0:9090", []string{"/srv/work trees"})
	require.NoError(t, err)
	require.Equal(t, ManagerOpenRC, result.Manager)
	content, err := os.ReadFile(filepath.Join(root, "rc", "init.d", "crux-server"))
	require.NoError(t, err)
	require.Contains(t, string(content), "supervisor=supervise-daemon")
	require.Contains(t, string(content), "--workspace-root")
	require.Contains(t, string(content), "/srv/work trees")
	require.Equal(t, [][]string{
		{"rc-update", "--user", "add", "crux-server", "default"},
		{"rc-service", "--user", "crux-server", "start"},
	}, commands)
}

func TestDetectManagerRejectsUnknownExplicitManager(t *testing.T) {
	_, err := detectManager("launchd")
	require.EqualError(t, err, "unsupported service manager: launchd")
}
