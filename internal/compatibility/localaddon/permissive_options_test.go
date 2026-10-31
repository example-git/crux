package localaddon

import (
	"bytes"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/stretchr/testify/require"
)

func TestAdaptersIgnoreUnknownOptionsWithoutChangingRequests(t *testing.T) {
	for _, test := range []struct {
		name    string
		adapter compatibility.Adapter
		args    []string
	}{
		{"codex", codexAdapter{}, []string{"exec", "--model", "gpt-5", "--json", "literal prompt"}},
		{"claude", claudeAdapter{}, []string{"--print", "--model", "gpt-5", "--output-format", "json", "literal prompt"}},
		{"agy", agyAdapter{}, []string{"--print", "literal prompt", "--model", "gpt-5", "--output-format", "json"}},
		{"copilot", copilotAdapter{}, []string{"--prompt", "literal prompt", "--allow-all-tools", "--model", "gpt-5", "--output-format", "json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			invocation := compatibility.Invocation{Args: test.args, WorkingDir: "/workspace"}
			baseline, err := test.adapter.Translate(t.Context(), invocation)
			require.NoError(t, err)
			for _, flag := range []string{"--future-option", "--future-option=arbitrary", "--future-option=", "-Z"} {
				for _, before := range []bool{true, false} {
					invocation.Args = append(append([]string{}, test.args...), flag)
					if before {
						invocation.Args = append([]string{flag}, test.args...)
					}
					request, err := test.adapter.Translate(t.Context(), invocation)
					require.NoError(t, err, "%v", invocation.Args)
					require.Equal(t, baseline, request, "%v", invocation.Args)
				}
			}
		})
	}
}

func TestAdaptersKeepPositionalsAfterUnknownOptions(t *testing.T) {
	for _, test := range []struct {
		adapter compatibility.Adapter
		args    []string
	}{
		{codexAdapter{}, []string{"exec", "--future-option", "literal prompt"}},
		{claudeAdapter{}, []string{"--print", "--future-option", "literal prompt"}},
	} {
		request, err := test.adapter.Translate(t.Context(), compatibility.Invocation{Args: test.args, WorkingDir: "/workspace"})
		require.NoError(t, err)
		require.Equal(t, "literal prompt", request.Prompt.Text)
	}
}

func TestNoopOptionsDoNotRequireValuesOrConsumeFollowingFlags(t *testing.T) {
	for _, test := range []struct {
		name    string
		adapter compatibility.Adapter
		args    []string
	}{
		{"codex", codexAdapter{}, []string{"exec", "--color", "--add-dir", "--model", "gpt-5", "prompt", "--enable="}},
		{"claude", claudeAdapter{}, []string{"--print", "--settings", "--file", "--model", "gpt-5", "prompt", "--betas"}},
		{"agy", agyAdapter{}, []string{"--future-option", "value", "--remote-control", "--bg-updater", "--model", "gpt-5", "--print", "prompt"}},
		{"copilot", copilotAdapter{}, []string{"--context", "--port", "--model", "gpt-5", "--future-option", "value", "--prompt", "prompt", "--allow-all-tools", "--log-level="}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request, err := test.adapter.Translate(t.Context(), compatibility.Invocation{Args: test.args, WorkingDir: "/workspace"})
			require.NoError(t, err)
			require.Equal(t, "prompt", request.Prompt.Text)
			require.Equal(t, "gpt-5", request.Model)
			require.False(t, request.Permissions.Bypass)
		})
	}
}

func TestCodexIgnoresUnknownConfigKeysAlongsideSupportedSettings(t *testing.T) {
	for _, bundle := range []bool{false, true} {
		args := []string{"exec", "--config", "future.setting=arbitrary", "--config", "future.setting=other", "--config", "model=gpt-5"}
		if bundle {
			for _, override := range skySightMemgenOverrides() {
				args = append(args, "--config", override)
			}
		}
		args = append(args, "prompt")
		request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: args, WorkingDir: "/workspace"})
		require.NoError(t, err)
		require.Equal(t, "prompt", request.Prompt.Text)
		require.Equal(t, "gpt-5", request.Model)
		require.Equal(t, bundle, request.InstructionIsolation)
	}
}

func TestCodexProtocolParsersIgnoreUnknownOptions(t *testing.T) {
	for _, test := range []struct {
		args     []string
		protocol compatibility.RealtimeProtocol
	}{
		{[]string{"--future-root", "--config", "model=first", "--config", "model=second", "app-server", "--future-option", "value", "--stdio", "--ws-auth", "--another="}, compatibility.ProtocolCodexAppServer},
		{[]string{"app-server", "generate-json-schema", "--future-option", "value", "--out", "/output", "--prettier", "--another"}, compatibility.ProtocolCodexSchema},
	} {
		request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: test.args, WorkingDir: "/workspace"})
		require.NoError(t, err)
		require.Equal(t, test.protocol, request.Protocol)
		if test.protocol == compatibility.ProtocolCodexSchema {
			require.Equal(t, "/output", request.Metadata["schema-out"])
		}
	}
}

func TestIgnoredOptionsReachNativeExecutionThroughRegistry(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{"codex", []string{"--future-root", "exec", "--add-dir", "/ignored", "--config", "future.setting=true", "--future-option=value", "--json", "literal prompt"}},
		{"claude", []string{"--future-option=value", "--print", "--settings", "--output-format", "json", "literal prompt"}},
		{"agy", []string{"--remote-control", "--bg-updater", "--future-option", "value", "--print", "literal prompt", "--output-format", "json"}},
		{"copilot", []string{"--port", "12345", "--future-option", "value", "--prompt", "literal prompt", "--allow-all-tools", "--output-format", "json"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := make(chan proto.AgentMessage, 1)
			workingDir := nativeAPIFixture(t, "native option acceptance", func(_, _, _ string, request proto.AgentMessage) {
				requests <- request
			})
			registry := compatibility.NewRegistry()
			for _, registration := range builtinRegistrations() {
				require.NoError(t, registry.Register(registration))
			}
			var stdout, stderr bytes.Buffer
			exitCode, handled := registry.Dispatch(t.Context(), compatibility.Invocation{
				Executable: test.name,
				Args:       test.args,
				WorkingDir: workingDir,
				Env:        []string{},
				Stdin:      strings.NewReader(""),
				Stdout:     &stdout,
				Stderr:     &stderr,
			})
			require.True(t, handled)
			require.Zero(t, exitCode, stderr.String())
			require.Empty(t, stderr.String())
			require.Contains(t, stdout.String(), "native option acceptance")
			select {
			case request := <-requests:
				require.Equal(t, "literal prompt", request.Prompt)
			default:
				t.Fatal("ignored options prevented native execution")
			}
		})
	}
}
