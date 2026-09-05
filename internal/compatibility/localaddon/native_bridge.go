package localaddon

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/google/uuid"
)

// nativeCompatBridge exposes the protocol-neutral part of the native Crux
// client/server bridge. Target adapters retain ownership of their wire shapes
// and external identifiers.
type nativeCompatBridge struct {
	base      *codexNativeBridge
	ephemeral map[*nativeCompatSession]struct{}
}

type nativeCompatSession struct {
	workspace     *codexNativeWorkspace
	session       *proto.Session
	model         config.SelectedModel
	deleteOnClose bool
}

type nativeCompatUsage struct {
	InputTokens  int64
	OutputTokens int64
	Cost         float64
}

type nativeCompatTurnResult struct {
	Text      string
	Cancelled bool
	Usage     nativeCompatUsage
	Err       error
}

func newNativeCompatBridge(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (*nativeCompatBridge, error) {
	base, err := newCodexNativeBridge(ctx, invocation, request)
	if err != nil {
		return nil, err
	}
	return &nativeCompatBridge{base: base, ephemeral: make(map[*nativeCompatSession]struct{})}, nil
}

func (b *nativeCompatBridge) Close() {
	if b == nil || b.base == nil {
		return
	}
	for session := range b.ephemeral {
		_ = session.workspace.client.DeleteSession(context.Background(), session.workspace.value.ID, session.session.ID)
	}
	b.base.Close()
}

func (b *nativeCompatBridge) markEphemeral(session *nativeCompatSession) {
	if session != nil {
		session.deleteOnClose = true
		b.ephemeral[session] = struct{}{}
	}
}

func (b *nativeCompatBridge) resolveSession(ctx context.Context, request compatibility.Request) (*nativeCompatSession, error) {
	workspace, err := b.base.workspace(request.WorkingDir)
	if err != nil {
		return nil, err
	}
	var sess *proto.Session
	deleteOnClose := !request.Session.Persistent
	switch request.Session.Mode {
	case compatibility.SessionExplicit:
		if request.Session.ID == "" {
			return nil, errors.New("session ID is required")
		}
		sess, err = workspace.client.GetSession(ctx, workspace.value.ID, request.Session.ID)
		if err == nil && !request.Session.Persistent {
			sess, err = workspace.client.ForkSession(ctx, workspace.value.ID, sess.ID)
		}
	case compatibility.SessionLatest:
		var sessions []proto.Session
		sessions, err = workspace.client.ListSessions(ctx, workspace.value.ID)
		if err == nil {
			sort.SliceStable(sessions, func(i, j int) bool {
				if sessions[i].UpdatedAt == sessions[j].UpdatedAt {
					return sessions[i].ID > sessions[j].ID
				}
				return sessions[i].UpdatedAt > sessions[j].UpdatedAt
			})
			for i := range sessions {
				if sessions[i].ParentSessionID == "" {
					sess = &sessions[i]
					break
				}
			}
			if sess == nil {
				err = errors.New("no session available to continue")
			} else if !request.Session.Persistent {
				sess, err = workspace.client.ForkSession(ctx, workspace.value.ID, sess.ID)
			}
		}
	case compatibility.SessionFork:
		if request.Session.ID == "" {
			return nil, errors.New("source session ID is required to fork")
		}
		sess, err = workspace.client.ForkSession(ctx, workspace.value.ID, request.Session.ID)
	default:
		title := request.Prompt.Text
		if len(title) > 80 {
			title = title[:80]
		}
		sess, err = workspace.client.CreateSession(ctx, workspace.value.ID, title)
	}
	if err != nil {
		return nil, err
	}
	selection, err := b.base.selectModel(ctx, workspace, request.Model, "")
	if err != nil {
		if deleteOnClose && sess != nil {
			_ = workspace.client.DeleteSession(context.Background(), workspace.value.ID, sess.ID)
		}
		return nil, err
	}
	result := &nativeCompatSession{workspace: workspace, session: sess, model: selection, deleteOnClose: deleteOnClose}
	if deleteOnClose {
		b.markEphemeral(result)
	}
	return result, nil
}

func (b *nativeCompatBridge) getSession(ctx context.Context, workspace *codexNativeWorkspace, sessionID string) (*nativeCompatSession, error) {
	if sessionID == "" {
		return nil, errors.New("session ID is required")
	}
	sess, err := workspace.client.GetSession(ctx, workspace.value.ID, sessionID)
	if err != nil {
		return nil, err
	}
	selection, err := b.base.selectModel(ctx, workspace, "", "")
	if err != nil {
		return nil, err
	}
	return &nativeCompatSession{workspace: workspace, session: sess, model: selection}, nil
}

func (b *nativeCompatBridge) createSession(ctx context.Context, cwd, title, model string) (*nativeCompatSession, error) {
	workspace, err := b.base.workspace(cwd)
	if err != nil {
		return nil, err
	}
	sess, err := workspace.client.CreateSession(ctx, workspace.value.ID, title)
	if err != nil {
		return nil, err
	}
	selection, err := b.base.selectModel(ctx, workspace, model, "")
	if err != nil {
		return nil, err
	}
	return &nativeCompatSession{workspace: workspace, session: sess, model: selection}, nil
}

func (b *nativeCompatBridge) listSessions(ctx context.Context, cwd string) (*codexNativeWorkspace, []proto.Session, error) {
	workspace, err := b.base.workspace(cwd)
	if err != nil {
		return nil, nil, err
	}
	sessions, err := workspace.client.ListSessions(ctx, workspace.value.ID)
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(sessions, func(i, j int) bool {
		if sessions[i].UpdatedAt == sessions[j].UpdatedAt {
			return sessions[i].ID > sessions[j].ID
		}
		return sessions[i].UpdatedAt > sessions[j].UpdatedAt
	})
	return workspace, sessions, nil
}

func (b *nativeCompatBridge) selectModel(ctx context.Context, session *nativeCompatSession, model string) (config.SelectedModel, error) {
	selection, err := b.base.selectModel(ctx, session.workspace, model, "")
	if err == nil {
		session.model = selection
	}
	return selection, err
}

func (b *nativeCompatBridge) modelCatalog(ctx context.Context, workspace *codexNativeWorkspace) ([]any, error) {
	listed, err := b.base.listModels(ctx, workspace)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(listed))
	for _, raw := range listed {
		model, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		id, _ := model["id"].(string)
		name, _ := model["displayName"].(string)
		if name == "" {
			name = id
		}
		vision := false
		if modalities, ok := model["inputModalities"].([]string); ok {
			for _, modality := range modalities {
				vision = vision || modality == "image"
			}
		}
		reasoning := make([]string, 0)
		if levels, ok := model["supportedReasoningEfforts"].([]any); ok {
			for _, rawLevel := range levels {
				if level, ok := rawLevel.(map[string]any)["reasoningEffort"].(string); ok {
					reasoning = append(reasoning, level)
				}
			}
		}
		value := map[string]any{
			"id":   id,
			"name": name,
			"capabilities": map[string]any{
				"supports": map[string]any{"vision": vision, "reasoningEffort": len(reasoning) != 0},
				"limits":   map[string]any{"max_context_window_tokens": 0},
			},
		}
		if len(reasoning) != 0 {
			value["supportedReasoningEfforts"] = reasoning
			value["defaultReasoningEffort"] = model["defaultReasoningEffort"]
		}
		result = append(result, value)
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].(map[string]any)["id"].(string) < result[j].(map[string]any)["id"].(string)
	})
	return result, nil
}

func (b *nativeCompatBridge) cancel(ctx context.Context, session *nativeCompatSession) error {
	return session.workspace.client.CancelAgentSession(ctx, session.workspace.value.ID, session.session.ID)
}

func runInteractiveCodexFork(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	forked, err := bridge.resolveSession(ctx, request)
	if err != nil {
		return fmt.Errorf("fork Codex session %q: %w", request.Session.ID, err)
	}
	request.Session = compatibility.Session{Mode: compatibility.SessionExplicit, ID: forked.session.ID, Persistent: true}
	arguments, _ := nativeArguments(request)
	return runNative(ctx, invocation, request.WorkingDir, arguments, invocation.Stdin, invocation.Stdout, invocation.Stderr)
}

func runNativeCompatibilityOneShot(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) (runErr error) {
	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	session, err := bridge.resolveSession(ctx, request)
	if err != nil {
		if request.Output.Mode != "" && request.Output.Mode != compatibility.OutputText {
			_ = writeNativeOneShotError(invocation, request, "", err.Error())
			return &compatibility.ExitError{Code: 1}
		}
		return err
	}
	request.Model = session.model.Provider + "/" + session.model.Model
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
	var stream *oneShotStream
	var onChunk func(string) error
	if usesOneShotStream(request) {
		stream = newOneShotStream(invocation.Stdout, request)
		stream.conversationID = session.session.ID
		if err := stream.start(); err != nil {
			return err
		}
		onChunk = func(chunk string) error {
			_, err := stream.Write([]byte(chunk))
			return err
		}
	}
	turn := bridge.runTurn(ctx, session, prompt, nativeAgentPermissionMode(request), onChunk)
	if stream != nil {
		stream.usage = turn.Usage
	}
	if turn.Cancelled {
		if stream != nil {
			_ = stream.fail("Interrupted")
		} else {
			_ = writeNativeOneShotError(invocation, request, session.session.ID, "Interrupted")
		}
		return &compatibility.ExitError{Code: 1}
	}
	if turn.Err != nil {
		if stream != nil {
			_ = stream.fail(turn.Err.Error())
		} else {
			_ = writeNativeOneShotError(invocation, request, session.session.ID, turn.Err.Error())
		}
		return &compatibility.ExitError{Code: 1}
	}
	if stream != nil && stream.capture.Len() == 0 && turn.Text != "" {
		_, _ = stream.capture.WriteString(turn.Text)
	}
	if _, err := structuredResult(request, turn.Text); err != nil {
		message := "structured output validation failed: " + err.Error()
		if stream != nil {
			_ = stream.fail(message)
		} else {
			_ = writeNativeOneShotError(invocation, request, session.session.ID, message)
		}
		return &compatibility.ExitError{Code: 1}
	}
	if request.Output.LastMessagePath != "" {
		if err := writeLastMessage(request.Output.LastMessagePath, turn.Text); err != nil {
			return err
		}
	}
	if stream != nil {
		if err := stream.complete(); err != nil {
			return err
		}
	} else if err := writeNativeOneShotSuccess(invocation, request, session.session.ID, turn); err != nil {
		return err
	}
	return writeCompatibilityLog(request, nil)
}

func writeNativeOneShotSuccess(invocation compatibility.Invocation, request compatibility.Request, sessionID string, turn nativeCompatTurnResult) error {
	structured, _ := structuredResult(request, turn.Text)
	switch request.Output.Mode {
	case "", compatibility.OutputText:
		_, err := fmt.Fprintln(invocation.Stdout, turn.Text)
		return err
	case compatibility.OutputJSON:
		var value map[string]any
		switch request.Source {
		case "claude":
			value = map[string]any{"type": "result", "subtype": "success", "uuid": uuid.NewString(), "session_id": sessionID, "duration_ms": 0, "duration_api_ms": 0, "is_error": false, "num_turns": 1, "result": turn.Text, "stop_reason": "end_turn", "total_cost_usd": turn.Usage.Cost, "usage": map[string]int64{"input_tokens": turn.Usage.InputTokens, "output_tokens": turn.Usage.OutputTokens}, "modelUsage": map[string]any{request.Model: map[string]any{"inputTokens": turn.Usage.InputTokens, "outputTokens": turn.Usage.OutputTokens, "costUSD": turn.Usage.Cost}}, "permission_denials": []any{}}
		case "agy":
			value = map[string]any{"conversation_id": sessionID, "status": "SUCCESS", "response": turn.Text, "duration_seconds": 0, "num_turns": 1, "usage": agyUsage(turn.Usage)}
		default:
			value = map[string]any{"result": turn.Text, "session_id": sessionID}
		}
		if structured != nil {
			value["structured_output"] = structured
		}
		return writeJSON(invocation.Stdout, value)
	default:
		return fmt.Errorf("unsupported native one-shot output mode %q", request.Output.Mode)
	}
}

func writeNativeOneShotError(invocation compatibility.Invocation, request compatibility.Request, sessionID, message string) error {
	switch request.Source {
	case "claude":
		return writeJSON(invocation.Stdout, map[string]any{"type": "result", "subtype": "error_during_execution", "uuid": uuid.NewString(), "session_id": sessionID, "duration_ms": 0, "duration_api_ms": 0, "is_error": true, "num_turns": 0, "errors": []string{message}, "stop_reason": "error", "total_cost_usd": 0, "usage": map[string]int{}, "modelUsage": map[string]any{}, "permission_denials": []any{}})
	case "agy":
		return writeJSON(invocation.Stdout, map[string]any{"conversation_id": sessionID, "status": "ERROR", "response": "", "error": message, "duration_seconds": 0, "num_turns": 0, "usage": zeroUsage()})
	default:
		return writeJSON(invocation.Stdout, map[string]any{"type": "error", "message": message, "session_id": sessionID})
	}
}

func (b *nativeCompatBridge) runTurn(ctx context.Context, session *nativeCompatSession, prompt string, permissionMode proto.AgentPermissionMode, onChunk func(string) error) nativeCompatTurnResult {
	result := nativeCompatTurnResult{}
	if err := b.base.ensureAgent(ctx, session.workspace); err != nil {
		result.Err = err
		return result
	}
	before, err := session.workspace.client.GetSession(ctx, session.workspace.value.ID, session.session.ID)
	if err != nil {
		result.Err = err
		return result
	}
	runID := uuid.NewString()
	if err := session.workspace.client.SendMessageWithPermissionMode(ctx, session.workspace.value.ID, session.session.ID, runID, prompt, permissionMode); err != nil {
		result.Err = err
		return result
	}
	read := make(map[string]int)
	for {
		select {
		case <-ctx.Done():
			_ = b.cancel(context.Background(), session)
			result.Cancelled = true
			return result
		case event, ok := <-session.workspace.events:
			if !ok {
				result.Err = errors.New("Crux event stream closed")
				return result
			}
			switch value := event.(type) {
			case pubsub.Event[proto.Message]:
				msg := value.Payload
				if msg.SessionID != session.session.ID || msg.Role != proto.Assistant {
					continue
				}
				text := msg.Content().String()
				offset := read[msg.ID]
				if offset > len(text) {
					offset = 0
				}
				delta := text[offset:]
				read[msg.ID] = len(text)
				if delta != "" {
					result.Text += delta
					if onChunk != nil {
						if err := onChunk(delta); err != nil {
							result.Err = err
							return result
						}
					}
				}
			case pubsub.Event[proto.RunComplete]:
				complete := value.Payload
				if complete.RunID != runID {
					continue
				}
				if len(result.Text) < len(complete.Text) {
					delta := complete.Text[len(result.Text):]
					result.Text = complete.Text
					if delta != "" && onChunk != nil {
						if err := onChunk(delta); err != nil {
							result.Err = err
							return result
						}
					}
				}
				result.Cancelled = complete.Cancelled
				if complete.Error != "" && !complete.Cancelled {
					result.Err = errors.New(complete.Error)
				}
				after, getErr := session.workspace.client.GetSession(context.Background(), session.workspace.value.ID, session.session.ID)
				if getErr == nil {
					session.session = after
					result.Usage = nativeCompatUsage{
						InputTokens:  max(after.PromptTokens-before.PromptTokens, 0),
						OutputTokens: max(after.CompletionTokens-before.CompletionTokens, 0),
						Cost:         max(after.Cost-before.Cost, 0),
					}
				}
				return result
			case pubsub.Event[proto.AgentEvent]:
				if value.Payload.Error != nil && value.Payload.RunID == runID {
					result.Err = fmt.Errorf("agent run failed: %w", value.Payload.Error)
					return result
				}
			}
		}
	}
}
