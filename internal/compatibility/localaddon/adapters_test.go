package localaddon

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

func TestCodexAdapterTranslatesExecInvocation(t *testing.T) {
	adapter := codexAdapter{}
	request, err := adapter.Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--model", "gpt-test", "--cd", "sub", "--sandbox", "workspace-write", "--json", "review this"},
		WorkingDir: "/workspace",
		Stdin:      bytes.NewBufferString("extra context"),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "gpt-test", request.Model)
	require.Equal(t, filepath.Join("/workspace", "sub"), request.WorkingDir)
	require.Equal(t, compatibility.SandboxMode("workspace-write"), request.Permissions.Sandbox)
	require.Equal(t, compatibility.OutputJSONLines, request.Output.Mode)
	require.Equal(t, compatibility.PromptCombined, request.Prompt.Source)
	require.Contains(t, request.Prompt.Text, "review this")
	require.Contains(t, request.Prompt.Text, "extra context")
}

func TestCodexAdapterAcceptsGlobalOptionsBeforeExec(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--model", "gpt-test", "exec", "--dangerously-bypass-approvals-and-sandbox", "prompt"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "gpt-test", request.Model)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.True(t, request.Permissions.Bypass)
}

func TestCodexAdapterEnablesAppServerStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"id\":1,\"method\":\"initialize\"}\n")
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"app-server", "--listen", "stdio://"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCodexAdapterGeneratesAppServerSchemas(t *testing.T) {
	workingDir := t.TempDir()
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "generate-json-schema", "--out", "schemas", "--experimental"}, WorkingDir: workingDir})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexSchema, request.Protocol)
	require.NoError(t, runCodexSchemaGenerator(request))
	data, err := os.ReadFile(filepath.Join(workingDir, "schemas", "codex_app_server_protocol.v2.schemas.json"))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(data))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadStartResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ConfigReadResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "PluginListResponse.json"))
	clientSchema, err := os.ReadFile(filepath.Join(workingDir, "schemas", "ClientRequest.json"))
	require.NoError(t, err)
	require.Contains(t, string(clientSchema), `"config/read"`)
	require.Contains(t, string(clientSchema), `"plugin/list"`)
	require.Contains(t, string(clientSchema), `"modelProvider"`)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "generate-ts", "--out", "types"}, WorkingDir: workingDir})
	require.NoError(t, err)
	require.NoError(t, runCodexSchemaGenerator(request))
	require.FileExists(t, filepath.Join(workingDir, "types", "ClientRequest.ts"))
}

func TestCodexAdapterAcceptsAppServerStdioFlag(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "--stdio"}, WorkingDir: "/workspace", Stdin: bytes.NewReader(nil)})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
}

func TestCodexAdapterDetectsSubcommandsAfterLeadingGlobalFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "model=\"o3\"", "--enable", "feature", "-m", "gpt-test", "app-server", "--stdio"},
		WorkingDir: "/workspace", Stdin: bytes.NewReader(nil),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
	require.Equal(t, "gpt-test", request.Model)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "exec", "review this"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "review this", request.Prompt.Text)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "resume", "session-id"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "fork", "session-id"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionFork, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "review"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "Review the current changes.", request.Prompt.Text)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "exec", "resume", "session-id", "continue please"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)
	require.Equal(t, "continue please", request.Prompt.Text)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", "x=1", "login"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 2, `command "login" is not supported`)
}

func TestCodexAdapterAcceptsResearchedAppServerNoopFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"app-server", "--stdio", "-c", "model=\"o3\"", "--enable", "feature", "--disable", "other",
			"--strict-config", "--analytics-default-enabled", "--code-mode-host", "https://example.invalid",
			"--ws-auth", "capability-token", "--ws-token-file", "/tmp/token", "--ws-token-sha256", "deadbeef",
			"--ws-shared-secret-file", "/tmp/secret", "--ws-issuer", "issuer", "--ws-audience", "audience",
			"--ws-max-clock-skew-seconds", "30",
		},
		WorkingDir: "/workspace", Stdin: bytes.NewReader(nil),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "--ws-auth", "not-a-real-mode"}, WorkingDir: "/workspace"})
	require.NoError(t, err)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "daemon", "start"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, `app-server command "daemon" is not supported`)
	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "proxy"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, `app-server command "proxy" is not supported`)
}

func TestCodexAdapterRejectsInvalidOutputSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))
	_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"exec", "--output-schema", path, "prompt"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, "not valid JSON")
}

func TestCodexAdapterUsesExitCodeTwoForInvalidValues(t *testing.T) {
	_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--sandbox", "invalid", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 2, "invalid value")
}

func TestClaudeAdapterTranslatesPrintAndValidatesStreams(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--model", "claude-test", "--effort", "high", "--permission-mode", "bypassPermissions", "--output-format", "json", "prompt"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "claude-test", request.Model)
	require.Equal(t, "high", request.Effort)
	require.True(t, request.Permissions.Bypass)
	require.Equal(t, compatibility.OutputJSON, request.Output.Mode)

	_, err = (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--input-format", "stream-json", "--output-format", "text"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires --output-format stream-json")
}

func TestClaudeAdapterCombinesStdinAndPrompt(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "prompt"},
		WorkingDir: "/workspace",
		Stdin:      bytes.NewBufferString("stdin"),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.PromptCombined, request.Prompt.Source)
	require.Equal(t, "stdin\n\nprompt", request.Prompt.Text)
}

func TestAgyAdapterTranslatesHeadlessInvocation(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "prompt", "--model", "gemini-test", "--effort", "medium", "--mode", "plan", "--output-format", "json", "--print-timeout", "30s"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.Equal(t, "gemini-test", request.Model)
	require.Equal(t, "medium", request.Effort)
	require.Equal(t, compatibility.PermissionMode("plan"), request.Permissions.Mode)
	require.Equal(t, compatibility.OutputJSON, request.Output.Mode)
	require.Equal(t, 30_000_000_000, int(request.Limits.Timeout))
}

func TestAgyAdapterDefaultsPrintTimeoutAndNormalizesPrimitiveSchema(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--print", "prompt", "--json-schema", "string"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, request.Limits.Timeout)
	require.JSONEq(t, `{"type":"string"}`, string(request.Output.Schema))
}

func TestAgyAdapterPreservesStreamJSONInput(t *testing.T) {
	stdin := bytes.NewBufferString("{\"event\":\"user\"}\n")
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--input-format", "stream-json", "--output-format", "stream-json"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterAcceptsEnvironmentAuthorization(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt"},
		WorkingDir: "/workspace",
		Env:        []string{"COPILOT_ALLOW_ALL=true"},
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
}

func TestCopilotAdapterEnablesACPStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n")
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--stdio", "--acp"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCopilotACP, request.Protocol)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterEnablesSDKStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"connect\"}\n")
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--headless", "--stdio", "--no-auto-update"}, WorkingDir: "/workspace", Stdin: stdin})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCopilotSDK, request.Protocol)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterAcceptsPipedPrompt(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--allow-all-tools"}, WorkingDir: "/workspace", Stdin: bytes.NewBufferString("prompt")})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, compatibility.PromptStdin, request.Prompt.Source)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestCopilotAdapterAcceptsSpaceSeparatedResumeValue(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--resume", "session-name"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-name", request.Session.ID)
}

func TestCopilotAdapterRequiresHeadlessAuthorization(t *testing.T) {
	_, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires --allow-all-tools")

	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt", "--allow-all", "--model", "copilot-test", "--output-format", "json", "--continue"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.True(t, request.Permissions.Bypass)
	require.Equal(t, "copilot-test", request.Model)
	require.Equal(t, compatibility.OutputJSONLines, request.Output.Mode)
	require.Equal(t, compatibility.SessionLatest, request.Session.Mode)
}

func TestClaudeAdapterValidatesPartialStreamOutput(t *testing.T) {
	_, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--include-partial-messages", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires print mode and stream-json output")
}

func TestCodexAdapterAcceptsResearchedNoopFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"exec", "--config", "key=value", "--enable", "feature", "--disable", "other", "--remote", "localhost:1",
			"--remote-auth-token-env", "TOKEN", "--strict-config", "--oss", "--local-provider", "provider", "--profile", "profile",
			"--approve-for-me", "--dangerously-bypass-hook-trust", "--search", "--thread-source", "cli", "--ignore-user-config",
			"--ignore-rules", "--color", "auto", "--codex-run-as-apply-patch", "prompt",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestClaudeAdapterAcceptsResearchedNoopFlags(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"--print", "--agents", `{"reviewer":{}}`, "--autocompact", "auto", "--max-thinking-tokens", "100",
			"--from-pr=12", "--name", "session", "--worktree=branch", "--tmux", "--file", "file.txt", "--betas", "beta",
			"--tools", "default", "--permission-prompt-tool", "prompt", "--allow-dangerously-skip-permissions",
			"--exclude-dynamic-system-prompt-sections", "--forward-subagent-text", "--prompt-suggestions",
			"--settings", "settings.json", "--setting-sources", "user", "--bare", "--safe-mode", "--disable-slash-commands",
			"--plugin-dir", "plugin", "--plugin-url", "https://example.invalid/plugin", "--mcp-config", "mcp.json", "--strict-mcp-config",
			"--ide", "--chrome", "--no-chrome", "--cloud=task", "--environment", "env", "--teleport=session",
			"--remote-control=name", "--remote-control-session-name-prefix", "prefix", "--debug=api", "--debug-file", "debug.log",
			"--brief", "--ax-screen-reader", "--thinking", "adaptive", "--thinking-display", "summary",
			"--append-subagent-system-prompt", "extra", "--plan-mode-instructions", "plan", "--resume-session-at", "turn",
			"--resume-drops-turn", "--reply-on-resume", "--rewind-files", "--watch-artifact", "artifact",
			"--watch-artifact-no-autoreact", "--prefill", "text", "--prefill-b64", "dGV4dA==", "--deep-link-origin", "origin",
			"--deep-link-repo", "repo", "--deep-link-last-fetch", "now", "--deep-link-cwd-b64", "L3RtcA==",
			"--plugin-dir-no-mcp", "plugin", "--advisor", "advisor", "--enable-auto-mode", "--messaging-socket-path", "socket",
			"--channels", "channel", "--dangerously-load-development-channels", "--no-home-settings", "--remote=host", "--pool=pool",
			"--correlation-id", "id", "--ref", "ref", "--on-branch", "branch", "--rc", "--init", "--init-only", "--maintenance",
			"--debug-to-stderr", "--session-mirror", "mirror", "--enable-auth-status", "--task-budget", "10", "--workload", "work",
			"--managed-settings", "managed.json", "--sdk-url", "https://example.invalid/sdk", "--handle-uri", "claude://test",
			"--agent-id", "id", "--agent-name", "name", "--team-name", "team", "--agent-color", "blue", "--plan-mode-required",
			"--parent-session-id", "parent", "--teammate-mode", "mode", "--agent-type", "type", "prompt",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestNoopFlagsAcceptAnyExplicitValue(t *testing.T) {
	_, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--autocompact", "not-a-real-mode", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--setting-sources", "user,invalid", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--context", "invalid", "--allow-all-tools", "-p", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--dynamic-retrieval", "skills=maybe", "--allow-all-tools", "-p", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"exec", "--color", "not-a-real-color", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
}

func TestAgyAdapterDefaultsStreamTurnsToFiveMinutes(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--input-format", "stream-json", "--output-format", "stream-json"},
		WorkingDir: "/workspace",
		Stdin:      strings.NewReader(""),
	})
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, request.Limits.Timeout)
}

func TestAgyAdapterRejectsUnsupportedModeEntrypoints(t *testing.T) {
	for _, flag := range []string{"--remote-control", "--bg-updater"} {
		_, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
			Args:       []string{flag, "--print", "prompt"},
			WorkingDir: "/workspace",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), flag+" is not supported")
	}
}

func TestCopilotAdapterAcceptsResearchedNoopFlags(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"--prompt", "prompt", "--allow-all-tools", "--connect=session", "--context", "long_context", "--name", "name",
			"--enable-memory", "--share=transcript", "--share-gist", "--max-ai-credits", "10", "--enable-reasoning-summaries",
			"--plugin-dir", "plugin", "--secret-env-vars", "SECRET", "--disallow-temp-dir", "--disable-builtin-mcps",
			"--enable-mcp-server", "server", "--allow-all-mcp-server-instructions", "--add-github-mcp-tool", "issues",
			"--add-github-mcp-toolset", "all", "--enable-all-github-mcp-tools", "--banner", "--no-banner", "--bash-env",
			"--no-bash-env", "--mouse", "--no-mouse", "--experimental", "--no-experimental", "--extension-sdk-path", "sdk",
			"--log-dir", "logs", "--log-level", "debug", "--config-dir", "config", "--sandbox", "--no-sandbox",
			"--worktree=branch", "--remote", "--no-remote", "--remote-export", "--no-remote-export", "--with-token",
			"--binary-version", "--prefer-version", "1.0.80", "--print-debug-info", "--session-idle-timeout", "60",
			"--dynamic-retrieval", "skills=on", "--cloud", "--server", "--headless", "--ui-server", "--host", "127.0.0.1",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestAdaptersReturnTargetShapedHelp(t *testing.T) {
	tests := []struct {
		adapter compatibility.Adapter
		name    string
		stderr  bool
	}{
		{adapter: codexAdapter{}, name: "codex"},
		{adapter: claudeAdapter{}, name: "claude"},
		{adapter: agyAdapter{}, name: "agy", stderr: true},
		{adapter: copilotAdapter{}, name: "copilot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.adapter.Translate(context.Background(), compatibility.Invocation{Args: []string{"--help"}, WorkingDir: "/workspace"})
			exitError, ok := errors.AsType[*compatibility.ExitError](err)
			require.True(t, ok)
			require.Zero(t, exitError.Code)
			if test.stderr {
				require.NotEmpty(t, exitError.Stderr)
				require.Empty(t, exitError.Stdout)
			} else {
				require.NotEmpty(t, exitError.Stdout)
				require.Empty(t, exitError.Stderr)
			}
		})
	}
}

func requireExitError(t *testing.T, err error, code int, message string) {
	t.Helper()
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, code, exitError.Code)
	require.Contains(t, exitError.Stderr, message)
}
