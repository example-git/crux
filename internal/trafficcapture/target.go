package trafficcapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type resolvedTarget struct {
	Command     []string
	Environment map[string]string
	WorkingDir  string
}

func resolveTarget(ctx context.Context, request Request) (resolvedTarget, error) {
	if (request.Executable == "") == (request.PID == 0) {
		return resolvedTarget{}, errors.New("exactly one of executable or pid is required")
	}
	if request.PID < 0 {
		return resolvedTarget{}, errors.New("pid must be greater than zero")
	}
	if request.PID != 0 {
		if len(request.Arguments) != 0 {
			return resolvedTarget{}, errors.New("arguments are only valid with executable")
		}
		target, err := resolvePIDTarget(ctx, request.PID)
		if err != nil {
			return resolvedTarget{}, err
		}
		if request.WorkingDirExplicit || target.WorkingDir == "" {
			target.WorkingDir = request.WorkingDir
		}
		return target, nil
	}
	executable, err := resolveExecutable(request.Executable, request.WorkingDir)
	if err != nil {
		return resolvedTarget{}, err
	}
	return resolvedTarget{
		Command:     append([]string{executable}, request.Arguments...),
		Environment: environmentMap(os.Environ()),
		WorkingDir:  request.WorkingDir,
	}, nil
}

func resolveExecutable(raw, workingDir string) (string, error) {
	value := raw
	if value == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve target executable home: %w", err)
		}
		value = home
	} else if strings.HasPrefix(value, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve target executable home: %w", err)
		}
		value = filepath.Join(home, strings.TrimPrefix(value, "~/"))
	}
	var path string
	var err error
	if strings.ContainsRune(value, filepath.Separator) {
		if !filepath.IsAbs(value) {
			value = filepath.Join(workingDir, value)
		}
		path, err = filepath.Abs(value)
	} else {
		path, err = exec.LookPath(value)
	}
	if err != nil {
		return "", fmt.Errorf("target executable was not found: %s", raw)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", fmt.Errorf("resolve target executable: %w", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("inspect target executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("target is not executable: %s", path)
	}
	return path, nil
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, content, ok := strings.Cut(value, "=")
		if ok {
			result[name] = content
		}
	}
	return result
}
