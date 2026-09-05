package localaddon

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/google/uuid"
)

const protocolInputLimit = 10 << 20

type protocolOutput struct {
	mu            sync.Mutex
	writer        io.Writer
	contentLength bool
}

func (o *protocolOutput) write(value any) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.contentLength {
		return writeJSON(o.writer, value)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(o.writer, "Content-Length: %d\r\n\r\n", len(payload)); err != nil {
		return err
	}
	_, err = o.writer.Write(payload)
	return err
}

type oneShotStream struct {
	output         *protocolOutput
	request        compatibility.Request
	conversationID string
	turnID         string
	messageID      string
	itemID         string
	parentID       string
	startedAt      time.Time
	usage          nativeCompatUsage
	capture        bytes.Buffer
}

func newOneShotStream(writer io.Writer, request compatibility.Request) *oneShotStream {
	return &oneShotStream{
		output:         &protocolOutput{writer: writer},
		request:        request,
		conversationID: uuid.NewString(),
		turnID:         uuid.NewString(),
		messageID:      uuid.NewString(),
		itemID:         uuid.NewString(),
		startedAt:      time.Now(),
	}
}

func (s *oneShotStream) start() error {
	switch s.request.Source {
	case "codex":
		if err := s.output.write(map[string]any{"type": "thread.started", "thread_id": s.conversationID}); err != nil {
			return err
		}
		return s.output.write(map[string]any{"type": "turn.started"})
	case "claude":
		if err := s.output.write(claudeSystemInit(s.request, s.conversationID)); err != nil {
			return err
		}
		if s.request.Metadata["include-partial-messages"] != "true" {
			return nil
		}
		if err := s.claudeStreamEvent(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_crux_" + strings.ReplaceAll(s.messageID, "-", ""), "type": "message", "role": "assistant", "model": s.request.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}}); err != nil {
			return err
		}
		return s.claudeStreamEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
	case "agy":
		permissionMode := "request-review"
		if nativePermissionMode(s.request) == "bypass" {
			permissionMode = "always-proceed"
		}
		init := map[string]any{"cwd": s.request.WorkingDir, "tools": []string{}, "permission_mode": permissionMode}
		if s.request.Model != "" {
			init["model"] = s.request.Model
		}
		if s.request.Agent != "" {
			init["agent"] = s.request.Agent
		}
		return s.output.write(map[string]any{"event": "init", "conversation_id": s.conversationID, "init": init})
	case "copilot":
		if err := s.output.write(s.copilotEvent("session.start", map[string]any{"sessionId": s.conversationID, "version": 1, "producer": "crux", "copilotVersion": "1.0.80", "startTime": s.startedAt.UTC().Format(time.RFC3339Nano)})); err != nil {
			return err
		}
		if err := s.output.write(s.copilotEvent("user.message", map[string]any{"content": s.request.Prompt.Text})); err != nil {
			return err
		}
		turn := s.copilotEvent("assistant.turn_start", map[string]any{"turnId": s.turnID})
		if err := s.output.write(turn); err != nil {
			return err
		}
		return s.output.write(s.copilotEvent("assistant.message_start", map[string]any{"messageId": s.messageID}))
	default:
		return nil
	}
}

func (s *oneShotStream) Write(data []byte) (int, error) {
	if _, err := s.capture.Write(data); err != nil {
		return 0, err
	}
	chunk := string(data)
	if chunk == "" {
		return len(data), nil
	}
	var err error
	switch s.request.Source {
	case "claude":
		if s.request.Metadata["include-partial-messages"] == "true" {
			err = s.claudeStreamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}})
		}
	case "agy":
		err = s.output.write(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": s.conversationID, "step_index": 1, "state": "ACTIVE", "step_type": "agent_response", "text_delta": chunk}})
	case "copilot":
		if s.request.Metadata["stream"] != "off" {
			err = s.output.write(s.copilotEvent("assistant.message_delta", map[string]any{"messageId": s.messageID, "deltaContent": chunk}))
		}
	}
	if err != nil {
		return 0, err
	}
	return len(data), nil
}

func (s *oneShotStream) complete() error {
	text := strings.TrimSuffix(s.capture.String(), "\n")
	duration := time.Since(s.startedAt)
	switch s.request.Source {
	case "codex":
		if err := s.output.write(map[string]any{"type": "item.completed", "item": map[string]any{"id": s.itemID, "type": "agent_message", "text": text}}); err != nil {
			return err
		}
		return s.output.write(map[string]any{"type": "turn.completed", "usage": map[string]int{"input_tokens": 0, "cached_input_tokens": 0, "output_tokens": 0, "reasoning_output_tokens": 0}})
	case "claude":
		if s.request.Metadata["include-partial-messages"] == "true" {
			if err := s.claudeStreamEvent(map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
				return err
			}
			if err := s.claudeStreamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}}); err != nil {
				return err
			}
			if err := s.claudeStreamEvent(map[string]any{"type": "message_stop"}); err != nil {
				return err
			}
		}
		if err := s.output.write(map[string]any{"type": "assistant", "uuid": uuid.NewString(), "session_id": s.conversationID, "parent_tool_use_id": nil, "message": map[string]any{"id": "msg_crux_" + strings.ReplaceAll(s.messageID, "-", ""), "type": "message", "role": "assistant", "model": s.request.Model, "content": []any{map[string]any{"type": "text", "text": text}}, "stop_reason": "end_turn", "usage": map[string]int64{"input_tokens": s.usage.InputTokens, "output_tokens": s.usage.OutputTokens}}}); err != nil {
			return err
		}
		result := map[string]any{"type": "result", "subtype": "success", "uuid": uuid.NewString(), "session_id": s.conversationID, "duration_ms": duration.Milliseconds(), "duration_api_ms": 0, "is_error": false, "num_turns": 1, "result": text, "stop_reason": "end_turn", "total_cost_usd": s.usage.Cost, "usage": map[string]int64{"input_tokens": s.usage.InputTokens, "output_tokens": s.usage.OutputTokens}, "modelUsage": map[string]any{s.request.Model: map[string]any{"inputTokens": s.usage.InputTokens, "outputTokens": s.usage.OutputTokens, "costUSD": s.usage.Cost}}, "permission_denials": []any{}}
		if structured, _ := structuredResult(s.request, text); structured != nil {
			result["structured_output"] = structured
		}
		return s.output.write(result)
	case "agy":
		if err := s.output.write(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": s.conversationID, "step_index": 1, "state": "DONE", "step_type": "agent_response", "text_delta": "", "duration_seconds": duration.Seconds()}}); err != nil {
			return err
		}
		result := map[string]any{"conversation_id": s.conversationID, "status": "SUCCESS", "response": text, "duration_seconds": duration.Seconds(), "num_turns": 1, "usage": agyUsage(s.usage)}
		if structured, _ := structuredResult(s.request, text); structured != nil {
			result["structured_output"] = structured
			var jsonSchema any
			if json.Unmarshal(s.request.Output.Schema, &jsonSchema) == nil {
				result["json_schema"] = jsonSchema
			}
		}
		return s.output.write(map[string]any{"event": "result", "result": result})
	case "copilot":
		if err := s.output.write(s.copilotEvent("assistant.message", map[string]any{"messageId": s.messageID, "content": text, "turnId": s.turnID})); err != nil {
			return err
		}
		if err := s.output.write(s.copilotEvent("assistant.turn_end", map[string]any{"turnId": s.turnID})); err != nil {
			return err
		}
		if err := s.output.write(s.copilotEvent("assistant.usage", map[string]any{"model": s.request.Model, "inputTokens": s.usage.InputTokens, "outputTokens": s.usage.OutputTokens, "cost": s.usage.Cost})); err != nil {
			return err
		}
		if err := s.output.write(s.copilotEvent("assistant.idle", map[string]any{})); err != nil {
			return err
		}
		return s.output.write(s.copilotEvent("session.idle", map[string]any{}))
	default:
		return nil
	}
}

func (s *oneShotStream) fail(message string) error {
	switch s.request.Source {
	case "codex":
		if err := s.output.write(map[string]any{"type": "error", "message": message}); err != nil {
			return err
		}
		return s.output.write(map[string]any{"type": "turn.failed", "error": map[string]any{"message": message}})
	case "claude":
		if s.request.Metadata["include-partial-messages"] == "true" {
			if err := s.claudeStreamEvent(map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
				return err
			}
			if err := s.claudeStreamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "error", "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}}); err != nil {
				return err
			}
			if err := s.claudeStreamEvent(map[string]any{"type": "message_stop"}); err != nil {
				return err
			}
		}
		return writeClaudeError(s.output, s.conversationID, 0, message)
	case "agy":
		return writeAgyStreamError(s.output, s.conversationID, 0, message)
	case "copilot":
		return s.output.write(s.copilotEvent("session.error", map[string]any{"errorType": "model_call", "message": message}))
	default:
		return nil
	}
}

func (s *oneShotStream) claudeStreamEvent(event map[string]any) error {
	return s.output.write(map[string]any{"type": "stream_event", "uuid": uuid.NewString(), "session_id": s.conversationID, "parent_tool_use_id": nil, "event": event})
}

func (s *oneShotStream) copilotEvent(eventType string, data map[string]any) map[string]any {
	id := uuid.NewString()
	event := map[string]any{"id": id, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "parentId": nil, "type": eventType, "data": data}
	if s.parentID != "" {
		event["parentId"] = s.parentID
	}
	if eventType == "assistant.message_start" || eventType == "assistant.message_delta" || eventType == "assistant.usage" || eventType == "assistant.idle" || eventType == "session.idle" {
		event["ephemeral"] = true
	}
	s.parentID = id
	return event
}

type captureWriter struct {
	buffer  bytes.Buffer
	onChunk func(string) error
}

func (w *captureWriter) Write(data []byte) (int, error) {
	if _, err := w.buffer.Write(data); err != nil {
		return 0, err
	}
	if w.onChunk != nil && len(data) != 0 {
		if err := w.onChunk(string(data)); err != nil {
			return 0, err
		}
	}
	return len(data), nil
}

func (w *captureWriter) text() string {
	return strings.TrimSuffix(w.buffer.String(), "\n")
}

func runProtocolTurn(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request, prompt string, resume bool, onChunk func(string) error) (string, error) {
	turn := request
	turn.Protocol = ""
	turn.Style = compatibility.ExecutionHeadless
	turn.Prompt = compatibility.Prompt{Source: compatibility.PromptArguments, Text: prompt}
	turn.Output = compatibility.Output{Mode: compatibility.OutputText, Schema: request.Output.Schema}
	turn.Metadata = nil
	turn.Session = compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}
	if resume {
		turn.Session.Mode = compatibility.SessionLatest
	}
	arguments, _ := nativeArguments(turn)
	var diagnostic bytes.Buffer
	capture := &captureWriter{onChunk: onChunk}
	workingDir := turn.WorkingDir
	if workingDir == "" {
		workingDir = invocation.WorkingDir
	}
	if err := runNative(ctx, invocation, workingDir, arguments, bytes.NewReader(nil), capture, &diagnostic); err != nil {
		message := strings.TrimSpace(diagnostic.String())
		if message == "" {
			message = err.Error()
		}
		return capture.text(), fmt.Errorf("%s", message)
	}
	if diagnostic.Len() != 0 {
		_, _ = diagnostic.WriteTo(invocation.Stderr)
	}
	text := capture.text()
	if _, err := structuredResult(turn, text); err != nil {
		return "", fmt.Errorf("structured output validation failed: %w", err)
	}
	return text, nil
}

func scanProtocolInput(reader io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), protocolInputLimit)
	return scanner
}

func streamContent(raw json.RawMessage) (string, error) {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", errors.New("message.content must be a string or an array of text blocks")
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported content block type %q", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, ""), nil
}

func runAgyStream(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	output := &protocolOutput{writer: invocation.Stdout}
	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	nativeSession, err := bridge.resolveSession(ctx, request)
	if err != nil {
		return err
	}
	conversationID := nativeSession.session.ID
	request.Model = nativeSession.model.Provider + "/" + nativeSession.model.Model
	permissionMode := "request-review"
	if nativePermissionMode(request) == "bypass" {
		permissionMode = "always-proceed"
	}
	init := map[string]any{"cwd": request.WorkingDir, "tools": []string{}, "permission_mode": permissionMode}
	if request.Model != "" {
		init["model"] = request.Model
	}
	if request.Agent != "" {
		init["agent"] = request.Agent
	}
	if err := output.write(map[string]any{"event": "init", "conversation_id": conversationID, "init": init}); err != nil {
		return err
	}
	scanner := scanProtocolInput(request.Prompt.Stdin)
	turns := 0
	for scanner.Scan() {
		line := scanner.Bytes()
		var input struct {
			Event   string `json:"event"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &input); err != nil {
			_ = writeAgyStreamError(output, conversationID, turns, "invalid JSON input: "+err.Error())
			return &compatibility.ExitError{Code: 1}
		}
		if input.Event == "" {
			_ = writeAgyStreamError(output, conversationID, turns, "stream input message is missing event")
			return &compatibility.ExitError{Code: 1}
		}
		if input.Event == "control_request" || input.Event == "control_response" {
			_ = writeAgyStreamError(output, conversationID, turns, input.Event+" is not supported")
			return &compatibility.ExitError{Code: 2}
		}
		if input.Event != "user" {
			_, _ = fmt.Fprintf(invocation.Stderr, "warning: ignoring unsupported stream input message event %q\n", input.Event)
			continue
		}
		prompt, err := streamContent(input.Message.Content)
		if err != nil {
			_ = writeAgyStreamError(output, conversationID, turns, err.Error())
			return &compatibility.ExitError{Code: 1}
		}
		if strings.HasPrefix(strings.TrimSpace(prompt), "/") {
			_ = writeAgyStreamError(output, conversationID, turns, "CLI slash commands are unavailable with --input-format stream-json")
			return &compatibility.ExitError{Code: 2}
		}
		stepIndex := turns * 2
		if err := output.write(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": conversationID, "step_index": stepIndex, "state": "DONE", "step_type": "user_input"}}); err != nil {
			return err
		}
		start := time.Now()
		turnContext := ctx
		cancel := func() {}
		if request.Limits.Timeout > 0 {
			turnContext, cancel = context.WithTimeout(ctx, request.Limits.Timeout)
		}
		turn := bridge.runTurn(turnContext, nativeSession, prompt, nativeAgentPermissionMode(request), func(chunk string) error {
			return output.write(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": conversationID, "step_index": stepIndex + 1, "state": "ACTIVE", "step_type": "agent_response", "text_delta": chunk}})
		})
		text, runErr := turn.Text, turn.Err
		cancel()
		if errors.Is(turnContext.Err(), context.DeadlineExceeded) {
			runErr = fmt.Errorf("execution timed out after %s", request.Limits.Timeout)
		}
		turns++
		if !errors.Is(turnContext.Err(), context.DeadlineExceeded) && (turn.Cancelled || errors.Is(turnContext.Err(), context.Canceled)) {
			_ = writeAgyStreamStatus(output, conversationID, turns, "CANCELED", "execution canceled")
			return &compatibility.ExitError{Code: 1}
		}
		if runErr != nil {
			_ = writeAgyStreamError(output, conversationID, turns, runErr.Error())
			return &compatibility.ExitError{Code: 1}
		}
		duration := time.Since(start).Seconds()
		if err := output.write(map[string]any{"event": "step_update", "step_update": map[string]any{"conversation_id": conversationID, "step_index": stepIndex + 1, "state": "DONE", "step_type": "agent_response", "text_delta": "", "duration_seconds": duration}}); err != nil {
			return err
		}
		result := map[string]any{"conversation_id": conversationID, "status": "SUCCESS", "response": text, "duration_seconds": duration, "num_turns": turns, "usage": agyUsage(turn.Usage)}
		if structured, _ := structuredResult(request, text); structured != nil {
			result["structured_output"] = structured
			var jsonSchema any
			if json.Unmarshal(request.Output.Schema, &jsonSchema) == nil {
				result["json_schema"] = jsonSchema
			}
		}
		if err := output.write(map[string]any{"event": "result", "result": result}); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		_ = writeAgyStreamError(output, conversationID, turns, err.Error())
		return &compatibility.ExitError{Code: 1}
	}
	return nil
}

func writeAgyStreamError(output *protocolOutput, conversationID string, turns int, message string) error {
	return writeAgyStreamStatus(output, conversationID, turns, "ERROR", message)
}

func writeAgyStreamStatus(output *protocolOutput, conversationID string, turns int, status, message string) error {
	return output.write(map[string]any{"event": "result", "result": map[string]any{"conversation_id": conversationID, "status": status, "response": "", "error": message, "duration_seconds": 0, "num_turns": turns, "usage": zeroUsage()}})
}

func agyUsage(usage nativeCompatUsage) map[string]int64 {
	return map[string]int64{"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens, "thinking_tokens": 0, "cache_read_tokens": 0, "total_tokens": usage.InputTokens + usage.OutputTokens}
}

func zeroUsage() map[string]int {
	return map[string]int{"input_tokens": 0, "output_tokens": 0, "thinking_tokens": 0, "cache_read_tokens": 0, "total_tokens": 0}
}

func claudeSystemInit(request compatibility.Request, sessionID string) map[string]any {
	permissionMode := string(request.Permissions.Mode)
	if permissionMode == "" || permissionMode == "manual" || permissionMode == "auto" {
		permissionMode = "default"
	}
	return map[string]any{
		"type": "system", "subtype": "init", "uuid": uuid.NewString(), "session_id": sessionID,
		"cwd": request.WorkingDir, "tools": []string{}, "model": request.Model,
		"permissionMode": permissionMode, "claude_code_version": "2.1.245",
		"apiKeySource": "temporary", "mcp_servers": []any{}, "slash_commands": []string{},
		"output_style": "default", "agents": []string{}, "betas": []string{}, "skills": []string{}, "plugins": []any{},
	}
}

func claudeInitializeResult(request compatibility.Request) map[string]any {
	models := []any{}
	if request.Model != "" {
		models = append(models, map[string]any{"value": request.Model, "displayName": request.Model, "description": "Configured Crux model"})
	}
	return map[string]any{
		"commands": []any{}, "agents": []any{}, "output_style": "default",
		"available_output_styles": []string{}, "models": models, "account": map[string]any{},
	}
}

func claudeContextUsageResult(request compatibility.Request) map[string]any {
	return map[string]any{
		"categories": []any{}, "totalTokens": 0, "maxTokens": 0, "rawMaxTokens": 0,
		"percentage": 0, "gridRows": []any{}, "model": request.Model, "memoryFiles": []any{},
		"mcpTools": []any{}, "agents": []any{}, "isAutoCompactEnabled": false, "apiUsage": nil,
	}
}

func claudeSettingsResult(request compatibility.Request) map[string]any {
	return map[string]any{
		"effective": map[string]any{}, "sources": []any{},
		"applied": map[string]any{"model": request.Model, "effort": nil},
	}
}

func runClaudeStream(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	type queuedPrompt struct {
		uuid   string
		prompt string
	}
	type turnResult struct {
		prompt    queuedPrompt
		messageID string
		text      string
		startedAt time.Time
		cancelled bool
		usage     nativeCompatUsage
		err       error
	}

	output := &protocolOutput{writer: invocation.Stdout}
	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	nativeSession, err := bridge.resolveSession(ctx, request)
	if err != nil {
		return err
	}
	sessionID := nativeSession.session.ID
	request.Model = nativeSession.model.Provider + "/" + nativeSession.model.Model
	partial := request.Metadata["include-partial-messages"] == "true"
	replay := request.Metadata["replay-user-messages"] == "true"
	if err := output.write(claudeSystemInit(request, sessionID)); err != nil {
		return err
	}

	lines := make(chan []byte)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := scanProtocolInput(request.Prompt.Stdin)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
	}()

	turnDone := make(chan turnResult, 1)
	var queue []queuedPrompt
	seenUUIDs := make(map[string]struct{})
	var cancelTurn context.CancelFunc
	active := false
	turns := 0
	startNext := func() {
		if active || len(queue) == 0 {
			return
		}
		prompt := queue[0]
		queue = queue[1:]
		turnCtx, cancel := context.WithCancel(ctx)
		cancelTurn = cancel
		active = true
		startedAt := time.Now()
		messageID := "msg_crux_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		go func() {
			streamEvent := func(event map[string]any) error {
				return output.write(map[string]any{"type": "stream_event", "uuid": uuid.NewString(), "session_id": sessionID, "parent_tool_use_id": nil, "event": event})
			}
			if partial {
				if err := streamEvent(map[string]any{"type": "message_start", "message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": request.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}}); err != nil {
					turnDone <- turnResult{prompt: prompt, messageID: messageID, startedAt: startedAt, err: err}
					return
				}
				if err := streamEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}); err != nil {
					turnDone <- turnResult{prompt: prompt, messageID: messageID, startedAt: startedAt, err: err}
					return
				}
			}
			turn := bridge.runTurn(turnCtx, nativeSession, prompt.prompt, nativeAgentPermissionMode(request), func(chunk string) error {
				if !partial {
					return nil
				}
				return streamEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": chunk}})
			})
			text, runErr := turn.Text, turn.Err
			if partial {
				stopReason := "end_turn"
				if runErr != nil {
					stopReason = "error"
				}
				if err := streamEvent(map[string]any{"type": "content_block_stop", "index": 0}); err != nil {
					runErr = err
				} else if err := streamEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 0}}); err != nil {
					runErr = err
				} else if err := streamEvent(map[string]any{"type": "message_stop"}); err != nil {
					runErr = err
				}
			}
			turnDone <- turnResult{prompt: prompt, messageID: messageID, text: text, startedAt: startedAt, cancelled: turn.Cancelled, usage: turn.Usage, err: runErr}
		}()
	}

	for lines != nil || active || len(queue) != 0 {
		startNext()
		select {
		case <-ctx.Done():
			if cancelTurn != nil {
				cancelTurn()
			}
			return ctx.Err()
		case result := <-turnDone:
			active = false
			cancelTurn = nil
			turns++
			if result.cancelled {
				if err := writeClaudeError(output, sessionID, turns, "Interrupted"); err != nil {
					return err
				}
				continue
			}
			if result.err != nil {
				if err := writeClaudeError(output, sessionID, turns, result.err.Error()); err != nil {
					return err
				}
				continue
			}
			messageID := result.messageID
			if err := output.write(map[string]any{
				"type": "assistant", "uuid": uuid.NewString(), "session_id": sessionID, "parent_tool_use_id": nil,
				"message": map[string]any{"id": messageID, "type": "message", "role": "assistant", "model": request.Model, "content": []any{map[string]any{"type": "text", "text": result.text}}, "stop_reason": "end_turn", "usage": map[string]int64{"input_tokens": result.usage.InputTokens, "output_tokens": result.usage.OutputTokens}},
			}); err != nil {
				return err
			}
			resultOutput := map[string]any{
				"type": "result", "subtype": "success", "uuid": uuid.NewString(), "session_id": sessionID,
				"duration_ms": time.Since(result.startedAt).Milliseconds(), "duration_api_ms": 0, "is_error": false, "num_turns": turns,
				"result": result.text, "stop_reason": "end_turn", "total_cost_usd": result.usage.Cost,
				"usage":      map[string]int64{"input_tokens": result.usage.InputTokens, "output_tokens": result.usage.OutputTokens},
				"modelUsage": map[string]any{request.Model: map[string]any{"inputTokens": result.usage.InputTokens, "outputTokens": result.usage.OutputTokens, "costUSD": result.usage.Cost}}, "permission_denials": []any{},
			}
			if structured, _ := structuredResult(request, result.text); structured != nil {
				resultOutput["structured_output"] = structured
			}
			if err := output.write(resultOutput); err != nil {
				return err
			}
		case line, ok := <-lines:
			if !ok {
				lines = nil
				continue
			}
			var envelope struct {
				Type      string          `json:"type"`
				UUID      string          `json:"uuid"`
				RequestID string          `json:"request_id"`
				Request   json.RawMessage `json:"request"`
				Priority  string          `json:"priority"`
				Message   struct {
					Role    string          `json:"role"`
					Content json.RawMessage `json:"content"`
				} `json:"message"`
			}
			if err := json.Unmarshal(line, &envelope); err != nil {
				if err := writeClaudeError(output, sessionID, turns, "invalid JSON input: "+err.Error()); err != nil {
					return err
				}
				return &compatibility.ExitError{Code: 1}
			}
			if envelope.Type == "control_request" {
				var control struct {
					Subtype      string `json:"subtype"`
					CancelQueued bool   `json:"cancel_queued"`
					MessageUUID  string `json:"message_uuid"`
					Model        string `json:"model"`
					Mode         string `json:"mode"`
				}
				_ = json.Unmarshal(envelope.Request, &control)
				result := map[string]any{}
				responseSubtype := "success"
				switch control.Subtype {
				case "initialize":
					result = claudeInitializeResult(request)
					if models, modelErr := bridge.modelCatalog(ctx, nativeSession.workspace); modelErr == nil {
						result["models"] = models
					} else {
						responseSubtype = "error"
						result = map[string]any{"error": modelErr.Error()}
					}
				case "interrupt":
					if active && cancelTurn != nil {
						cancelTurn()
					}
				case "cancel_async_message":
					cancelled := false
					remaining := queue[:0]
					for _, queued := range queue {
						if queued.uuid == control.MessageUUID {
							cancelled = true
							continue
						}
						remaining = append(remaining, queued)
					}
					queue = remaining
					result["cancelled"] = cancelled
				case "set_model":
					selection, modelErr := bridge.selectModel(ctx, nativeSession, control.Model)
					if modelErr != nil {
						responseSubtype = "error"
						result["error"] = modelErr.Error()
					} else {
						request.Model = selection.Provider + "/" + selection.Model
						result["model"] = request.Model
					}
				case "set_permission_mode":
					if !enum(control.Mode, "default", "acceptEdits", "bypassPermissions", "dontAsk", "plan") {
						responseSubtype = "error"
						result["error"] = "invalid permission mode: " + control.Mode
					} else {
						request.Permissions.Mode = compatibility.PermissionMode(control.Mode)
					}
				case "set_max_thinking_tokens":
					responseSubtype = "error"
					result["error"] = "set_max_thinking_tokens is unsupported by native Crux sessions"
				case "mcp_status":
					result["mcpServers"] = []any{}
				case "get_context_usage":
					result = claudeContextUsageResult(request)
				case "rewind_files":
					result["canRewind"] = false
					result["error"] = "file rewind is unavailable for native Crux sessions"
				case "get_settings":
					result = claudeSettingsResult(request)
				case "seed_read_state", "mcp_message", "mcp_set_servers", "reload_plugins", "mcp_reconnect", "mcp_toggle", "stop_task", "apply_flag_settings", "hook_callback", "elicitation":
					responseSubtype = "error"
					result["error"] = "unsupported control request subtype: " + control.Subtype
				default:
					responseSubtype = "error"
					result["error"] = "unsupported control request subtype: " + control.Subtype
				}
				if err := output.write(map[string]any{"type": "control_response", "response": map[string]any{"request_id": envelope.RequestID, "subtype": responseSubtype, "response": result}}); err != nil {
					return err
				}
				continue
			}
			if envelope.Type == "keep_alive" || envelope.Type == "update_environment_variables" || envelope.Type == "control_response" {
				continue
			}
			if envelope.Type != "user" {
				if err := writeClaudeError(output, sessionID, turns, fmt.Sprintf("unsupported stream input type %q", envelope.Type)); err != nil {
					return err
				}
				return &compatibility.ExitError{Code: 1}
			}
			prompt, err := streamContent(envelope.Message.Content)
			if err != nil {
				if err := writeClaudeError(output, sessionID, turns, err.Error()); err != nil {
					return err
				}
				return &compatibility.ExitError{Code: 1}
			}
			if envelope.UUID != "" {
				if _, duplicate := seenUUIDs[envelope.UUID]; duplicate {
					if replay {
						if err := output.write(map[string]any{"type": "user", "uuid": envelope.UUID, "session_id": sessionID, "message": map[string]any{"role": "user", "content": prompt}, "parent_tool_use_id": nil, "isReplay": true}); err != nil {
							return err
						}
					}
					continue
				}
				seenUUIDs[envelope.UUID] = struct{}{}
			}
			if replay {
				if err := output.write(map[string]any{"type": "user", "uuid": envelope.UUID, "session_id": sessionID, "message": map[string]any{"role": "user", "content": json.RawMessage(envelope.Message.Content)}, "parent_tool_use_id": nil, "isReplay": true}); err != nil {
					return err
				}
			}
			queued := queuedPrompt{uuid: envelope.UUID, prompt: prompt}
			switch envelope.Priority {
			case "now":
				queue = append([]queuedPrompt{queued}, queue...)
			case "", "next", "later":
				queue = append(queue, queued)
			default:
				if err := writeClaudeError(output, sessionID, turns, fmt.Sprintf("invalid message priority %q", envelope.Priority)); err != nil {
					return err
				}
				return &compatibility.ExitError{Code: 1}
			}
		}
	}
	return <-scanErrors
}

func writeClaudeError(output *protocolOutput, sessionID string, turns int, message string) error {
	return output.write(map[string]any{"type": "result", "subtype": "error_during_execution", "uuid": uuid.NewString(), "session_id": sessionID, "duration_ms": 0, "duration_api_ms": 0, "is_error": true, "num_turns": turns, "errors": []string{message}, "stop_reason": "error", "total_cost_usd": 0, "usage": map[string]int{}, "modelUsage": map[string]any{}, "permission_denials": []any{}})
}

func protocolTextBlocks(raw json.RawMessage) (string, error) {
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return "", err
	}
	var parts []string
	for _, block := range blocks {
		if block.Type != "text" {
			return "", fmt.Errorf("unsupported prompt block type %q", block.Type)
		}
		parts = append(parts, block.Text)
	}
	return strings.Join(parts, ""), nil
}

func runCopilotACP(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	type sessionState struct {
		turns      int
		workingDir string
		mode       string
		model      string
		config     map[string]any
		native     *nativeCompatSession
	}
	type turnResult struct {
		requestID json.RawMessage
		sessionID string
		cancelled bool
		usage     nativeCompatUsage
		err       error
	}
	type activeTurn struct {
		requestID json.RawMessage
		sessionID string
		cancel    context.CancelFunc
	}

	output := &protocolOutput{writer: invocation.Stdout}
	lines := make(chan []byte)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := scanProtocolInput(request.Prompt.Stdin)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
		scanErrors <- scanner.Err()
	}()

	initialized := false
	sessions := make(map[string]*sessionState)
	turnDone := make(chan turnResult, 1)
	var active *activeTurn
	modes := func(current string) map[string]any {
		return map[string]any{
			"currentModeId": current,
			"availableModes": []any{
				map[string]any{"id": "interactive", "name": "Interactive"},
				map[string]any{"id": "plan", "name": "Plan"},
			},
		}
	}
	newSessionResult := func(sessionID string, state *sessionState) map[string]any {
		return map[string]any{"sessionId": sessionID, "modes": modes(state.mode), "configOptions": []any{}}
	}
	for lines != nil || active != nil {
		select {
		case <-ctx.Done():
			if active != nil {
				active.cancel()
			}
			return ctx.Err()
		case result := <-turnDone:
			state := sessions[result.sessionID]
			if state != nil {
				state.turns++
			}
			if err := output.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": result.sessionID, "update": map[string]any{"sessionUpdate": "usage_update", "used": result.usage.InputTokens + result.usage.OutputTokens, "size": result.usage.OutputTokens, "cost": result.usage.Cost}}}); err != nil {
				return err
			}
			if result.cancelled {
				if err := writeRPCResult(output, result.requestID, map[string]any{"stopReason": "cancelled"}); err != nil {
					return err
				}
			} else if result.err != nil {
				if err := writeRPCError(output, result.requestID, -32603, result.err.Error()); err != nil {
					return err
				}
			} else if err := writeRPCResult(output, result.requestID, map[string]any{"stopReason": "end_turn"}); err != nil {
				return err
			}
			active = nil
		case line, ok := <-lines:
			if !ok {
				lines = nil
				if active != nil {
					active.cancel()
				}
				continue
			}
			var message struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Method  string          `json:"method"`
				Params  json.RawMessage `json:"params"`
			}
			if err := json.Unmarshal(line, &message); err != nil {
				if err := writeRPCError(output, nil, -32700, "Parse error"); err != nil {
					return err
				}
				continue
			}
			if message.JSONRPC != "2.0" || message.Method == "" {
				if len(message.ID) != 0 {
					if err := writeRPCError(output, message.ID, -32600, "Invalid request"); err != nil {
						return err
					}
				}
				continue
			}
			if message.Method != "initialize" && !initialized {
				if len(message.ID) != 0 {
					if err := writeRPCError(output, message.ID, -32600, "Agent is not initialized"); err != nil {
						return err
					}
				}
				continue
			}
			switch message.Method {
			case "initialize":
				var params struct {
					ProtocolVersion int `json:"protocolVersion"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ProtocolVersion != 1 {
					if err := writeRPCError(output, message.ID, -32602, "Unsupported protocol version"); err != nil {
						return err
					}
					continue
				}
				initialized = true
				result := map[string]any{
					"protocolVersion":   1,
					"agentCapabilities": map[string]any{"loadSession": true, "promptCapabilities": map[string]any{"image": false, "audio": false, "embeddedContext": false}},
					"agentInfo":         map[string]any{"name": "Crux Copilot compatibility", "version": "1.0.80"},
					"authMethods":       []any{},
				}
				if err := writeRPCResult(output, message.ID, result); err != nil {
					return err
				}
			case "authenticate":
				if err := writeRPCResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "session/new", "session/load":
				var params struct {
					SessionID string `json:"sessionId"`
					CWD       string `json:"cwd"`
				}
				if json.Unmarshal(message.Params, &params) != nil || !filepath.IsAbs(params.CWD) {
					if err := writeRPCError(output, message.ID, -32602, "cwd must be an absolute path"); err != nil {
						return err
					}
					continue
				}
				var nativeSession *nativeCompatSession
				var sessionErr error
				if message.Method == "session/load" {
					workspace, workspaceErr := bridge.base.workspace(params.CWD)
					if workspaceErr != nil {
						sessionErr = workspaceErr
					} else {
						nativeSession, sessionErr = bridge.getSession(ctx, workspace, params.SessionID)
					}
				} else {
					nativeSession, sessionErr = bridge.createSession(ctx, params.CWD, "Copilot ACP session", request.Model)
				}
				if nativeSession != nil && !request.Session.Persistent {
					bridge.markEphemeral(nativeSession)
				}
				if sessionErr != nil {
					if err := writeRPCError(output, message.ID, -32002, sessionErr.Error()); err != nil {
						return err
					}
					continue
				}
				sessionID := nativeSession.session.ID
				state := &sessionState{workingDir: params.CWD, mode: "interactive", model: nativeSession.model.Provider + "/" + nativeSession.model.Model, config: make(map[string]any), native: nativeSession}
				sessions[sessionID] = state
				if err := writeRPCResult(output, message.ID, newSessionResult(sessionID, state)); err != nil {
					return err
				}
			case "session/set_mode":
				var params struct {
					SessionID string `json:"sessionId"`
					ModeID    string `json:"modeId"`
				}
				if json.Unmarshal(message.Params, &params) != nil || !enum(params.ModeID, "interactive", "plan") {
					if err := writeRPCError(output, message.ID, -32602, "Invalid session mode"); err != nil {
						return err
					}
					continue
				}
				state := sessions[params.SessionID]
				if state == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				state.mode = params.ModeID
				if err := writeRPCResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "session/set_config_option":
				var params struct {
					SessionID string `json:"sessionId"`
					ConfigID  string `json:"configId"`
					Value     any    `json:"value"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ConfigID == "" {
					if err := writeRPCError(output, message.ID, -32602, "Invalid config option"); err != nil {
						return err
					}
					continue
				}
				state := sessions[params.SessionID]
				if state == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				state.config[params.ConfigID] = params.Value
				if params.ConfigID == "model" {
					model, ok := params.Value.(string)
					if !ok {
						if err := writeRPCError(output, message.ID, -32602, "model must be a string"); err != nil {
							return err
						}
						continue
					}
					selection, modelErr := bridge.selectModel(ctx, state.native, model)
					if modelErr != nil {
						if err := writeRPCError(output, message.ID, -32602, modelErr.Error()); err != nil {
							return err
						}
						continue
					}
					state.model = selection.Provider + "/" + selection.Model
				}
				if err := writeRPCResult(output, message.ID, map[string]any{}); err != nil {
					return err
				}
			case "session/prompt":
				if active != nil {
					if err := writeRPCError(output, message.ID, -32600, "A prompt is already running"); err != nil {
						return err
					}
					continue
				}
				var params struct {
					SessionID string          `json:"sessionId"`
					Prompt    json.RawMessage `json:"prompt"`
				}
				if json.Unmarshal(message.Params, &params) != nil {
					if err := writeRPCError(output, message.ID, -32602, "Invalid session/prompt params"); err != nil {
						return err
					}
					continue
				}
				state := sessions[params.SessionID]
				if state == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				prompt, err := protocolTextBlocks(params.Prompt)
				if err != nil {
					if err := writeRPCError(output, message.ID, -32602, err.Error()); err != nil {
						return err
					}
					continue
				}
				turnCtx, cancel := context.WithCancel(ctx)
				active = &activeTurn{requestID: append(json.RawMessage(nil), message.ID...), sessionID: params.SessionID, cancel: cancel}
				requestID := append(json.RawMessage(nil), message.ID...)
				sessionID := params.SessionID
				go func() {
					turn := bridge.runTurn(turnCtx, state.native, prompt, nativeAgentPermissionMode(request), func(chunk string) error {
						return output.write(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sessionID, "update": map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": chunk}}}})
					})
					turnDone <- turnResult{requestID: requestID, sessionID: sessionID, cancelled: turn.Cancelled || errors.Is(turnCtx.Err(), context.Canceled), usage: turn.Usage, err: turn.Err}
				}()
			case "session/cancel":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if active != nil && active.sessionID == params.SessionID {
					active.cancel()
				}
			case "$/cancel_request":
				if active != nil {
					active.cancel()
				}
			case "session/close":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if active != nil && active.sessionID == params.SessionID {
					active.cancel()
				}
				delete(sessions, params.SessionID)
				if len(message.ID) != 0 {
					if err := writeRPCResult(output, message.ID, map[string]any{}); err != nil {
						return err
					}
				}
			default:
				if len(message.ID) != 0 {
					if err := writeRPCError(output, message.ID, -32601, "Method not found"); err != nil {
						return err
					}
				}
			}
		}
	}
	return <-scanErrors
}

func writeRPCResult(output *protocolOutput, id json.RawMessage, result any) error {
	return output.write(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "result": result})
}

func writeRPCError(output *protocolOutput, id json.RawMessage, code int, message string) error {
	value := map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": code, "message": message}}
	if len(id) != 0 {
		value["id"] = json.RawMessage(id)
	}
	return output.write(value)
}

func runCodexAppServer(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	return runCodexAppServerNative(ctx, invocation, request)
}

func writeCodexResult(output *protocolOutput, id json.RawMessage, result any) error {
	return output.write(map[string]any{"id": json.RawMessage(id), "result": result})
}

func writeCodexError(output *protocolOutput, id json.RawMessage, code int, message string) error {
	value := map[string]any{"id": nil, "error": map[string]any{"code": code, "message": message}}
	if len(id) != 0 {
		value["id"] = json.RawMessage(id)
	}
	return output.write(value)
}
