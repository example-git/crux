package localaddon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestCopilotSessionBindingsPersistCallerIDs(t *testing.T) {
	workspace := &proto.Workspace{Path: t.TempDir()}
	require.NoError(t, saveCopilotBinding(workspace, "caller-session", "native-session"))

	bindings, err := loadCopilotBindings(workspace)
	require.NoError(t, err)
	require.Equal(t, "native-session", bindings["caller-session"])

	info, err := os.Stat(filepath.Join(workspace.Path, ".crux", "compatibility", "copilot-sessions.json"))
	require.NoError(t, err)
	require.True(t, info.Mode().IsRegular())
	if runtime.GOOS != "windows" {
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	require.NoError(t, deleteCopilotBinding(workspace, "caller-session"))
	bindings, err = loadCopilotBindings(workspace)
	require.NoError(t, err)
	require.NotContains(t, bindings, "caller-session")
}
