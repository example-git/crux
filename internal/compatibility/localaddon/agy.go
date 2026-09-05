package localaddon

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/example-git/crux/internal/compatibility"
)

type agyAdapter struct{}

func (agyAdapter) Translate(_ context.Context, invocation compatibility.Invocation) (compatibility.Request, error) {
	request := compatibility.Request{
		Source:     "agy",
		Style:      compatibility.ExecutionInteractive,
		WorkingDir: invocation.WorkingDir,
		Session:    compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:     compatibility.Output{Mode: compatibility.OutputText},
	}
	if len(invocation.Args) != 0 && !strings.HasPrefix(invocation.Args[0], "-") && enum(invocation.Args[0], "agent", "agents", "changelog", "help", "install", "mcp", "mic-serve", "models", "plugin", "plugins", "update") {
		return compatibility.Request{}, parseError(1, "command %q is not supported by the Crux compatibility layer\n", invocation.Args[0])
	}
	parser := parser{args: invocation.Args}
	inputFormat := "text"
	promptSet := false
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput("Usage of agy:\n  -p, --print, --prompt string\n  -i, --prompt-interactive string\n  --output-format string\n", true)
		case "-v", "--version":
			return compatibility.Request{}, successOutput("1.1.20\n", false)
		case "-p", "--print", "--prompt":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			if promptSet {
				return compatibility.Request{}, agyFlagError("prompt specified more than once")
			}
			promptSet = true
			request.Style = compatibility.ExecutionHeadless
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptArguments, Text: value}
		case "-i", "--prompt-interactive":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			if promptSet {
				return compatibility.Request{}, agyFlagError("prompt specified more than once")
			}
			promptSet = true
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptArguments, Text: value}
		case "--model", "--agent", "--effort":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			switch name {
			case "--model":
				request.Model = value
			case "--agent":
				request.Agent = value
			case "--effort":
				request.Effort = value
			}
		case "--add-dir":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			request.AdditionalDirectories = append(request.AdditionalDirectories, value)
		case "-c", "--continue":
			request.Session.Mode = compatibility.SessionLatest
		case "--conversation":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			request.Session.Mode = compatibility.SessionExplicit
			request.Session.ID = value
		case "--project":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["project"] = value
		case "--new-project":
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["new_project"] = "true"
		case "--mode":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "accept-edits", "plan") {
				return compatibility.Request{}, agyFlagError("invalid value %q for --mode", value)
			}
			request.Permissions.Mode = compatibility.PermissionMode(value)
		case "--sandbox":
			request.Permissions.Sandbox = "sandbox"
		case "--dangerously-skip-permissions":
			request.Permissions.Bypass = true
		case "--input-format":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "text", "stream-json") {
				return compatibility.Request{}, agyFlagError("invalid value %q for --input-format", value)
			}
			inputFormat = value
		case "--output-format":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "text", "json", "stream-json") {
				return compatibility.Request{}, agyFlagError("invalid value %q for --output-format", value)
			}
			switch value {
			case "text":
				request.Output.Mode = compatibility.OutputText
			case "json":
				request.Output.Mode = compatibility.OutputJSON
			case "stream-json":
				request.Output.Mode = compatibility.OutputStreamJSON
			}
		case "--json-schema":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			data := []byte(value)
			if fileData, readErr := os.ReadFile(value); readErr == nil {
				data = fileData
			} else if enum(value, "string", "number", "integer", "boolean") {
				data, _ = json.Marshal(map[string]string{"type": value})
			} else if !json.Valid(data) {
				return compatibility.Request{}, agyFlagError("invalid JSON schema %q", value)
			}
			request.Output.Schema = data
		case "--print-timeout":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			duration, err := parseDuration(value)
			if err != nil || duration <= 0 {
				return compatibility.Request{}, agyFlagError("invalid duration %q", value)
			}
			request.Limits.Timeout = duration
		case "--disable-slash-commands":
		case "--remote-control", "--bg-updater":
			return compatibility.Request{}, agyFlagError("%s is not supported by the Crux compatibility layer", name)
		case "--log-file":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, agyFlagError(err.Error())
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["log-file"] = value
		default:
			return compatibility.Request{}, agyFlagError("flag provided but not defined: %s", argument)
		}
	}
	if inputFormat == "stream-json" {
		if promptSet {
			return compatibility.Request{}, agyFlagError("a command-line prompt cannot be used with stream-json input")
		}
		if request.Output.Mode != compatibility.OutputStreamJSON {
			return compatibility.Request{}, agyFlagError("stream-json input requires stream-json output")
		}
		request.Style = compatibility.ExecutionHeadless
		request.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: invocation.Stdin}
	}
	if request.Style == compatibility.ExecutionHeadless && request.Limits.Timeout == 0 {
		request.Limits.Timeout = 5 * time.Minute
	}
	return request, nil
}

func agyFlagError(format string, values ...any) error {
	return parseError(2, format+"\nUsage of agy:\n  -p string\n  -i string\n", values...)
}
