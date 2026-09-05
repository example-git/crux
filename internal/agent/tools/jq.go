package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/shell"
)

const (
	JQToolName            = "jq"
	maxJQToolOutput       = 30 * 1024
	jqOutputTruncatedMark = "\n[output truncated to fit 30720-byte response limit]"
)

var errJQOutputLimit = errors.New("jq output limit reached")

//go:embed jq.md
var jqToolDescription string

type JQParams struct {
	Filter        string            `json:"filter,omitempty" description:"jq filter to run; defaults to ."`
	Input         string            `json:"input,omitempty" description:"Serialized JSON stream, or raw text when raw_input is true; mutually exclusive with files"`
	Files         []string          `json:"files,omitempty" description:"JSON or raw-text files to process in order; mutually exclusive with input"`
	RawOutput     bool              `json:"raw_output,omitempty" description:"Output strings without JSON quoting"`
	JoinOutput    bool              `json:"join_output,omitempty" description:"Like raw_output but without trailing newlines"`
	CompactOutput bool              `json:"compact_output,omitempty" description:"Emit compact one-line JSON"`
	Slurp         bool              `json:"slurp,omitempty" description:"Read all input values into an array"`
	NullInput     bool              `json:"null_input,omitempty" description:"Run the filter once with null and ignore input"`
	ExitStatus    bool              `json:"exit_status,omitempty" description:"Fail when the last output is false or null"`
	RawInput      bool              `json:"raw_input,omitempty" description:"Read each input line as a string instead of parsing JSON"`
	Args          map[string]string `json:"args,omitempty" description:"String variables exposed to the filter as $name"`
	JSONArgs      map[string]any    `json:"json_args,omitempty" description:"JSON variables exposed to the filter as $name"`
}

type JQPermissionsParams struct {
	FilePath string `json:"file_path"`
	Filter   string `json:"filter"`
}

type JQResponseMetadata struct {
	Truncated bool `json:"truncated,omitempty"`
}

type jqOutputWriter struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func NewJQTool(permissions permission.Service, workingDir string, environments ...[]string) fantasy.AgentTool {
	var environment []string
	if len(environments) > 0 {
		environment = slices.Clone(environments[0])
	}
	return fantasy.NewAgentTool(
		JQToolName,
		jqToolDescription,
		func(ctx context.Context, params JQParams, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			options, denied, err := prepareJQOptions(ctx, permissions, workingDir, call.ID, params)
			if err != nil {
				return fantasy.NewTextErrorResponse(err.Error()), nil
			}
			if denied {
				return NewPermissionDeniedResponse(), nil
			}
			options.Environment = slices.Clone(environment)
			stdout := &jqOutputWriter{remaining: maxJQToolOutput - len(jqOutputTruncatedMark)}
			var stderr bytes.Buffer
			err = shell.RunJQ(ctx, options, strings.NewReader(params.Input), stdout, &stderr)
			if errors.Is(err, errJQOutputLimit) {
				return fantasy.WithResponseMetadata(
					fantasy.NewTextResponse(stdout.String()),
					JQResponseMetadata{Truncated: true},
				), nil
			}
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return fantasy.ToolResponse{}, ctxErr
				}
				message := strings.TrimSpace(stderr.String())
				if message == "" {
					message = fmt.Sprintf("jq exited with status %d", shell.ExitCode(err))
				}
				if output := stdout.String(); output != "" {
					if !strings.HasSuffix(output, "\n") {
						output += "\n"
					}
					message = output + message
				}
				return fantasy.NewTextErrorResponse(message), nil
			}
			if stdout.buffer.Len() == 0 {
				return fantasy.NewTextResponse("no output"), nil
			}
			return fantasy.NewTextResponse(stdout.String()), nil
		},
	)
}

func prepareJQOptions(ctx context.Context, permissions permission.Service, workingDir, toolCallID string, params JQParams) (shell.JQOptions, bool, error) {
	if params.Input != "" && len(params.Files) > 0 {
		return shell.JQOptions{}, false, errors.New("input and files are mutually exclusive")
	}
	if params.NullInput && (params.Input != "" || len(params.Files) > 0) {
		return shell.JQOptions{}, false, errors.New("null_input cannot be combined with input or files")
	}
	variables, err := jqVariables(params.Args, params.JSONArgs)
	if err != nil {
		return shell.JQOptions{}, false, err
	}
	files := make([]string, 0, len(params.Files))
	for _, file := range params.Files {
		if strings.TrimSpace(file) == "" {
			return shell.JQOptions{}, false, errors.New("files must not contain an empty path")
		}
		resolved, err := canonicalToolPath(workingDir, file)
		if err != nil {
			return shell.JQOptions{}, false, err
		}
		allowed, err := authorizeExternalPath(
			ctx,
			permissions,
			workingDir,
			resolved,
			toolCallID,
			JQToolName,
			"read",
			"Read jq input file outside working directory: "+resolved,
			JQPermissionsParams{FilePath: resolved, Filter: params.Filter},
		)
		if err != nil {
			return shell.JQOptions{}, false, err
		}
		if !allowed {
			return shell.JQOptions{}, true, nil
		}
		files = append(files, resolved)
	}
	return shell.JQOptions{
		Filter:        params.Filter,
		Files:         files,
		RawOutput:     params.RawOutput,
		JoinOutput:    params.JoinOutput,
		CompactOutput: params.CompactOutput,
		Slurp:         params.Slurp,
		NullInput:     params.NullInput,
		ExitStatus:    params.ExitStatus,
		RawInput:      params.RawInput,
		Variables:     variables,
	}, false, nil
}

func jqVariables(args map[string]string, jsonArgs map[string]any) ([]shell.JQVariable, error) {
	names := make([]string, 0, len(args)+len(jsonArgs))
	for name := range args {
		names = append(names, name)
	}
	for name := range jsonArgs {
		if _, exists := args[name]; exists {
			return nil, fmt.Errorf("variable %q is present in both args and json_args", name)
		}
		names = append(names, name)
	}
	slices.Sort(names)
	variables := make([]shell.JQVariable, 0, len(names))
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("variable names must not be empty")
		}
		if value, exists := args[name]; exists {
			variables = append(variables, shell.JQVariable{Name: name, Value: value})
			continue
		}
		value := jsonArgs[name]
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("encode json_args variable %q: %w", name, err)
		}
		var normalized any
		if err := json.Unmarshal(encoded, &normalized); err != nil {
			return nil, fmt.Errorf("decode json_args variable %q: %w", name, err)
		}
		variables = append(variables, shell.JQVariable{Name: name, Value: normalized})
	}
	return variables, nil
}

func (w *jqOutputWriter) Write(value []byte) (int, error) {
	if w.remaining == 0 {
		w.truncated = true
		return 0, errJQOutputLimit
	}
	if len(value) <= w.remaining {
		written, err := w.buffer.Write(value)
		w.remaining -= written
		return written, err
	}
	prefix := value[:w.remaining]
	for len(prefix) > 0 && !utf8.Valid(prefix) {
		prefix = prefix[:len(prefix)-1]
	}
	_, _ = w.buffer.Write(prefix)
	w.remaining = 0
	w.truncated = true
	return len(prefix), errJQOutputLimit
}

func (w *jqOutputWriter) String() string {
	result := w.buffer.String()
	if w.truncated {
		result += jqOutputTruncatedMark
	}
	return result
}

var _ io.Writer = (*jqOutputWriter)(nil)
