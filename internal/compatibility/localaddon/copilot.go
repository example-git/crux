package localaddon

import (
	"context"
	"strings"

	"github.com/example-git/crux/internal/compatibility"
)

type copilotAdapter struct{}

func (copilotAdapter) Translate(_ context.Context, invocation compatibility.Invocation) (compatibility.Request, error) {
	request := compatibility.Request{
		Source:     "copilot",
		Style:      compatibility.ExecutionInteractive,
		WorkingDir: invocation.WorkingDir,
		Session:    compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:     compatibility.Output{Mode: compatibility.OutputText},
	}
	if len(invocation.Args) != 0 && !strings.HasPrefix(invocation.Args[0], "-") && enum(invocation.Args[0], "completion", "help", "init", "login", "mcp", "plugin", "plugins", "skill", "update", "version", "app") {
		if invocation.Args[0] == "version" {
			return compatibility.Request{}, successOutput("GitHub Copilot CLI 1.0.80\n", false)
		}
		return compatibility.Request{}, copilotError("command %q is not supported by the Crux compatibility layer", invocation.Args[0])
	}
	parser := parser{args: invocation.Args}
	promptSet := false
	allowAllTools := environmentBool(invocation.Env, "COPILOT_ALLOW_ALL")
	modeSet := ""
	serverMode := false
	stdioMode := false
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput("Usage: copilot [options]\n\nOptions:\n  -i, --interactive <prompt>\n  -p, --prompt <prompt>\n  --output-format <text|json>\n", false)
		case "-v", "--version":
			return compatibility.Request{}, successOutput("GitHub Copilot CLI 1.0.80\n", false)
		case "-p", "--prompt":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			if promptSet {
				return compatibility.Request{}, copilotError("prompt specified more than once")
			}
			promptSet = true
			request.Style = compatibility.ExecutionHeadless
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptArguments, Text: value}
		case "-i", "--interactive":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			if promptSet {
				return compatibility.Request{}, copilotError("prompt specified more than once")
			}
			promptSet = true
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptArguments, Text: value}
		case "--context":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
		case "--connect", "--share", "-w", "--worktree":
			consumeNoopOptionalValue(&parser, argument)
		case "-C":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.WorkingDir = absoluteFrom(invocation.WorkingDir, value)
		case "--add-dir":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.AdditionalDirectories = append(request.AdditionalDirectories, value)
		case "--model", "--agent", "--effort", "--reasoning-effort":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			switch name {
			case "--model":
				request.Model = value
			case "--agent":
				request.Agent = value
			default:
				request.Effort = value
			}
		case "--mode":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "interactive", "plan", "autopilot") {
				return compatibility.Request{}, copilotError("invalid mode %q", value)
			}
			if modeSet != "" && modeSet != value {
				return compatibility.Request{}, copilotError("mode options are mutually exclusive")
			}
			modeSet = value
			request.Permissions.Mode = compatibility.PermissionMode(value)
		case "--plan", "--autopilot":
			value := strings.TrimPrefix(name, "--")
			if modeSet != "" && modeSet != value {
				return compatibility.Request{}, copilotError("mode options are mutually exclusive")
			}
			modeSet = value
			request.Permissions.Mode = compatibility.PermissionMode(value)
		case "--max-autopilot-continues":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["max_autopilot_continues"] = value
		case "--max-ai-credits", "--session-idle-timeout":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
		case "--dynamic-retrieval":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
		case "--continue":
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
		case "--session-id":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["session_id"] = value
		case "--output-format":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "text", "json") {
				return compatibility.Request{}, copilotError("invalid output format %q", value)
			}
			if value == "json" {
				request.Output.Mode = compatibility.OutputJSONLines
			}
		case "-s", "--silent":
			request.Output.Quiet = true
		case "--attachment":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.Attachments = append(request.Attachments, value)
		case "--allow-all", "--yolo":
			request.Permissions.Bypass = true
			allowAllTools = true
		case "--allow-all-tools":
			allowAllTools = true
			request.Permissions.AdditionalRules = append(request.Permissions.AdditionalRules, "allow-all-tools")
		case "--allow-all-paths":
			request.Permissions.AdditionalRules = append(request.Permissions.AdditionalRules, "allow-all-paths")
		case "--allow-all-urls":
			request.Permissions.AdditionalRules = append(request.Permissions.AdditionalRules, "allow-all-urls")
		case "--allow-tool", "--deny-tool", "--allow-url", "--deny-url":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			switch name {
			case "--allow-tool":
				request.Permissions.AllowedTools = append(request.Permissions.AllowedTools, value)
			case "--deny-tool":
				request.Permissions.DeniedTools = append(request.Permissions.DeniedTools, value)
			case "--allow-url":
				request.Permissions.AllowedURLs = append(request.Permissions.AllowedURLs, value)
			case "--deny-url":
				request.Permissions.DeniedURLs = append(request.Permissions.DeniedURLs, value)
			}
		case "--available-tools", "--excluded-tools", "--additional-mcp-config", "--disable-mcp-server", "--enable-mcp-server", "--add-github-mcp-tool", "--add-github-mcp-toolset":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata[strings.TrimPrefix(name, "--")] = value
		case "-n", "--name", "--secret-env-vars", "--plugin-dir", "--extension-sdk-path", "--log-dir", "--log-level", "--config-dir", "--prefer-version", "--host", "--auth-token-env":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
		case "--enable-memory", "--enable-reasoning-summaries", "--share-gist", "--allow-all-mcp-server-instructions", "--enable-all-github-mcp-tools", "--disable-builtin-mcps", "--banner", "--no-banner", "--bash-env", "--no-bash-env", "--mouse", "--no-mouse", "--no-color", "--no-auto-update", "--plain-diff", "--screen-reader", "--no-custom-instructions", "--disallow-temp-dir", "--no-ask-user", "--experimental", "--no-experimental", "--sandbox", "--no-sandbox", "--remote", "--no-remote", "--remote-export", "--no-remote-export", "--cloud", "--print-debug-info", "--with-token", "--binary-version", "--managed-server", "--no-auto-login":
		case "--server", "--headless", "--ui-server":
			serverMode = true
		case "--acp":
			request.Protocol = compatibility.ProtocolCopilotACP
			request.Style = compatibility.ExecutionHeadless
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: invocation.Stdin}
			request.Output.Mode = compatibility.OutputJSONLines
		case "--stdio":
			stdioMode = true
		case "--port":
			if _, err := parser.value(name, argument); err != nil {
				return compatibility.Request{}, copilotError(err.Error())
			}
			return compatibility.Request{}, copilotError("TCP transport is not supported by the Crux compatibility layer")
		case "--stream":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "on", "off") {
				return compatibility.Request{}, copilotError("invalid stream value %q", value)
			}
			request.Metadata = ensureMetadata(request.Metadata)
			request.Metadata["stream"] = value
		default:
			return compatibility.Request{}, copilotError("unknown option %q\nTry 'copilot --help' for more information.", argument)
		}
	}
	if request.Protocol == "" && serverMode && !promptSet {
		if !stdioMode {
			return compatibility.Request{}, copilotError("SDK server compatibility requires --stdio")
		}
		request.Protocol = compatibility.ProtocolCopilotSDK
		request.Style = compatibility.ExecutionHeadless
		request.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: invocation.Stdin}
		request.Output.Mode = compatibility.OutputJSONLines
	}
	if request.Protocol == "" && !promptSet {
		stdin, present, err := stdinText(invocation.Stdin)
		if err != nil {
			return compatibility.Request{}, copilotError("failed to read stdin: %v", err)
		}
		if present {
			request.Style = compatibility.ExecutionHeadless
			request.Prompt = compatibility.Prompt{Source: compatibility.PromptStdin, Text: stdin}
		}
	}
	if request.Style == compatibility.ExecutionHeadless && request.Protocol == "" && !allowAllTools {
		return compatibility.Request{}, copilotError("non-interactive use requires --allow-all-tools")
	}
	return request, nil
}

func copilotError(format string, values ...any) error {
	return parseError(1, format+"\n", values...)
}
