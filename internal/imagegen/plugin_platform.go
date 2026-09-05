package imagegen

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

type imagePlatformOutput struct {
	bytes.Buffer
}

func (b *imagePlatformOutput) Write(data []byte) (int, error) {
	if len(data) > 1024-b.Len() {
		return 0, errors.New("host platform output exceeds limit")
	}
	return b.Buffer.Write(data)
}

func imagePlatformValues(ctx context.Context, environment []string) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "os_version": "", "terminal_program": "", "terminal_version": "", "term": ""}
	names := map[string]string{"TERM_PROGRAM": "terminal_program", "TERM_PROGRAM_VERSION": "terminal_version", "TERM": "term"}
	for _, entry := range environment {
		name, value, ok := strings.Cut(entry, "=")
		key := names[name]
		if !ok || key == "" {
			continue
		}
		if len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return nil, errors.New("host terminal identity is invalid")
		}
		values[key] = value
	}
	if runtime.GOOS == "darwin" {
		probe, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		command := exec.CommandContext(probe, "/usr/bin/sw_vers", "-productVersion")
		command.Env = []string{}
		var output imagePlatformOutput
		command.Stdout = &output
		if err := command.Run(); err == nil {
			values["os_version"] = strings.TrimSpace(output.String())
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return values, nil
}
