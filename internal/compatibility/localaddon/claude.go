package localaddon

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/example-git/crux/internal/compatibility"
)

type claudeAdapter struct{}

func (claudeAdapter) Translate(_ context.Context, invocation compatibility.Invocation) (compatibility.Request, error) {
	request := compatibility.Request{
		Source:     "claude",
		Style:      compatibility.ExecutionInteractive,
		WorkingDir: invocation.WorkingDir,
		Session:    compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:     compatibility.Output{Mode: compatibility.OutputText},
	}
	if len(invocation.Args) != 0 && !strings.HasPrefix(invocation.Args[0], "-") && enum(invocation.Args[0], "agents", "auth", "auto-mode", "doctor", "gateway", "import", "install", "mcp", "plugin", "plugins", "project", "setup-token", "ultrareview", "update", "upgrade", "logs", "attach", "stop", "kill", "respawn", "rm", "daemon", "self-hosted-runner") {
		return compatibility.Request{}, claudeError("command %q is not supported by the Crux compatibility layer", invocation.Args[0])
	}

	parser := parser{args: invocation.Args}
	var prompt []string
	inputFormat := "text"
	verbose := false
	includePartial := false
	replayMessages := false
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput("Usage: claude [options] [prompt]\n\nOptions:\n  -p, --print\n  --input-format <text|stream-json>\n  --output-format <text|json|stream-json>\n", false)
		case "-v", "--version":
			return compatibility.Request{}, successOutput("2.1.245 (Claude Code)\n", false)
		case "-p", "--print":
			request.Style = compatibility.ExecutionHeadless
		case "--agents", "--autocompact", "--max-thinking-tokens":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
		case "--model", "--fallback-model", "--effort", "--agent":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			switch name {
			case "--model":
				request.Model = value
			case "--fallback-model":
				request.SmallModel = value
			case "--effort":
				request.Effort = value
			case "--agent":
				request.Agent = value
			}
		case "--add-dir":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			request.AdditionalDirectories = append(request.AdditionalDirectories, value)
		case "-c", "--continue":
			request.Session.Mode = compatibility.SessionLatest
		case "-r", "--resume":
			request.Session.Mode = compatibility.SessionLatest
			if _, value, ok := strings.Cut(argument, "="); ok && value != "" {
				request.Session.Mode = compatibility.SessionExplicit
				request.Session.ID = value
			} else if parser.more() && !strings.HasPrefix(parser.args[parser.index], "-") {
				request.Session.Mode = compatibility.SessionExplicit
				request.Session.ID = parser.next()
			}
		case "--fork-session":
			request.Session.Mode = compatibility.SessionFork
		case "--session-id":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			request.Session.Mode = compatibility.SessionExplicit
			request.Session.ID = value
		case "--no-session-persistence":
			request.Session.Persistent = false
		case "--permission-mode":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "acceptEdits", "auto", "bypassPermissions", "manual", "dontAsk", "plan") {
				return compatibility.Request{}, claudeError("invalid permission mode %q", value)
			}
			request.Permissions.Mode = compatibility.PermissionMode(value)
			request.Permissions.Bypass = value == "bypassPermissions"
		case "--dangerously-skip-permissions":
			request.Permissions.Bypass = true
		case "--allowedTools", "--allowed-tools", "--disallowedTools", "--disallowed-tools":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			values := strings.Fields(value)
			if strings.Contains(strings.ToLower(name), "disallowed") {
				request.Permissions.DeniedTools = append(request.Permissions.DeniedTools, values...)
			} else {
				request.Permissions.AllowedTools = append(request.Permissions.AllowedTools, values...)
			}
		case "--input-format":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "text", "stream-json") {
				return compatibility.Request{}, claudeError("invalid input format %q", value)
			}
			inputFormat = value
		case "--output-format":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "text", "json", "stream-json") {
				return compatibility.Request{}, claudeError("invalid output format %q", value)
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
			if err != nil || !json.Valid([]byte(value)) {
				return compatibility.Request{}, claudeError("invalid JSON schema")
			}
			request.Output.Schema = []byte(value)
		case "--max-turns":
			value, err := parser.value(name, argument)
			turns, parseErr := strconv.Atoi(value)
			if err != nil || parseErr != nil || turns < 1 {
				return compatibility.Request{}, claudeError("invalid max turns %q", value)
			}
			request.Limits.MaxTurns = turns
		case "--max-budget-usd":
			value, err := parser.value(name, argument)
			budget, parseErr := strconv.ParseFloat(value, 64)
			if err != nil || parseErr != nil || budget <= 0 {
				return compatibility.Request{}, claudeError("invalid budget %q", value)
			}
			request.Limits.BudgetUSD = budget
		case "--system-prompt", "--append-system-prompt":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			if name == "--system-prompt" {
				request.SystemPrompt = value
			} else {
				request.AppendSystemPrompt = value
			}
		case "--system-prompt-file", "--append-system-prompt-file":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			data, err := os.ReadFile(value)
			if err != nil {
				return compatibility.Request{}, claudeError("read prompt file: %v", err)
			}
			if name == "--system-prompt-file" {
				request.SystemPrompt = string(data)
			} else {
				request.AppendSystemPrompt = string(data)
			}
		case "--verbose":
			verbose = true
			request.Output.Verbose = true
		case "--include-partial-messages", "--replay-user-messages", "--include-hook-events":
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata[strings.TrimPrefix(name, "--")] = "true"
			includePartial = includePartial || name == "--include-partial-messages"
			replayMessages = replayMessages || name == "--replay-user-messages"
		case "--setting-sources":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
		case "--file":
			if err := consumeNoopVariadic(&parser, name, argument); err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
		case "--from-pr", "-w", "--worktree", "--teleport", "--remote-control", "--rc", "-d", "--debug":
			consumeNoopOptionalValue(&parser, argument)
		case "--sdk-url":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
			endpoint, err := url.Parse(value)
			if err != nil || !enum(endpoint.Scheme, "http", "https", "ws", "wss") || endpoint.Host == "" {
				return compatibility.Request{}, claudeError("--sdk-url requires an HTTP or WebSocket URL")
			}
			switch endpoint.Scheme {
			case "http":
				endpoint.Scheme = "ws"
			case "https":
				endpoint.Scheme = "wss"
			}
			request.Protocol = compatibility.ProtocolClaudeSDK
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["sdk-url"] = endpoint.String()
			request.Style = compatibility.ExecutionHeadless
			request.Output.Mode = compatibility.OutputStreamJSON
			verbose = true
			request.Output.Verbose = true
		case "-n", "--name", "--betas", "--tools", "--permission-prompt-tool", "--settings", "--plugin-dir", "--plugin-url", "--mcp-config", "--environment", "--cloud", "--remote", "--pool", "--remote-control-session-name-prefix", "--debug-file", "--thinking", "--thinking-display", "--append-subagent-system-prompt", "--plan-mode-instructions", "--resume-session-at", "--watch-artifact", "--prefill", "--prefill-b64", "--deep-link-origin", "--deep-link-repo", "--deep-link-last-fetch", "--deep-link-cwd-b64", "--plugin-dir-no-mcp", "--advisor", "--messaging-socket-path", "--channels", "--correlation-id", "--ref", "--on-branch", "--session-mirror", "--task-budget", "--workload", "--managed-settings", "--handle-uri", "--agent-id", "--agent-name", "--team-name", "--agent-color", "--parent-session-id", "--teammate-mode", "--agent-type":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, claudeError(err.Error())
			}
		case "--tmux", "--allow-dangerously-skip-permissions", "--forward-subagent-text", "--prompt-suggestions", "--exclude-dynamic-system-prompt-sections", "--bare", "--safe-mode", "--disable-slash-commands", "--strict-mcp-config", "--ide", "--chrome", "--no-chrome", "--brief", "--ax-screen-reader", "--init", "--init-only", "--maintenance", "-d2e", "--debug-to-stderr", "--enable-auth-status", "--resume-drops-turn", "--reply-on-resume", "--rewind-files", "--watch-artifact-no-autoreact", "--enable-auto-mode", "--dangerously-load-development-channels", "--no-home-settings", "--plan-mode-required":
		case "--":
			for parser.more() {
				prompt = append(prompt, parser.next())
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return compatibility.Request{}, claudeError("unknown option %q", argument)
			}
			prompt = append(prompt, argument)
		}
	}
	nonInteractiveOutput := nonTerminalOutput(invocation.Stdout)
	if request.Output.Mode == compatibility.OutputStreamJSON && (request.Style == compatibility.ExecutionHeadless || nonInteractiveOutput) && !verbose {
		return compatibility.Request{}, claudeError("stream-json output requires --verbose")
	}
	if includePartial && ((request.Style != compatibility.ExecutionHeadless && !nonInteractiveOutput) || request.Output.Mode != compatibility.OutputStreamJSON) {
		return compatibility.Request{}, claudeError("--include-partial-messages requires print mode and stream-json output")
	}
	if replayMessages && (inputFormat != "stream-json" || request.Output.Mode != compatibility.OutputStreamJSON) {
		return compatibility.Request{}, claudeError("--replay-user-messages requires stream-json input and output")
	}
	if inputFormat == "stream-json" {
		if request.Output.Mode != compatibility.OutputStreamJSON {
			return compatibility.Request{}, claudeError("--input-format stream-json requires --output-format stream-json")
		}
		if !verbose {
			return compatibility.Request{}, claudeError("stream-json output requires --verbose")
		}
		if len(prompt) != 0 {
			return compatibility.Request{}, claudeError("a prompt cannot be used with stream-json input")
		}
		request.Style = compatibility.ExecutionHeadless
		request.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: invocation.Stdin}
		return request, nil
	}
	request.Prompt.Text = strings.Join(prompt, " ")
	request.Prompt.Source = compatibility.PromptArguments
	if nonInteractiveOutput {
		request.Style = compatibility.ExecutionHeadless
	}
	stdin, present, err := stdinText(invocation.Stdin)
	if err != nil {
		return compatibility.Request{}, claudeError("failed to read stdin: %v", err)
	}
	if present {
		request.Style = compatibility.ExecutionHeadless
		if request.Prompt.Text == "" {
			request.Prompt.Text = stdin
			request.Prompt.Source = compatibility.PromptStdin
		} else {
			request.Prompt.Text = stdin + "\n\n" + request.Prompt.Text
			request.Prompt.Source = compatibility.PromptCombined
		}
	}
	if request.Style == compatibility.ExecutionHeadless && strings.TrimSpace(request.Prompt.Text) == "" {
		return compatibility.Request{}, claudeError("print mode requires a prompt or stdin")
	}
	return request, nil
}

func ensureMetadata(metadata map[string]string) map[string]string {
	if metadata == nil {
		return make(map[string]string)
	}
	return metadata
}

func claudeError(format string, values ...any) error {
	return parseError(1, format+"\n", values...)
}
