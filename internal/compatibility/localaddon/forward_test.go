package localaddon

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	compatinstall "github.com/example-git/crux/internal/compatibility/install"
	"github.com/stretchr/testify/require"
)

func TestForwardIfDisabledRunsOriginalExecutableLaterInPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	root := filepath.Join(t.TempDir(), "compatibility")
	crux := writeExecutable(t, filepath.Join(t.TempDir(), "crux"), "#!/bin/sh\nexit 99\n")
	manager, err := compatinstall.New(root)
	require.NoError(t, err)
	status, err := manager.Install(compatinstall.Options{Executable: crux, SkipPath: true})
	require.NoError(t, err)
	_, err = manager.Disable()
	require.NoError(t, err)

	duplicateBin := t.TempDir()
	require.NoError(t, os.Link(crux, filepath.Join(duplicateBin, "codex")))
	originalBin := t.TempDir()
	writeExecutable(t, filepath.Join(originalBin, "codex"), "#!/bin/sh\nprintf 'args:%s\\n' \"$*\"\nIFS= read -r line\nprintf '%s\\n' \"$line\"\nexit 23\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	exitCode, handled, err := ForwardIfDisabled(context.Background(), compatibility.Invocation{
		Executable: filepath.Join(status.Bin, "codex"),
		Args:       []string{"one", "two"},
		Env:        []string{"PATH=" + status.Bin + string(os.PathListSeparator) + duplicateBin + string(os.PathListSeparator) + originalBin},
		Stdin:      bytes.NewBufferString("input\n"),
		Stdout:     &stdout,
		Stderr:     &stderr,
	})
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, 23, exitCode)
	require.Equal(t, "args:one two\ninput\n", stdout.String())
	require.Empty(t, stderr.String())
}

func TestForwardIfDisabledLeavesEnabledAndUnmanagedInvocationsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable fixture is Unix-only")
	}
	root := filepath.Join(t.TempDir(), "compatibility")
	crux := writeExecutable(t, filepath.Join(t.TempDir(), "crux"), "#!/bin/sh\nexit 0\n")
	manager, err := compatinstall.New(root)
	require.NoError(t, err)
	status, err := manager.Install(compatinstall.Options{Executable: crux, SkipPath: true})
	require.NoError(t, err)

	exitCode, handled, err := ForwardIfDisabled(context.Background(), compatibility.Invocation{
		Executable: filepath.Join(status.Bin, "claude"),
		Env:        []string{"PATH=" + status.Bin},
	})
	require.NoError(t, err)
	require.False(t, handled)
	require.Zero(t, exitCode)

	_, err = manager.Disable()
	require.NoError(t, err)
	unmanaged := writeExecutable(t, filepath.Join(t.TempDir(), "claude"), "#!/bin/sh\nexit 0\n")
	exitCode, handled, err = ForwardIfDisabled(context.Background(), compatibility.Invocation{
		Executable: unmanaged,
		Env:        []string{"PATH=" + filepath.Dir(unmanaged)},
	})
	require.NoError(t, err)
	require.False(t, handled)
	require.Zero(t, exitCode)
}

func TestForwardIfDisabledFailsClearlyWithoutOriginalExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable fixture is Unix-only")
	}
	root := filepath.Join(t.TempDir(), "compatibility")
	crux := writeExecutable(t, filepath.Join(t.TempDir(), "crux"), "#!/bin/sh\nexit 0\n")
	manager, err := compatinstall.New(root)
	require.NoError(t, err)
	status, err := manager.Install(compatinstall.Options{Executable: crux, SkipPath: true})
	require.NoError(t, err)
	_, err = manager.Disable()
	require.NoError(t, err)

	exitCode, handled, err := ForwardIfDisabled(context.Background(), compatibility.Invocation{
		Executable: filepath.Join(status.Bin, "agy"),
		Env:        []string{"PATH=" + status.Bin},
	})
	require.ErrorContains(t, err, "no original")
	require.True(t, handled)
	require.Equal(t, 127, exitCode)
}

func writeExecutable(t *testing.T, path, content string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o700))
	return path
}
