package log

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSetupCleanupOwnsLogFile(t *testing.T) {
	const child = "CRUX_TEST_LOG_OWNERSHIP"
	if os.Getenv(child) != "1" {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetupCleanupOwnsLogFile$", "-test.count=1")
		command.Env = append(os.Environ(), child+"=1")
		output, err := command.CombinedOutput()
		require.NoError(t, err, "%s", output)
		return
	}

	root := t.TempDir()
	path := filepath.Join(root, "owned.log")
	previous := slog.Default()
	cleanup := Setup(path, false)
	defer cleanup()
	logger := slog.Default()
	secondaryCleanup := Setup(filepath.Join(root, "unused.log"), false)
	require.NoError(t, secondaryCleanup())
	logger.Info("still owned by first caller")
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Contains(t, string(data), "still owned by first caller")
	require.NoFileExists(t, filepath.Join(root, "unused.log"))
	require.NoError(t, cleanup())
	require.Same(t, previous, slog.Default())
	require.NoError(t, os.Remove(path))
	logger.Info("late write must not reopen closed file")
	require.NoFileExists(t, path)
	require.NoError(t, cleanup())
}
