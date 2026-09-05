package localaddon

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
)

type codexAdapter struct{}

func (codexAdapter) Translate(_ context.Context, invocation compatibility.Invocation) (compatibility.Request, error) {
	request := compatibility.Request{
		Source:     "codex",
		Style:      compatibility.ExecutionInteractive,
		WorkingDir: invocation.WorkingDir,
		Session:    compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:     compatibility.Output{Mode: compatibility.OutputText},
	}
	arguments := invocation.Args
	reviewMode := false
	subcommandResolved := false

	parser := parser{args: arguments}
	var prompt []string
	readStdin := false
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput("Usage: codex [OPTIONS] [PROMPT]\n       codex exec [OPTIONS] [PROMPT]\n", false)
		case "-V", "--version":
			return compatibility.Request{}, successOutput("codex-cli 0.149.1\n", false)
		case "-m", "--model":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			request.Model = value
		case "-c", "--config", "--enable", "--disable", "--remote", "--remote-auth-token-env", "--local-provider", "-p", "--profile", "--thread-source", "--color":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
		case "--strict-config", "--oss", "--approve-for-me", "--dangerously-bypass-hook-trust", "--search", "--ignore-user-config", "--ignore-rules", "--codex-run-as-apply-patch", "--codex-run-as-arg0-exec-helper", "--codex-run-as-fs-helper", "--all", "--include-non-interactive", "--uncommitted":
		case "--base", "--commit", "--title":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
		case "-C", "--cd":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			request.WorkingDir = absoluteFrom(invocation.WorkingDir, value)
		case "--add-dir":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			request.AdditionalDirectories = append(request.AdditionalDirectories, value)
		case "-i", "--image":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			request.Attachments = append(request.Attachments, value)
		case "-s", "--sandbox":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "read-only", "workspace-write", "danger-full-access") {
				return compatibility.Request{}, codexParseError(fmt.Errorf("invalid value %q for --sandbox", value))
			}
			request.Permissions.Sandbox = compatibility.SandboxMode(value)
		case "-a", "--ask-for-approval":
			value, err := parser.value(name, argument)
			if err != nil || !enum(value, "on-request", "never") {
				return compatibility.Request{}, codexParseError(fmt.Errorf("invalid value %q for --ask-for-approval", value))
			}
			request.Permissions.Mode = compatibility.PermissionMode(value)
		case "--dangerously-bypass-approvals-and-sandbox":
			request.Permissions.Bypass = true
		case "--json":
			request.Output.Mode = compatibility.OutputJSONLines
		case "--output-schema":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			data, err := os.ReadFile(value)
			if err != nil {
				return compatibility.Request{}, parseError(2, "error: read output schema: %v\n", err)
			}
			if !json.Valid(data) {
				return compatibility.Request{}, parseError(2, "error: output schema is not valid JSON\n")
			}
			request.Output.Schema = data
		case "-o", "--output-last-message":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			request.Output.LastMessagePath = value
		case "--ephemeral":
			request.Session.Persistent = false
		case "--last":
			request.Session.Mode = compatibility.SessionLatest
		case "--skip-git-repo-check", "--no-alt-screen":
		case "-":
			readStdin = true
		case "--":
			for parser.more() {
				prompt = append(prompt, parser.next())
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return compatibility.Request{}, parseError(2, "error: unexpected argument %q found\n\nUsage: codex [OPTIONS] [PROMPT]\n", argument)
			}
			if len(prompt) == 0 && !subcommandResolved {
				switch argument {
				case "app-server":
					return translateCodexAppServer(invocation, request, parser.args[parser.index:])
				case "exec", "e":
					subcommandResolved = true
					request.Style = compatibility.ExecutionHeadless
					readStdin = true
					if parser.more() {
						switch parser.args[parser.index] {
						case "resume":
							request.Session.Mode = compatibility.SessionExplicit
							parser.next()
						case "fork":
							request.Session.Mode = compatibility.SessionFork
							parser.next()
						case "review":
							reviewMode = true
							parser.next()
						}
					}
					continue
				case "review":
					subcommandResolved = true
					request.Style = compatibility.ExecutionHeadless
					reviewMode = true
					continue
				case "fork":
					subcommandResolved = true
					request.Session.Mode = compatibility.SessionFork
					continue
				case "resume":
					subcommandResolved = true
					request.Session.Mode = compatibility.SessionExplicit
					continue
				}
				subcommandResolved = true
				if codexUnsupportedCommand(argument) {
					return compatibility.Request{}, parseError(2, "error: command %q is not supported by the Crux compatibility layer\n", argument)
				}
			}
			if (request.Session.Mode == compatibility.SessionExplicit || request.Session.Mode == compatibility.SessionFork) && request.Session.ID == "" {
				request.Session.ID = argument
			} else {
				prompt = append(prompt, argument)
			}
		}
	}
	request.Prompt.Text = strings.Join(prompt, " ")
	if reviewMode && strings.TrimSpace(request.Prompt.Text) == "" {
		request.Prompt.Text = "Review the current changes."
	}
	if request.Style == compatibility.ExecutionHeadless && request.Session.Mode == compatibility.SessionFork && strings.TrimSpace(request.Prompt.Text) == "" {
		request.Prompt.Text = "Continue the forked session."
	}
	request.Prompt.Source = compatibility.PromptArguments
	if readStdin {
		stdin, present, err := stdinText(invocation.Stdin)
		if err != nil {
			return compatibility.Request{}, parseError(1, "failed to read stdin: %v\n", err)
		}
		if present {
			switch request.Prompt.Text {
			case "", "-":
				request.Prompt.Text = stdin
				request.Prompt.Source = compatibility.PromptStdin
			default:
				request.Prompt.Text += "\n\n<stdin>\n" + stdin + "\n</stdin>"
				request.Prompt.Source = compatibility.PromptCombined
			}
		}
	}
	if request.Style == compatibility.ExecutionHeadless && strings.TrimSpace(request.Prompt.Text) == "" {
		return compatibility.Request{}, parseError(2, "error: no prompt provided\n\nUsage: codex exec [OPTIONS] [PROMPT]\n")
	}
	if err := normalizeCodexCLIPolicy(&request); err != nil {
		return compatibility.Request{}, codexParseError(err)
	}
	return request, nil
}

func normalizeCodexCLIPolicy(request *compatibility.Request) error {
	approval := string(request.Permissions.Mode)
	if approval == "" {
		approval = "on-request"
	}
	sandbox := string(request.Permissions.Sandbox)
	if sandbox == "" {
		sandbox = "workspace-write"
	}
	if request.Permissions.Bypass {
		if (request.Permissions.Mode != "" && approval != "never") || (request.Permissions.Sandbox != "" && sandbox != "danger-full-access") {
			return fmt.Errorf("--dangerously-bypass-approvals-and-sandbox conflicts with the requested approval or sandbox policy")
		}
		approval = "never"
		sandbox = "danger-full-access"
	}
	policy, err := parseCodexExecutionPolicy(approval, "user", json.RawMessage(strconv.Quote(sandbox)), codexExecutionPolicy{})
	if err != nil {
		return err
	}
	request.Permissions.Mode = compatibility.PermissionMode(policy.approvalPolicy)
	request.Permissions.Sandbox = compatibility.SandboxMode(policy.sandbox)
	request.Permissions.Bypass = policy.permissionMode == proto.AgentPermissionBypass
	return nil
}

func translateCodexAppServer(invocation compatibility.Invocation, base compatibility.Request, arguments []string) (compatibility.Request, error) {
	invocation.WorkingDir = base.WorkingDir
	if len(arguments) != 0 && enum(arguments[0], "generate-json-schema", "generate-internal-json-schema", "generate-ts") {
		return translateCodexSchemaGenerator(invocation, arguments[0], arguments[1:])
	}
	if len(arguments) != 0 && enum(arguments[0], "daemon", "proxy") {
		return compatibility.Request{}, parseError(2, "error: app-server command %q is not supported by the Crux compatibility layer\n", arguments[0])
	}
	parser := parser{args: arguments}
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput("Usage: codex app-server [OPTIONS]\n\nOptions:\n  --listen <URI>\n", false)
		case "--stdio", "--strict-config", "--analytics-default-enabled":
		case "--listen":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			if value != "stdio://" {
				return compatibility.Request{}, parseError(2, "error: only the app-server stdio transport is supported by the Crux compatibility layer\n")
			}
		case "-c", "--config", "--enable", "--disable", "--code-mode-host", "--ws-auth", "--ws-token-file", "--ws-token-sha256", "--ws-shared-secret-file", "--ws-issuer", "--ws-audience", "--ws-max-clock-skew-seconds":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
		default:
			return compatibility.Request{}, parseError(2, "error: app-server option %q is not supported by the Crux compatibility layer\n", argument)
		}
	}
	base.Source = "codex"
	base.Protocol = compatibility.ProtocolCodexAppServer
	base.Style = compatibility.ExecutionHeadless
	base.Prompt = compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: invocation.Stdin}
	base.Session = compatibility.Session{Mode: compatibility.SessionNew, Persistent: base.Session.Persistent}
	base.Output = compatibility.Output{Mode: compatibility.OutputJSONLines}
	return base, nil
}

func translateCodexSchemaGenerator(invocation compatibility.Invocation, command string, arguments []string) (compatibility.Request, error) {
	parser := parser{args: arguments}
	out := ""
	for parser.more() {
		argument := parser.next()
		name := optionName(argument)
		switch name {
		case "-h", "--help":
			return compatibility.Request{}, successOutput(fmt.Sprintf("Usage: codex app-server %s [OPTIONS] --out <DIR>\n", command), false)
		case "-o", "--out":
			value, err := parser.value(name, argument)
			if err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
			out = absoluteFrom(invocation.WorkingDir, value)
		case "-c", "--config", "--enable", "--disable", "-p", "--prettier":
			if err := consumeNoopValue(&parser, name, argument); err != nil {
				return compatibility.Request{}, codexParseError(err)
			}
		case "--experimental":
		default:
			return compatibility.Request{}, parseError(2, "error: app-server schema option %q is not supported by the Crux compatibility layer\n", argument)
		}
	}
	if out == "" {
		return compatibility.Request{}, parseError(2, "error: the following required arguments were not provided:\n  --out <DIR>\n")
	}
	return compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexSchema, Style: compatibility.ExecutionHeadless,
		WorkingDir: invocation.WorkingDir, Metadata: map[string]string{"schema-command": command, "schema-out": out},
	}, nil
}

func codexUnsupportedCommand(argument string) bool {
	return enum(argument, "agents", "login", "logout", "mcp", "plugin", "mcp-server", "remote-control", "app", "completion", "update", "doctor", "sandbox", "debug", "apply", "queue", "archive", "unarchive", "delete", "cloud", "exec-server", "features", "execpolicy", "responses-api-proxy", "stdio-to-uds", "migrate-rollouts", "help", "a")
}

func codexParseError(err error) error {
	return parseError(2, "error: %v\n\nUsage: codex [OPTIONS] [PROMPT]\n", err)
}
