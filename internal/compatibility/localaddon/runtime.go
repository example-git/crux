package localaddon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/example-git/crux/foundation/schema"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
)

type nativeRuntime struct{}

func Register() error {
	runtime := nativeRuntime{}
	registrations := []compatibility.Registration{
		{Name: "codex", Adapter: codexAdapter{}, Runtime: runtime},
		{Name: "claude", Adapter: claudeAdapter{}, Runtime: runtime},
		{Name: "agy", Adapter: agyAdapter{}, Runtime: runtime},
		{Name: "copilot", Adapter: copilotAdapter{}, Runtime: runtime},
	}
	for _, registration := range registrations {
		if err := compatibility.Register(registration); err != nil {
			return err
		}
	}
	return nil
}

func (nativeRuntime) Execute(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	switch request.Protocol {
	case compatibility.ProtocolCodexAppServer:
		return runCodexAppServer(ctx, invocation, request)
	case compatibility.ProtocolCodexSchema:
		return runCodexSchemaGenerator(request)
	case compatibility.ProtocolClaudeSDK:
		return runClaudeSDK(ctx, invocation, request)
	case compatibility.ProtocolCopilotACP:
		return runCopilotACP(ctx, invocation, request)
	case compatibility.ProtocolCopilotSDK:
		return runCopilotSDK(ctx, invocation, request)
	}
	if request.Prompt.Source == compatibility.PromptStreamJSON {
		switch request.Source {
		case "claude":
			return runClaudeStream(ctx, invocation, request)
		case "agy":
			runErr := runAgyStream(ctx, invocation, request)
			logErr := writeCompatibilityLog(request, runErr)
			if runErr != nil {
				return runErr
			}
			return logErr
		default:
			return errors.New("streaming JSON input is not supported for this compatibility target")
		}
	}

	workingDir := request.WorkingDir
	if workingDir == "" {
		workingDir = invocation.WorkingDir
	}
	info, err := os.Stat(workingDir)
	if err != nil {
		return fmt.Errorf("inspect working directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("working directory %q is not a directory", workingDir)
	}

	runContext := ctx
	cancel := func() {}
	if request.Limits.Timeout > 0 {
		runContext, cancel = context.WithTimeout(ctx, request.Limits.Timeout)
	}
	defer cancel()

	if request.Style == compatibility.ExecutionHeadless && (request.Source == "codex" || request.Source == "claude" || request.Source == "agy" || request.Source == "copilot") {
		return runNativeCompatibilityOneShot(runContext, invocation, request)
	}
	if request.Source == "codex" && request.Style == compatibility.ExecutionInteractive && request.Session.Mode == compatibility.SessionFork {
		return runInteractiveCodexFork(runContext, invocation, request)
	}

	arguments, headless := nativeArguments(request)
	if !headless {
		return runNative(runContext, invocation, workingDir, arguments, invocation.Stdin, invocation.Stdout, invocation.Stderr)
	}

	var output bytes.Buffer
	var diagnostic bytes.Buffer
	var targetStream *oneShotStream
	var nativeOutput io.Writer = &output
	if usesOneShotStream(request) {
		targetStream = newOneShotStream(invocation.Stdout, request)
		if err := targetStream.start(); err != nil {
			return err
		}
		nativeOutput = targetStream
	}
	if err := runNative(runContext, invocation, workingDir, arguments, bytes.NewReader(nil), nativeOutput, &diagnostic); err != nil {
		message := strings.TrimSpace(diagnostic.String())
		timedOut := errors.Is(runContext.Err(), context.DeadlineExceeded)
		if timedOut {
			message = fmt.Sprintf("execution timed out after %s", request.Limits.Timeout)
		}
		if targetStream != nil {
			if writeErr := targetStream.fail(message); writeErr != nil {
				return writeErr
			}
		} else if request.Output.Mode != "" && request.Output.Mode != compatibility.OutputText {
			if writeErr := writeErrorOutput(invocation.Stdout, request, message); writeErr != nil {
				return writeErr
			}
		} else if message != "" {
			_, _ = fmt.Fprintln(invocation.Stderr, message)
		}
		if timedOut {
			return &compatibility.ExitError{Code: 1}
		}
		return err
	}
	if diagnostic.Len() != 0 {
		_, _ = diagnostic.WriteTo(invocation.Stderr)
	}
	text := strings.TrimSuffix(output.String(), "\n")
	if targetStream != nil {
		text = strings.TrimSuffix(targetStream.capture.String(), "\n")
	}
	if _, err := structuredResult(request, text); err != nil {
		message := "structured output validation failed: " + err.Error()
		if targetStream != nil {
			if writeErr := targetStream.fail(message); writeErr != nil {
				return writeErr
			}
		} else if request.Output.Mode != "" && request.Output.Mode != compatibility.OutputText {
			if writeErr := writeErrorOutput(invocation.Stdout, request, message); writeErr != nil {
				return writeErr
			}
		} else {
			_, _ = fmt.Fprintln(invocation.Stderr, message)
		}
		return &compatibility.ExitError{Code: 1}
	}
	if request.Output.LastMessagePath != "" {
		if err := writeLastMessage(request.Output.LastMessagePath, text); err != nil {
			return err
		}
	}
	if targetStream != nil {
		if err := targetStream.complete(); err != nil {
			return err
		}
	} else if err := writeOutput(invocation.Stdout, request, text); err != nil {
		return err
	}
	return writeCompatibilityLog(request, nil)
}

func usesOneShotStream(request compatibility.Request) bool {
	switch request.Source {
	case "codex", "copilot":
		return request.Output.Mode == compatibility.OutputJSONLines
	case "claude", "agy":
		return request.Output.Mode == compatibility.OutputStreamJSON
	default:
		return false
	}
}

func nativeArguments(request compatibility.Request) ([]string, bool) {
	headless := request.Style == compatibility.ExecutionHeadless
	arguments := make([]string, 0, 10)
	if headless {
		arguments = append(arguments, "run", "--quiet", "--compatibility-permission-mode", nativePermissionMode(request))
		if request.Model != "" {
			arguments = append(arguments, "--model", request.Model)
		}
		if request.SmallModel != "" {
			arguments = append(arguments, "--small-model", request.SmallModel)
		}
	} else {
		if request.Permissions.Bypass {
			arguments = append(arguments, "--yolo")
		}
		if request.Prompt.Text != "" {
			arguments = append(arguments, "--initial-prompt", request.Prompt.Text)
		}
	}
	switch request.Session.Mode {
	case compatibility.SessionLatest:
		arguments = append(arguments, "--continue")
	case compatibility.SessionExplicit:
		arguments = append(arguments, "--session", request.Session.ID)
	}
	if headless {
		prompt := request.Prompt.Text
		if len(request.Output.Schema) != 0 {
			prompt = "Return only JSON matching this JSON Schema:\n" + string(request.Output.Schema) + "\n\n" + prompt
		}
		if request.SystemPrompt != "" {
			prompt = "<system>\n" + request.SystemPrompt + "\n</system>\n\n" + prompt
		}
		if request.AppendSystemPrompt != "" {
			prompt = "<additional-system>\n" + request.AppendSystemPrompt + "\n</additional-system>\n\n" + prompt
		}
		arguments = append(arguments, prompt)
	}
	return arguments, headless
}

func nativeAgentPermissionMode(request compatibility.Request) proto.AgentPermissionMode {
	if len(request.Permissions.DeniedTools) != 0 || len(request.Permissions.DeniedPaths) != 0 || len(request.Permissions.DeniedURLs) != 0 || request.Permissions.Mode == "plan" || request.Permissions.Mode == "dontAsk" {
		return proto.AgentPermissionDeny
	}
	if request.Permissions.Bypass {
		return proto.AgentPermissionBypass
	}
	if request.Permissions.Mode == "never" {
		return proto.AgentPermissionDeny
	}
	return proto.AgentPermissionInteractive
}

func nativePermissionMode(request compatibility.Request) string {
	mode := nativeAgentPermissionMode(request)
	if mode == proto.AgentPermissionBypass {
		return string(mode)
	}
	// The child `crux run` compatibility flag intentionally accepts only
	// fail-closed modes. Interactive compatibility requests use the native
	// bridge above, where permission prompts retain their normal lifecycle.
	return string(proto.AgentPermissionDeny)
}

func runNative(ctx context.Context, invocation compatibility.Invocation, workingDir string, arguments []string, stdin io.Reader, stdout, stderr io.Writer) error {
	command := exec.CommandContext(ctx, invocation.Executable, arguments...)
	command.Dir = workingDir
	command.Env = invocation.Env
	command.Stdin = stdin
	command.Stdout = stdout
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if exitError, ok := errors.AsType[*exec.ExitError](err); ok {
			return &compatibility.ExitError{Code: exitError.ExitCode()}
		}
		return fmt.Errorf("run native Crux command: %w", err)
	}
	return nil
}

func structuredResult(request compatibility.Request, text string) (any, error) {
	if len(request.Output.Schema) == 0 {
		return nil, nil
	}
	var outputSchema schema.Schema
	if err := json.Unmarshal(request.Output.Schema, &outputSchema); err != nil {
		return nil, fmt.Errorf("invalid JSON schema: %w", err)
	}
	return schema.ParseAndValidate(text, outputSchema)
}

func writeOutput(output io.Writer, request compatibility.Request, text string) error {
	switch request.Output.Mode {
	case "", compatibility.OutputText:
		_, err := fmt.Fprintln(output, text)
		return err
	case compatibility.OutputJSON:
		return writeJSON(output, aggregateOutput(request, text))
	case compatibility.OutputJSONLines, compatibility.OutputStreamJSON:
		for _, value := range streamOutput(request.Source, text) {
			if err := writeJSON(output, value); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("unsupported output mode %q", request.Output.Mode)
	}
}

func aggregateOutput(request compatibility.Request, text string) any {
	structured, _ := structuredResult(request, text)
	switch request.Source {
	case "claude":
		result := map[string]any{
			"type": "result", "subtype": "success", "uuid": uuid.NewString(), "session_id": uuid.NewString(),
			"duration_ms": 0, "duration_api_ms": 0, "is_error": false, "num_turns": 1,
			"result": text, "stop_reason": "end_turn", "total_cost_usd": 0,
			"usage": map[string]int{}, "modelUsage": map[string]any{}, "permission_denials": []any{},
		}
		if structured != nil {
			result["structured_output"] = structured
		}
		return result
	case "agy":
		result := map[string]any{
			"conversation_id": uuid.NewString(), "status": "SUCCESS", "response": text,
			"duration_seconds": 0, "num_turns": 1, "usage": zeroUsage(),
		}
		if structured != nil {
			result["structured_output"] = structured
			var jsonSchema any
			if json.Unmarshal(request.Output.Schema, &jsonSchema) == nil {
				result["json_schema"] = jsonSchema
			}
		}
		return result
	default:
		return map[string]any{"result": text}
	}
}

func streamOutput(source, text string) []any {
	switch source {
	case "codex":
		return []any{
			map[string]any{"type": "item.completed", "item": map[string]any{"type": "agent_message", "text": text}},
			map[string]any{"type": "turn.completed"},
		}
	case "claude":
		return []any{
			map[string]any{"type": "assistant", "message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}},
			map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": text},
		}
	case "agy":
		return []any{
			map[string]any{"event": "init"},
			map[string]any{"event": "result", "status": "SUCCESS", "result": text},
		}
	default:
		return []any{map[string]any{"type": "assistant", "content": text}}
	}
}

func writeErrorOutput(output io.Writer, request compatibility.Request, message string) error {
	if message == "" {
		message = "native Crux execution failed"
	}
	switch request.Source {
	case "codex":
		return writeJSON(output, map[string]any{"type": "error", "message": message})
	case "claude":
		return writeJSON(output, map[string]any{
			"type": "result", "subtype": "error_during_execution", "uuid": uuid.NewString(), "session_id": uuid.NewString(),
			"duration_ms": 0, "duration_api_ms": 0, "is_error": true, "num_turns": 0,
			"errors": []string{message}, "stop_reason": "error", "total_cost_usd": 0, "usage": map[string]int{},
			"modelUsage": map[string]any{}, "permission_denials": []any{},
		})
	case "agy":
		value := map[string]any{
			"conversation_id": "", "status": "ERROR", "response": "", "error": message,
			"duration_seconds": 0, "num_turns": 0, "usage": zeroUsage(),
		}
		if request.Output.Mode == compatibility.OutputStreamJSON {
			return writeJSON(output, map[string]any{"event": "result", "result": value})
		}
		return writeJSON(output, value)
	default:
		return writeJSON(output, map[string]any{"type": "error", "message": message})
	}
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeCompatibilityLog(request compatibility.Request, runErr error) error {
	path := request.Metadata["log-file"]
	if path == "" {
		return nil
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(request.WorkingDir, path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create compatibility log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open compatibility log: %w", err)
	}
	defer file.Close()
	status := "success"
	if runErr != nil {
		status = "error: " + runErr.Error()
	}
	_, err = fmt.Fprintf(file, "%s %s\n", time.Now().UTC().Format(time.RFC3339Nano), status)
	return err
}

func writeLastMessage(path, text string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve last-message output path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absolute), 0o700); err != nil {
		return fmt.Errorf("create last-message output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(absolute), ".crux-last-message-*")
	if err != nil {
		return fmt.Errorf("create last-message output: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := io.WriteString(temporary, text+"\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, absolute); err != nil {
		return fmt.Errorf("write last-message output: %w", err)
	}
	return nil
}
