package trafficcapture

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreparedRequestRejectsExecutableReplacement(t *testing.T) {
	workingDirectory := t.TempDir()
	name := "target"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	executable := filepath.Join(workingDirectory, name)
	moved := filepath.Join(workingDirectory, "approved-"+name)
	capturePath := filepath.Join(workingDirectory, "capture.mitm")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))

	prepared, err := Prepare(t.Context(), Request{
		Executable:  executable,
		WorkingDir:  workingDirectory,
		CapturePath: capturePath,
	})
	require.NoError(t, err)
	defer prepared.Close()
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	require.NoError(t, err)
	require.Equal(t, resolvedExecutable, prepared.Request().Executable)

	require.NoError(t, os.Rename(executable, moved))
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0o700))

	_, err = openVerifiedCapturePath(executable, prepared.executableIdentity, os.O_RDONLY)
	require.ErrorContains(t, err, "changed after approval")
	requireFileBytes(t, moved, "#!/bin/sh\n")
	requireFileBytes(t, executable, "#!/bin/sh\nexit 1\n")
}

func TestPrepareRejectsNonExecutableTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows executable validation does not use mode bits")
	}
	workingDirectory := t.TempDir()
	executable := filepath.Join(workingDirectory, "target")
	require.NoError(t, os.WriteFile(executable, []byte("not executable"), 0o600))

	_, err := Prepare(t.Context(), Request{
		Executable:  executable,
		WorkingDir:  workingDirectory,
		CapturePath: filepath.Join(workingDirectory, "capture.mitm"),
	})
	require.ErrorContains(t, err, "target is not executable")
}

func TestCaptureOutputRejectsParentReplacement(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "output")
	moved := filepath.Join(root, "approved-output")
	capturePath := filepath.Join(directory, "capture.mitm")
	require.NoError(t, os.Mkdir(directory, 0o700))

	binding, err := captureOutput(capturePath)
	require.NoError(t, err)
	defer binding.close()

	renameErr := os.Rename(directory, moved)
	if retainedDirectoryRenameBlocked(renameErr) {
		_, err = binding.create()
		require.NoError(t, err)
		require.NoDirExists(t, moved)
		require.FileExists(t, capturePath)
		return
	}
	require.NoError(t, renameErr)
	require.NoError(t, os.Mkdir(directory, 0o700))

	_, err = binding.create()
	require.ErrorContains(t, err, "changed after approval")
	require.NoFileExists(t, filepath.Join(moved, "capture.mitm"))
	require.NoFileExists(t, capturePath)
}

func TestCaptureOutputRejectsCreatedMissingDirectory(t *testing.T) {
	root := t.TempDir()
	directory := filepath.Join(root, "missing", "output")
	capturePath := filepath.Join(directory, "capture.mitm")
	binding, err := captureOutput(capturePath)
	require.NoError(t, err)
	defer binding.close()

	require.NoError(t, os.MkdirAll(directory, 0o700))
	_, err = binding.create()
	require.ErrorContains(t, err, "changed after approval")
	require.NoFileExists(t, capturePath)
}

func retainedDirectoryRenameBlocked(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	return errors.Is(err, syscall.Errno(5)) || errors.Is(err, syscall.Errno(32))
}

func requireFileBytes(t *testing.T, path, expected string) {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, expected, string(content))
}
