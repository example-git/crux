//go:build darwin

package trafficcapture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func resolvePIDTarget(ctx context.Context, pid int) (resolvedTarget, error) {
	commandLine, err := psValue(ctx, pid, "command")
	if err != nil {
		return resolvedTarget{}, err
	}
	executable, err := psValue(ctx, pid, "comm")
	if err != nil {
		return resolvedTarget{}, err
	}
	command, err := splitProcessCommand(commandLine)
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("parse PID %d command line: %w", pid, err)
	}
	if len(command) == 0 {
		return resolvedTarget{}, fmt.Errorf("PID %d has no readable command line", pid)
	}
	resolved, err := resolveExecutable(executable, "")
	if err != nil {
		return resolvedTarget{}, fmt.Errorf("inspect PID %d executable: %w", pid, err)
	}
	command[0] = resolved
	return resolvedTarget{
		Command:     command,
		Environment: environmentMap(os.Environ()),
		WorkingDir:  processWorkingDirectory(ctx, pid),
	}, nil
}

func psValue(ctx context.Context, pid int, field string) (string, error) {
	command := exec.CommandContext(ctx, "ps", "-ww", "-p", strconv.Itoa(pid), "-o", field+"=")
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("inspect PID %d %s: %w", pid, field, err)
	}
	value := strings.TrimSpace(string(output))
	if value == "" {
		return "", fmt.Errorf("PID %d has no readable %s", pid, field)
	}
	return value, nil
}

func processWorkingDirectory(ctx context.Context, pid int) string {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return ""
	}
	command := exec.CommandContext(ctx, lsof, "-a", "-p", strconv.Itoa(pid), "-d", "cwd", "-Fn")
	output, err := command.Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "n") {
			return strings.TrimPrefix(line, "n")
		}
	}
	return ""
}

func splitProcessCommand(value string) ([]string, error) {
	var result []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			result = append(result, current.String())
			current.Reset()
			started = false
		}
	}
	for _, character := range value {
		if escaped {
			current.WriteRune(character)
			escaped = false
			started = true
			continue
		}
		if character == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if character == quote {
				quote = 0
			} else {
				current.WriteRune(character)
			}
			started = true
			continue
		}
		if character == '\'' || character == '"' {
			quote = character
			started = true
			continue
		}
		if character == ' ' || character == '\t' || character == '\n' {
			flush()
			continue
		}
		current.WriteRune(character)
		started = true
	}
	if escaped || quote != 0 {
		return nil, fmt.Errorf("unterminated quote or escape")
	}
	flush()
	return result, nil
}
