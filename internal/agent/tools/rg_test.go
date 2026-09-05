package tools

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestEmbeddedRipgrepTargets(t *testing.T) {
	darwinARM64, err := os.ReadFile("ripgrep/rg-darwin-arm64")
	require.NoError(t, err)
	machoFile, err := macho.NewFile(bytes.NewReader(darwinARM64))
	require.NoError(t, err)
	require.Equal(t, macho.CpuArm64, machoFile.Cpu)
	require.NoError(t, machoFile.Close())

	linuxAMD64, err := os.ReadFile("ripgrep/rg-linux-amd64")
	require.NoError(t, err)
	amd64File, err := elf.NewFile(bytes.NewReader(linuxAMD64))
	require.NoError(t, err)
	require.Equal(t, elf.EM_X86_64, amd64File.Machine)
	require.NoError(t, amd64File.Close())

	linuxARM64, err := os.ReadFile("ripgrep/rg-linux-arm64")
	require.NoError(t, err)
	arm64File, err := elf.NewFile(bytes.NewReader(linuxARM64))
	require.NoError(t, err)
	require.Equal(t, elf.EM_AARCH64, arm64File.Machine)
	require.NoError(t, arm64File.Close())

	binary, ok := embeddedRipgrep(runtime.GOOS, runtime.GOARCH)
	if embeddedRipgrepOS == "" {
		require.False(t, ok)
		require.Empty(t, binary)
	} else {
		require.True(t, ok)
		require.NotEmpty(t, binary)
	}
	_, ok = embeddedRipgrep("darwin", "amd64")
	require.False(t, ok)
}

func TestGetRgUsesEmbeddedBinary(t *testing.T) {
	binary, ok := embeddedRipgrep(runtime.GOOS, runtime.GOARCH)
	if !ok {
		require.Empty(t, getRg())
		return
	}
	path := getRg()
	require.NotEmpty(t, path)
	require.Equal(t, "rg-"+embeddedRipgrepVersion+"-"+runtime.GOOS+"-"+runtime.GOARCH, filepath.Base(path))
	require.True(t, validRipgrepFile(path, binary))
}

func TestMaterializeRipgrepReplacesInvalidCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	binary, ok := embeddedRipgrep(runtime.GOOS, runtime.GOARCH)
	if !ok {
		t.Skip("current platform has no embedded ripgrep")
	}

	path, err := materializeRipgrep(binary, runtime.GOOS, runtime.GOARCH)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	output, err := exec.CommandContext(t.Context(), path, "--version").Output()
	require.NoError(t, err)
	require.Contains(t, string(output), "ripgrep "+embeddedRipgrepVersion)

	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o700))
	restored, err := materializeRipgrep(binary, runtime.GOOS, runtime.GOARCH)
	require.NoError(t, err)
	require.Equal(t, path, restored)
	require.True(t, validRipgrepFile(restored, binary))
}
