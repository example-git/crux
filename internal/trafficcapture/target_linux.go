//go:build linux

package trafficcapture

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func resolvePIDTarget(ctx context.Context, pid int) (resolvedTarget, error) {
	if err := ctx.Err(); err != nil {
		return resolvedTarget{}, err
	}
	root := filepath.Join("/proc", strconv.Itoa(pid))
	commandLine, err := os.ReadFile(filepath.Join(root, "cmdline"))
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("inspect PID %d command line: %w", pid, err)
	}
	parts := strings.Split(string(commandLine), "\x00")
	command := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			command = append(command, part)
		}
	}
	if len(command) == 0 {
		return resolvedTarget{}, fmt.Errorf("PID %d has no readable command line", pid)
	}
	executable, err := filepath.EvalSymlinks(filepath.Join(root, "exe"))
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("inspect PID %d executable: %w", pid, err)
	}
	command[0] = executable
	environment := environmentMap(os.Environ())
	if data, readErr := os.ReadFile(filepath.Join(root, "environ")); readErr == nil {
		environment = environmentMap(strings.Split(string(data), "\x00"))
	}
	workingDir, _ := filepath.EvalSymlinks(filepath.Join(root, "cwd"))
	return resolvedTarget{
		Command:     command,
		Environment: environment,
		WorkingDir:  workingDir,
	}, nil
}
