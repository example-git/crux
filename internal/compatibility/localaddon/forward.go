package localaddon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/example-git/crux/internal/compatibility"
	compatinstall "github.com/example-git/crux/internal/compatibility/install"
)

func ForwardIfDisabled(ctx context.Context, invocation compatibility.Invocation) (int, bool, error) {
	pathValue := environmentValue(invocation.Env, "PATH")
	mode, err := compatinstall.ModeForInvocation(invocation.Executable, pathValue)
	if err != nil {
		return 1, true, err
	}
	if !mode.Managed || mode.Enabled {
		return 0, false, nil
	}
	name := filepath.Base(invocation.Executable)
	original, err := findOriginalExecutable(name, mode.Path, mode.Bin, pathValue)
	if err != nil {
		return 127, true, fmt.Errorf("compatibility mode is disabled and no original %q executable was found later in PATH", name)
	}
	command := exec.CommandContext(ctx, original, invocation.Args...)
	command.Env = invocation.Env
	command.Dir = invocation.WorkingDir
	command.Stdin = invocation.Stdin
	command.Stdout = invocation.Stdout
	command.Stderr = invocation.Stderr
	if err := command.Run(); err != nil {
		var exitError *exec.ExitError
		if errors.As(err, &exitError) {
			code := exitError.ExitCode()
			if code < 0 {
				code = 1
			}
			return code, true, nil
		}
		return 126, true, fmt.Errorf("run original %q executable: %w", name, err)
	}
	return 0, true, nil
}

func findOriginalExecutable(name, managedPath, managedBin, pathValue string) (string, error) {
	candidates := []string{name}
	if runtime.GOOS == "windows" && filepath.Ext(name) == "" {
		candidates = append(candidates, name+".exe")
	}
	managedBin, _ = filepath.Abs(managedBin)
	managedInfo, _ := os.Stat(managedPath)
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = "."
		}
		absoluteDirectory, err := filepath.Abs(directory)
		if err != nil || samePath(absoluteDirectory, managedBin) {
			continue
		}
		for _, candidateName := range candidates {
			candidate := filepath.Join(absoluteDirectory, candidateName)
			info, err := os.Stat(candidate)
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
				continue
			}
			if managedInfo != nil && os.SameFile(managedInfo, info) {
				continue
			}
			return candidate, nil
		}
	}
	return "", os.ErrNotExist
}

func environmentValue(environment []string, name string) string {
	prefix := name + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if strings.HasPrefix(environment[index], prefix) {
			return strings.TrimPrefix(environment[index], prefix)
		}
	}
	return ""
}

func samePath(left, right string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
	}
	return filepath.Clean(left) == filepath.Clean(right)
}
