package tools

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/log"
)

const embeddedRipgrepVersion = "15.2.0"

var getRg = sync.OnceValue(func() string {
	binary, ok := embeddedRipgrep(runtime.GOOS, runtime.GOARCH)
	if !ok {
		return ""
	}
	path, err := materializeRipgrep(binary, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		if log.Initialized() {
			slog.Warn("Embedded ripgrep is unavailable; using the native search fallback", "error", err)
		}
		return ""
	}
	return path
})

func embeddedRipgrep(goos, goarch string) ([]byte, bool) {
	if goos != embeddedRipgrepOS || goarch != embeddedRipgrepArch || len(embeddedRipgrepBinary) == 0 {
		return nil, false
	}
	return embeddedRipgrepBinary, true
}

func materializeRipgrep(binary []byte, goos, goarch string) (string, error) {
	cacheDirectory, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache directory: %w", err)
	}
	directory := filepath.Join(cacheDirectory, "crux", "bin")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create ripgrep cache directory: %w", err)
	}
	path := filepath.Join(directory, fmt.Sprintf("rg-%s-%s-%s", embeddedRipgrepVersion, goos, goarch))
	if validRipgrepFile(path, binary) {
		return path, nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("replace cached ripgrep: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".rg-*")
	if err != nil {
		return "", fmt.Errorf("create temporary ripgrep: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		temporary.Close()
		return "", fmt.Errorf("make temporary ripgrep executable: %w", err)
	}
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write embedded ripgrep: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync embedded ripgrep: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close embedded ripgrep: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return "", fmt.Errorf("install embedded ripgrep: %w", err)
	}
	return path, nil
}

func validRipgrepFile(path string, expected []byte) bool {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o700 {
		return false
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) != len(expected) {
		return false
	}
	return sha256.Sum256(data) == sha256.Sum256(expected)
}

func getRgCmd(ctx context.Context, globPattern string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	args := []string{"--files", "--null"}
	if globPattern != "" {
		if !filepath.IsAbs(globPattern) && !strings.HasPrefix(globPattern, "/") {
			globPattern = "/" + globPattern
		}
		args = append(args, "--glob", globPattern)
	}
	return exec.CommandContext(ctx, name, args...)
}

func getRgSearchCmd(ctx context.Context, pattern, path, include string) *exec.Cmd {
	name := getRg()
	if name == "" {
		return nil
	}
	args := []string{"--json", "-H", "-n", "-0", pattern}
	if include != "" {
		args = append(args, "--glob", include)
	}
	args = append(args, path)

	return exec.CommandContext(ctx, name, args...)
}
