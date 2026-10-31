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
	"github.com/example-git/crux/internal/proto"
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

func TestNativeRuntimeKeepsTurnInstructionsOutOfUserPrompt(t *testing.T) {
	requestSeen := make(chan proto.AgentMessage, 1)
	workingDir := nativeAPIFixture(t, `{"answer":"ok"}`, func(_, _, _ string, request proto.AgentMessage) {
		requestSeen <- request
	})
	request := compatibility.Request{
		Source:               "codex",
		Style:                compatibility.ExecutionHeadless,
		WorkingDir:           workingDir,
		Prompt:               compatibility.Prompt{Source: compatibility.PromptStdin, Text: "literal user prompt"},
		SystemPrompt:         "developer instructions",
		AppendSystemPrompt:   "additional instructions",
		InstructionIsolation: true,
		AnalysisEffort:       "medium",
		ProviderOperation:    "codex-memgen",
		Session:              compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Permissions:          compatibility.PermissionPolicy{Bypass: true},
		Output: compatibility.Output{
			Mode:   compatibility.OutputText,
			Schema: json.RawMessage(`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`),
		},
	}
	var stdout bytes.Buffer
	err := (nativeRuntime{}).Execute(t.Context(), compatibility.Invocation{
		Executable: "crux",
		WorkingDir: request.WorkingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1"},
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
	}, request)
	require.NoError(t, err)
	require.JSONEq(t, `{"answer":"ok"}`, stdout.String())
	received := <-requestSeen
	require.Equal(t, "literal user prompt", received.Prompt)
	require.Equal(t, "developer instructions\n\nadditional instructions\n\nReturn only JSON matching this JSON Schema:\n"+string(request.Output.Schema), received.TurnInstructions)
	require.True(t, received.InstructionIsolation)
	require.Equal(t, "medium", received.AnalysisEffort)
	require.Equal(t, "codex-memgen", received.ProviderOperation)
}

func TestNativeOneShotTurnInstructionsOmitEmptySections(t *testing.T) {
	require.Equal(t, "developer", nativeOneShotTurnInstructions(compatibility.Request{SystemPrompt: "developer"}))
	require.Empty(t, nativeOneShotTurnInstructions(compatibility.Request{}))
}

func TestCodexAttachmentsMaterializeRelativeImageFiles(t *testing.T) {
	workingDir := t.TempDir()
	image := filepath.Join(workingDir, "pixel.png")
	content := []byte("\x89PNG\r\n\x1a\nfixture")
	require.NoError(t, os.WriteFile(image, content, 0o600))

	attachments, err := codexAttachmentPaths(workingDir, []string{"pixel.png"})
	require.NoError(t, err)
	require.Len(t, attachments, 1)
	require.Equal(t, image, attachments[0].FilePath)
	require.Equal(t, "pixel.png", attachments[0].FileName)
	require.Equal(t, "image/png", attachments[0].MimeType)
	require.Equal(t, content, attachments[0].Content)
}

func TestCodexAttachmentsRejectMissingAndNonRegularFiles(t *testing.T) {
	workingDir := t.TempDir()
	_, err := codexAttachments(compatibility.Request{
		Source: "codex", WorkingDir: workingDir, Attachments: []string{"missing.png"},
	})
	require.ErrorContains(t, err, "inspect attachment")

	_, err = codexAttachments(compatibility.Request{
		Source: "codex", WorkingDir: workingDir, Attachments: []string{"."},
	})
	require.ErrorContains(t, err, "not a regular file")
}

func TestCodexNativeTurnInputRejectsUnsupportedAndInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		input []codexTurnInput
		match string
	}{
		{name: "missing local image path", input: []codexTurnInput{{Type: "localImage"}}, match: "localImage path must not be empty"},
		{name: "local image detail", input: []codexTurnInput{{Type: "localImage", Path: "not-image.txt", Detail: json.RawMessage(`"high"`)}}, match: "localImage.detail is not supported"},
		{name: "invalid text elements", input: []codexTurnInput{{Type: "text", Text: "prompt", TextElements: json.RawMessage(`{}`)}}, match: "text.textElements must be an array"},
		{name: "missing skill name", input: []codexTurnInput{{Type: "skill", Path: "fixture/SKILL.md"}}, match: "skill name must not be empty"},
		{name: "missing skill path", input: []codexTurnInput{{Type: "skill", Name: "fixture"}}, match: "skill path must not be empty"},
		{name: "remote image", input: []codexTurnInput{{Type: "image"}}, match: `turn input type "image" is not supported`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := codexNativeTurnInput(test.input)
			require.ErrorContains(t, err, test.match)
		})
	}
}

func TestValidateCodexTurnInputPresence(t *testing.T) {
	tests := []struct {
		name    string
		params  string
		wantErr string
	}{
		{name: "explicit empty", params: `{"input":[]}`},
		{name: "empty text", params: `{"input":[{"type":"text","text":""}]}`},
		{name: "text", params: `{"input":[{"type":"text","text":"prompt"}]}`},
		{name: "local image", params: `{"input":[{"type":"localImage","path":"image.png"}]}`},
		{name: "skill", params: `{"input":[{"type":"skill","name":"fixture","path":"fixture/SKILL.md"}]}`},
		{name: "missing input", params: `{}`, wantErr: "input must be an array"},
		{name: "null input", params: `{"input": null}`, wantErr: "input must be an array"},
		{name: "object input", params: `{"input":{}}`, wantErr: "input must be an array"},
		{name: "null item", params: `{"input":[null]}`, wantErr: "input[0] must be an object"},
		{name: "missing type", params: `{"input":[{"text":"prompt"}]}`, wantErr: "input[0].type must be a non-empty string"},
		{name: "missing text", params: `{"input":[{"type":"text"}]}`, wantErr: "input[0].text is required"},
		{name: "null text", params: `{"input":[{"type":"text","text":null}]}`, wantErr: "input[0].text is required"},
		{name: "missing local image path", params: `{"input":[{"type":"localImage"}]}`, wantErr: "input[0].path is required"},
		{name: "null local image path", params: `{"input":[{"type":"localImage","path":null}]}`, wantErr: "input[0].path is required"},
		{name: "missing skill name", params: `{"input":[{"type":"skill","path":"fixture/SKILL.md"}]}`, wantErr: "input[0].name is required"},
		{name: "null skill name", params: `{"input":[{"type":"skill","name":null,"path":"fixture/SKILL.md"}]}`, wantErr: "input[0].name is required"},
		{name: "missing skill path", params: `{"input":[{"type":"skill","name":"fixture"}]}`, wantErr: "input[0].path is required"},
		{name: "null skill path", params: `{"input":[{"type":"skill","name":"fixture","path":null}]}`, wantErr: "input[0].path is required"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateCodexTurnInputPresence(json.RawMessage(test.params))
			if test.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, test.wantErr)
		})
	}
}

func TestCodexNativeTurnInputPreservesOrderAndExplicitEmpty(t *testing.T) {
	parts, err := codexNativeTurnInput([]codexTurnInput{
		{Type: "text", Text: "before", TextElements: json.RawMessage(`[{"byteRange":{"start":0,"end":6},"placeholder":"<note>"}]`)},
		{Type: "localImage", Path: "first.png"},
		{Type: "text", Text: "after"},
		{Type: "skill", Name: "fixture", Path: "fixture/SKILL.md"},
		{Type: "localImage", Path: "second.png"},
	})
	require.NoError(t, err)
	require.Equal(t, []proto.AgentInputPart{
		{Type: "text", Text: "before", TextElements: []any{map[string]any{"byteRange": map[string]any{"start": float64(0), "end": float64(6)}, "placeholder": "<note>"}}},
		{Type: "local_file", Path: "first.png"},
		{Type: "text", Text: "after", TextElements: []any{}},
		{Type: "skill", Text: "fixture", Path: "fixture/SKILL.md"},
		{Type: "local_file", Path: "second.png"},
	}, parts)
	content, err := codexUserMessageContent([]codexTurnInput{
		{Type: "text", Text: "before", TextElements: json.RawMessage(`[{"byteRange":{"start":0,"end":6},"placeholder":"<note>"}]`)},
		{Type: "localImage", Path: "first.png"},
		{Type: "skill", Name: "fixture", Path: "fixture/SKILL.md"},
	})
	require.NoError(t, err)
	require.Equal(t, "before", content[0].(map[string]any)["text"])
	require.Equal(t, []any{map[string]any{"byteRange": map[string]any{"start": float64(0), "end": float64(6)}, "placeholder": "<note>"}}, content[0].(map[string]any)["textElements"])
	require.Equal(t, map[string]any{"type": "localImage", "path": "first.png"}, content[1])
	require.Equal(t, map[string]any{"type": "skill", "name": "fixture", "path": "fixture/SKILL.md"}, content[2])

	empty, err := codexNativeTurnInput([]codexTurnInput{})
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)
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
