package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
)

const ScriptToolName = "script"

const scriptOutputLimit = 32 * 1024

type ScriptParams struct {
	Variables map[string]string `json:"variables,omitempty" description:"Values for the configured script variables"`
}

type ScriptPermissionsParams struct {
	Variables map[string]string `json:"variables,omitempty"`
}

type limitedScriptBuffer struct {
	mu        sync.Mutex
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func newLimitedScriptBuffer(limit int) *limitedScriptBuffer {
	return &limitedScriptBuffer{remaining: limit}
}

func (b *limitedScriptBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	originalLength := len(p)
	if len(p) > b.remaining {
		p = p[:b.remaining]
		b.truncated = true
	}
	if len(p) > 0 {
		_, _ = b.buffer.Write(p)
		b.remaining -= len(p)
	}
	return originalLength, nil
}

func (b *limitedScriptBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := b.buffer.String()
	if b.truncated {
		result += "\n... output truncated"
	}
	return result
}

func NewScriptTool(permissions permission.Service, workingDir string, script config.AgentScript) fantasy.AgentTool {
	return fantasy.NewAgentTool(
		ScriptToolName,
		scriptToolDescription(script),
		func(ctx context.Context, params ScriptParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			args, err := scriptArguments(script, params.Variables)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			sessionID := GetSessionFromContext(ctx)
			if sessionID == "" {
				return fantasy.ToolResponse{}, errors.New("session ID is required for executing configured script")
			}
			granted, err := permissions.Request(ctx, permission.CreatePermissionRequest{
				SessionID:   sessionID,
				Path:        script.Path,
				ToolCallID:  call.ID,
				ToolName:    ScriptToolName,
				Action:      "execute",
				Description: "Execute configured Python script: " + script.Path,
				Params:      ScriptPermissionsParams(params),
			})
			if err != nil {
				return fantasy.ToolResponse{}, err
			}
			if !granted {
				return NewPermissionDeniedResponse(), nil
			}

			runCtx, cancel := context.WithTimeout(ctx, script.Timeout)
			defer cancel()
			interpreter, err := pythonInterpreter()
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			command := exec.CommandContext(runCtx, interpreter, append([]string{script.Path}, args...)...)
			command.Dir = workingDir
			command.Env = os.Environ()
			stdout := newLimitedScriptBuffer(scriptOutputLimit)
			stderr := newLimitedScriptBuffer(scriptOutputLimit)
			command.Stdout = stdout
			command.Stderr = stderr
			err = command.Run()

			output := formatScriptOutput(stdout.String(), stderr.String())
			if runCtx.Err() == context.DeadlineExceeded {
				return fantasy.NewTextErrorResponse(fmt.Sprintf("configured script timed out after %s%s", script.Timeout, output)), nil
			}
			if err != nil {
				if output == "" {
					output = ": " + err.Error()
				}
				return fantasy.NewTextErrorResponse("configured script failed" + output), nil
			}
			if output == "" {
				return fantasy.NewTextResponse("Configured script completed with no output."), nil
			}
			return fantasy.NewTextResponse(strings.TrimPrefix(output, ": ")), nil
		},
	)
}

func pythonInterpreter() (string, error) {
	for _, name := range []string{"python3", "python"} {
		path, err := exec.LookPath(name)
		if err == nil {
			return path, nil
		}
	}
	return "", errors.New("configured script requires python3 or python on PATH")
}

func scriptArguments(script config.AgentScript, supplied map[string]string) ([]string, error) {
	for name := range supplied {
		variable, ok := script.Variables[name]
		if !ok {
			return nil, fmt.Errorf("unknown script variable %q", name)
		}
		if variable.Value != nil {
			return nil, fmt.Errorf("script variable %q is preset and cannot be overridden", name)
		}
	}

	names := make([]string, 0, len(script.Variables))
	for name := range script.Variables {
		names = append(names, name)
	}
	slices.Sort(names)
	args := make([]string, 0, len(names)*2)
	for _, name := range names {
		variable := script.Variables[name]
		value, ok := supplied[name]
		if !ok && variable.Value != nil {
			value, ok = *variable.Value, true
		}
		if !ok && variable.Default != nil {
			value, ok = *variable.Default, true
		}
		if !ok {
			if variable.Required {
				return nil, fmt.Errorf("script variable %q is required", name)
			}
			continue
		}
		if len(variable.Values) > 0 && !slices.Contains(variable.Values, value) {
			return nil, fmt.Errorf("script variable %q must be one of: %s", name, strings.Join(variable.Values, ", "))
		}
		args = append(args, variable.Flag, value)
	}
	return args, nil
}

func scriptToolDescription(script config.AgentScript) string {
	lines := []string{"Execute the Python script configured for this custom agent. You cannot select a script path or pass arbitrary command-line arguments.", "", "Configured variables:"}
	names := make([]string, 0, len(script.Variables))
	for name := range script.Variables {
		names = append(names, name)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return strings.Join(append(lines, "- none"), "\n")
	}
	for _, name := range names {
		variable := script.Variables[name]
		details := []string{variable.Flag}
		switch {
		case variable.Value != nil:
			details = append(details, "preset")
		case variable.Required:
			details = append(details, "required")
		case variable.Default != nil:
			details = append(details, "has default")
		default:
			details = append(details, "optional")
		}
		if len(variable.Values) > 0 {
			details = append(details, "values: "+strings.Join(variable.Values, ", "))
		}
		lines = append(lines, fmt.Sprintf("- %s (%s)", name, strings.Join(details, "; ")))
	}
	return strings.Join(lines, "\n")
}

func formatScriptOutput(stdout, stderr string) string {
	stdout = strings.TrimSpace(stdout)
	stderr = strings.TrimSpace(stderr)
	parts := make([]string, 0, 2)
	if stdout != "" {
		parts = append(parts, stdout)
	}
	if stderr != "" {
		parts = append(parts, "stderr:\n"+stderr)
	}
	if len(parts) == 0 {
		return ""
	}
	return ": " + strings.Join(parts, "\n\n")
}
