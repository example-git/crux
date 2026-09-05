package localaddon

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

func TestNativeRuntimeExecutesThroughNativeBridge(t *testing.T) {
	workingDir := nativeAPIFixture(t, "native response")
	executable := "crux"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	request := compatibility.Request{
		Source:      "claude",
		Style:       compatibility.ExecutionHeadless,
		WorkingDir:  workingDir,
		Prompt:      compatibility.Prompt{Source: compatibility.PromptArguments, Text: "prompt"},
		Session:     compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Permissions: compatibility.PermissionPolicy{Bypass: true},
		Output:      compatibility.Output{Mode: compatibility.OutputJSON},
	}
	err := (nativeRuntime{}).Execute(t.Context(), compatibility.Invocation{
		Executable: executable,
		WorkingDir: request.WorkingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1"},
		Stdout:     &stdout,
		Stderr:     &stderr,
	}, request)
	require.NoError(t, err)
	require.Empty(t, stderr.String())
	var output map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	require.Equal(t, "result", output["type"])
	require.Equal(t, "native response", output["result"])
}

func TestNativeRuntimeAllowsParsedNoopSemantics(t *testing.T) {
	workingDir := nativeAPIFixture(t, "ok")
	executable := "crux"
	err := (nativeRuntime{}).Execute(t.Context(), compatibility.Invocation{
		Executable: executable,
		WorkingDir: workingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1"},
		Stdout:     &bytes.Buffer{},
		Stderr:     &bytes.Buffer{},
	}, compatibility.Request{
		Source:                "claude",
		Style:                 compatibility.ExecutionHeadless,
		WorkingDir:            workingDir,
		Prompt:                compatibility.Prompt{Text: "prompt"},
		AdditionalDirectories: []string{"extra"},
		Agent:                 "agent",
		Effort:                "high",
		Session:               compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Attachments:           []string{"image.png"},
		Output:                compatibility.Output{Mode: compatibility.OutputText},
		Limits:                compatibility.Limits{MaxTurns: 2, BudgetUSD: 1},
		Metadata:              map[string]string{"target-option": "value"},
	})
	require.NoError(t, err)
}

func TestNativeRuntimeEmitsRealtimeOutputEnvelopes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	tests := []struct {
		name       string
		source     string
		mode       compatibility.OutputMode
		firstField string
		firstValue string
		lastField  string
		lastValue  string
	}{
		{name: "codex", source: "codex", mode: compatibility.OutputJSONLines, firstField: "type", firstValue: "thread.started", lastField: "type", lastValue: "turn.completed"},
		{name: "claude", source: "claude", mode: compatibility.OutputStreamJSON, firstField: "type", firstValue: "system", lastField: "type", lastValue: "result"},
		{name: "agy", source: "agy", mode: compatibility.OutputStreamJSON, firstField: "event", firstValue: "init", lastField: "event", lastValue: "result"},
		{name: "copilot", source: "copilot", mode: compatibility.OutputJSONLines, firstField: "type", firstValue: "session.start", lastField: "type", lastValue: "session.idle"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout bytes.Buffer
			workingDir := nativeAPIFixture(t, "first chunk second chunk")
			executable := "crux"
			err := (nativeRuntime{}).Execute(t.Context(), compatibility.Invocation{
				Executable: executable, WorkingDir: workingDir, Env: []string{compatibility.BypassEnvironment + "=1"}, Stdout: &stdout, Stderr: &bytes.Buffer{},
			}, compatibility.Request{
				Source: test.source, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
				Prompt: compatibility.Prompt{Text: "prompt"}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
				Permissions: compatibility.PermissionPolicy{Bypass: true}, Output: compatibility.Output{Mode: test.mode},
			})
			require.NoError(t, err)
			lines := decodeJSONLines(t, stdout.Bytes())
			require.Equal(t, test.firstValue, lines[0][test.firstField])
			require.Equal(t, test.lastValue, lines[len(lines)-1][test.lastField])
		})
	}
}

func TestNativeRuntimeShapesMachineReadableErrors(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	var stdout bytes.Buffer
	err := (nativeRuntime{}).Execute(t.Context(), compatibility.Invocation{
		Executable: executable,
		WorkingDir: workingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1"},
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
	}, compatibility.Request{
		Source:      "agy",
		Style:       compatibility.ExecutionHeadless,
		WorkingDir:  workingDir,
		Prompt:      compatibility.Prompt{Text: "prompt"},
		Session:     compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Permissions: compatibility.PermissionPolicy{Bypass: true},
		Model:       "missing-model",
		Output:      compatibility.Output{Mode: compatibility.OutputJSON},
	})
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, 1, exitError.Code)
	var output map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	require.Equal(t, "ERROR", output["status"])
	require.Contains(t, output["error"], "not available")
}

func TestNativeRuntimeWritesRequestedCompatibilityLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	workingDir := nativeAPIFixture(t, "ok")
	executable := "crux"
	logPath := filepath.Join(workingDir, "agy.log")
	request := compatibility.Request{
		Source: "agy", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Text: "prompt"}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output: compatibility.Output{Mode: compatibility.OutputText}, Metadata: map[string]string{"log-file": logPath},
	}
	require.NoError(t, (nativeRuntime{}).Execute(t.Context(), protocolInvocation(executable, workingDir, bytes.NewReader(nil), io.Discard), request))
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.Contains(t, string(data), "success")
}

func TestNativeRuntimeValidatesAndReportsStructuredOutput(t *testing.T) {
	workingDir := nativeAPIFixture(t, `{"answer":"ok"}`)
	executable := "crux"
	var stdout bytes.Buffer
	request := compatibility.Request{
		Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Text: "prompt"}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output: compatibility.Output{Mode: compatibility.OutputJSON, Schema: []byte(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`)},
	}
	err := (nativeRuntime{}).Execute(t.Context(), protocolInvocation(executable, workingDir, bytes.NewReader(nil), &stdout), request)
	require.NoError(t, err)
	var output map[string]any
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	require.Equal(t, map[string]any{"answer": "ok"}, output["structured_output"])

	workingDir = nativeAPIFixture(t, "not json")
	request.WorkingDir = workingDir
	stdout.Reset()
	err = (nativeRuntime{}).Execute(t.Context(), protocolInvocation(executable, workingDir, bytes.NewReader(nil), &stdout), request)
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, 1, exitError.Code)
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &output))
	require.Equal(t, "error", output["stop_reason"])
}

func TestNativePermissionModeFailsClosed(t *testing.T) {
	request := compatibility.Request{
		Permissions: compatibility.PermissionPolicy{
			Bypass:      true,
			DeniedTools: []string{"bash"},
		},
	}
	require.Equal(t, "deny", nativePermissionMode(request))
	request.Permissions.DeniedTools = nil
	require.Equal(t, "bypass", nativePermissionMode(request))
}

func TestNativeArgumentsPreserveInteractiveInitialPrompt(t *testing.T) {
	arguments, headless := nativeArguments(compatibility.Request{
		Style:       compatibility.ExecutionInteractive,
		Prompt:      compatibility.Prompt{Text: "prompt"},
		Permissions: compatibility.PermissionPolicy{Bypass: true},
	})
	require.False(t, headless)
	require.Equal(t, []string{"--yolo", "--initial-prompt", "prompt"}, arguments)
}

func TestNativeArgumentsPreserveModelAndSession(t *testing.T) {
	arguments, headless := nativeArguments(compatibility.Request{
		Style:      compatibility.ExecutionHeadless,
		Model:      "provider/model",
		SmallModel: "provider/small",
		Session:    compatibility.Session{Mode: compatibility.SessionExplicit, ID: "session"},
		Prompt:     compatibility.Prompt{Text: "prompt"},
	})
	require.True(t, headless)
	require.Equal(t, []string{"run", "--quiet", "--compatibility-permission-mode", "deny", "--model", "provider/model", "--small-model", "provider/small", "--session", "session", "prompt"}, arguments)
}
