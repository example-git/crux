package trafficcapture

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRuntimeArchivePathRejectsTraversal(t *testing.T) {
	root := t.TempDir()

	path, err := runtimeArchivePath(root, "lib/python3.12/os.py")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "lib", "python3.12", "os.py"), path)

	for _, value := range []string{"../escape", "lib/../../escape", "/absolute"} {
		_, err := runtimeArchivePath(root, value)
		require.ErrorContains(t, err, "unsafe embedded runtime archive path")
	}
}

func TestRuntimeArchiveLinkRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "lib")

	path, err := runtimeArchiveLink(root, parent, "python3.12/os.py")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(parent, "python3.12", "os.py"), path)

	_, err = runtimeArchiveLink(root, parent, "../../escape")
	require.ErrorContains(t, err, "unsafe embedded runtime archive link")
}

func TestShellJoinPreservesArguments(t *testing.T) {
	command := shellJoin("/path with spaces/crux", "value'quoted", "$(not-a-shell)")
	require.Equal(t, "'/path with spaces/crux' 'value'\"'\"'quoted' '$(not-a-shell)'", command)
}

func TestWritePaneLogKeepsBoundedTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pane.log")
	input := bytes.Repeat([]byte("0123456789abcdef"), 192*1024)

	require.NoError(t, WritePaneLog(path, bytes.NewReader(input)))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.LessOrEqual(t, len(data), 2*paneLogLimit)
	require.True(t, bytes.HasSuffix(input, data))
}

func TestResolveTargetPreservesExecutableArguments(t *testing.T) {
	workingDir := t.TempDir()
	executable := filepath.Join(workingDir, "capture target")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))

	target, err := resolveTarget(t.Context(), Request{
		Executable:  "./capture target",
		Arguments:   []string{"value with spaces", "$(not-a-shell)"},
		WorkingDir:  workingDir,
		CapturePath: filepath.Join(workingDir, "capture.mitm"),
	})
	require.NoError(t, err)
	resolved, err := filepath.EvalSymlinks(executable)
	require.NoError(t, err)
	require.Equal(t, []string{resolved, "value with spaces", "$(not-a-shell)"}, target.Command)
	require.Equal(t, workingDir, target.WorkingDir)
	require.NotEmpty(t, target.Environment)
}

func TestEnvironmentMapPreservesEqualsInValues(t *testing.T) {
	environment := environmentMap([]string{"TOKEN=left=right", "INVALID"})
	require.Equal(t, "left=right", environment["TOKEN"])
	require.NotContains(t, environment, "INVALID")
}

func TestEmbeddedRuntimeErrorExplainsTaggedBuild(t *testing.T) {
	if EmbeddedRuntimeAvailable() {
		require.NoError(t, EmbeddedRuntimeError())
		return
	}
	err := EmbeddedRuntimeError()
	require.Error(t, err)
	require.True(t, strings.Contains(err.Error(), "--embedded-mitmproxy"))
}
