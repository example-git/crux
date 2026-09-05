package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSystemdServiceLifecycleUsesRecordedInstallation(t *testing.T) {
	root := prepareServiceTest(t)
	servicePath := filepath.Join(root, "config", "systemd", "user", "crux-server.service")
	require.NoError(t, os.MkdirAll(filepath.Dir(servicePath), 0o700))
	require.NoError(t, os.WriteFile(servicePath, []byte("service"), 0o600))
	require.NoError(t, saveServiceMetadata(Result{Manager: ManagerSystemd, Path: servicePath}, "tcp://0.0.0.0:9090", []string{"/srv/projects"}))

	var commands [][]string
	original := runManagerCommand
	runManagerCommand = func(_ context.Context, name string, args ...string) error {
		commands = append(commands, append([]string{name}, args...))
		return nil
	}
	t.Cleanup(func() { runManagerCommand = original })

	status, err := Status(t.Context())
	require.NoError(t, err)
	require.True(t, status.Active)
	require.Equal(t, ManagerSystemd, status.Metadata.Manager)
	require.NoError(t, Start(t.Context()))
	require.NoError(t, Stop(t.Context()))
	require.NoError(t, Restart(t.Context()))
	require.NoError(t, Uninstall(t.Context()))
	require.NoFileExists(t, servicePath)
	require.NoFileExists(t, filepath.Join(root, "data", "server-service.json"))
	require.Equal(t, [][]string{
		{"systemctl", "--user", "is-active", "crux-server.service"},
		{"systemctl", "--user", "start", "crux-server.service"},
		{"systemctl", "--user", "stop", "crux-server.service"},
		{"systemctl", "--user", "restart", "crux-server.service"},
		{"systemctl", "--user", "disable", "--now", "crux-server.service"},
		{"systemctl", "--user", "daemon-reload"},
	}, commands)
}

func TestOpenRCAndRunitServiceLifecycleCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		manager  Manager
		path     func(string) string
		expected [][]string
	}{
		{
			name:    "openrc",
			manager: ManagerOpenRC,
			path: func(root string) string {
				return filepath.Join(root, "config", "rc", "init.d", "crux-server")
			},
			expected: [][]string{
				{"rc-service", "--user", "crux-server", "start"},
				{"rc-service", "--user", "crux-server", "stop"},
				{"rc-service", "--user", "crux-server", "restart"},
				{"rc-service", "--user", "crux-server", "stop"},
				{"rc-update", "--user", "del", "crux-server", "default"},
			},
		},
		{
			name:    "runit",
			manager: ManagerRunit,
			path: func(root string) string {
				return filepath.Join(root, "home", "service", "crux-server", "run")
			},
			expected: [][]string{
				{"sv", "up", "SERVICE"},
				{"sv", "down", "SERVICE"},
				{"sv", "restart", "SERVICE"},
				{"sv", "down", "SERVICE"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := prepareServiceTest(t)
			servicePath := test.path(root)
			require.NoError(t, os.MkdirAll(filepath.Dir(servicePath), 0o700))
			require.NoError(t, os.WriteFile(servicePath, []byte("service"), 0o700))
			require.NoError(t, saveServiceMetadata(Result{Manager: test.manager, Path: servicePath}, "tcp://0.0.0.0:9090", nil))
			var commands [][]string
			original := runManagerCommand
			runManagerCommand = func(_ context.Context, name string, args ...string) error {
				command := append([]string{name}, args...)
				for index, value := range command {
					if value == filepath.Dir(servicePath) && test.manager == ManagerRunit {
						command[index] = "SERVICE"
					}
				}
				commands = append(commands, command)
				return nil
			}
			t.Cleanup(func() { runManagerCommand = original })
			require.NoError(t, Start(t.Context()))
			require.NoError(t, Stop(t.Context()))
			require.NoError(t, Restart(t.Context()))
			require.NoError(t, Uninstall(t.Context()))
			require.NoFileExists(t, servicePath)
			require.Equal(t, test.expected, commands)
		})
	}
}

func TestServiceMetadataRejectsUnexpectedRemovalPath(t *testing.T) {
	root := prepareServiceTest(t)
	metadata := Metadata{Version: serviceStateVersion, Manager: ManagerSystemd, Path: filepath.Join(root, "unrelated")}
	content, err := json.Marshal(metadata)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "server-service.json"), content, 0o600))

	err = Uninstall(t.Context())
	require.ErrorContains(t, err, "refusing to manage unexpected")
}

func TestLegacyServiceInstallationIsDiscovered(t *testing.T) {
	root := prepareServiceTest(t)
	servicePath := filepath.Join(root, "config", "rc", "init.d", "crux-server")
	require.NoError(t, os.MkdirAll(filepath.Dir(servicePath), 0o700))
	require.NoError(t, os.WriteFile(servicePath, []byte("service"), 0o700))
	original := runManagerCommand
	runManagerCommand = func(context.Context, string, ...string) error { return nil }
	t.Cleanup(func() { runManagerCommand = original })

	status, err := Status(t.Context())
	require.NoError(t, err)
	require.Equal(t, ManagerOpenRC, status.Metadata.Manager)
	require.Equal(t, servicePath, status.Metadata.Path)
}

func TestReadLogsReturnsRequestedTail(t *testing.T) {
	root := prepareServiceTest(t)
	servicePath := filepath.Join(root, "config", "systemd", "user", "crux-server.service")
	require.NoError(t, os.MkdirAll(filepath.Dir(servicePath), 0o700))
	require.NoError(t, os.WriteFile(servicePath, []byte("service"), 0o600))
	require.NoError(t, saveServiceMetadata(Result{Manager: ManagerSystemd, Path: servicePath}, "tcp://0.0.0.0:9090", nil))
	metadata, err := loadServiceMetadata()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(metadata.LogPath), 0o700))
	require.NoError(t, os.WriteFile(metadata.LogPath, []byte("one\ntwo\nthree\n"), 0o600))

	logs, err := ReadLogs(2)
	require.NoError(t, err)
	require.Equal(t, "two\nthree", logs)
}

func prepareServiceTest(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "home"), 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "data"), 0o700))
	return root
}
