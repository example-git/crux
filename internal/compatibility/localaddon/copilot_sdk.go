package localaddon

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
)

func runCopilotSDK(ctx context.Context, invocation compatibility.Invocation, request compatibility.Request) error {
	type sessionState struct {
		workingDir  string
		turns       int
		events      []map[string]any
		lastEventID string
		native      *nativeCompatSession
		streaming   bool
	}
	type activeTurn struct {
		sessionID string
		messageID string
		cancel    context.CancelFunc
	}
	type turnResult struct {
		sessionID string
		messageID string
		turnID    string
		model     string
		text      string
		usage     nativeCompatUsage
		cancelled bool
		err       error
	}

	bridge, err := newNativeCompatBridge(ctx, invocation, request)
	if err != nil {
		return err
	}
	defer bridge.Close()
	output := &protocolOutput{writer: invocation.Stdout, contentLength: true}
	lines := make(chan []byte)
	scanErrors := make(chan error, 1)
	go func() {
		defer close(lines)
		reader := bufio.NewReader(request.Prompt.Stdin)
		for {
			line, err := readCopilotSDKFrame(reader)
			if err != nil {
				if errors.Is(err, io.EOF) {
					scanErrors <- nil
				} else {
					scanErrors <- err
				}
				return
			}
			select {
			case lines <- line:
			case <-ctx.Done():
				return
			}
		}
	}()

	sessions := make(map[string]*sessionState)
	var sessionsMu sync.Mutex
	var active *activeTurn
	turnDone := make(chan turnResult, 1)
	connected := false
	appendEvent := func(sessionID, eventType string, data map[string]any, ephemeral bool) (map[string]any, error) {
		event := map[string]any{
			"id": uuid.NewString(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
			"parentId": nil, "type": eventType, "data": data,
		}
		if ephemeral {
			event["ephemeral"] = true
		}
		sessionsMu.Lock()
		state := sessions[sessionID]
		if state != nil {
			if state.lastEventID != "" {
				event["parentId"] = state.lastEventID
			}
			state.lastEventID = event["id"].(string)
			if !ephemeral {
				state.events = append(state.events, event)
			}
		}
		sessionsMu.Unlock()
		if err := output.write(map[string]any{"jsonrpc": "2.0", "method": "session.event", "params": map[string]any{"sessionId": sessionID, "event": event}}); err != nil {
			return nil, err
		}
		return event, nil
	}

	for lines != nil || active != nil {
		select {
		case <-ctx.Done():
			if active != nil {
				active.cancel()
			}
			return ctx.Err()
		case result := <-turnDone:
			sessionsMu.Lock()
			state := sessions[result.sessionID]
			if state != nil {
				state.turns++
			}
			sessionsMu.Unlock()
			if result.cancelled {
				_, _ = appendEvent(result.sessionID, "abort", map[string]any{"reason": "user_initiated"}, false)
			} else if result.err != nil {
				_, _ = appendEvent(result.sessionID, "session.error", map[string]any{"errorType": "model_call", "message": result.err.Error()}, false)
			} else {
				if _, err := appendEvent(result.sessionID, "assistant.message", map[string]any{"messageId": result.messageID, "content": result.text, "turnId": result.turnID, "model": result.model}, false); err != nil {
					return err
				}
				if _, err := appendEvent(result.sessionID, "assistant.turn_end", map[string]any{"turnId": result.turnID}, false); err != nil {
					return err
				}
				if _, err := appendEvent(result.sessionID, "assistant.usage", map[string]any{"model": result.model, "inputTokens": result.usage.InputTokens, "outputTokens": result.usage.OutputTokens, "cost": result.usage.Cost}, true); err != nil {
					return err
				}
			}
			idleData := map[string]any{}
			if result.cancelled {
				idleData["aborted"] = true
			}
			if _, err := appendEvent(result.sessionID, "assistant.idle", idleData, true); err != nil {
				return err
			}
			if _, err := appendEvent(result.sessionID, "session.idle", map[string]any{}, true); err != nil {
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
			if message.Method != "connect" && !connected {
				if len(message.ID) != 0 {
					if err := writeRPCError(output, message.ID, -32001, "Client is not connected"); err != nil {
						return err
					}
				}
				continue
			}
			switch message.Method {
			case "connect":
				connected = true
				if err := writeRPCResult(output, message.ID, map[string]any{"ok": true, "protocolVersion": 3, "version": "1.0.80"}); err != nil {
					return err
				}
			case "ping":
				if err := writeRPCResult(output, message.ID, map[string]any{"message": "pong", "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "protocolVersion": 3}); err != nil {
					return err
				}
			case "status.get":
				if err := writeRPCResult(output, message.ID, map[string]any{"version": "1.0.80", "protocolVersion": 3}); err != nil {
					return err
				}
			case "auth.getStatus":
				workspace, workspaceErr := bridge.base.workspace(request.WorkingDir)
				if workspaceErr != nil {
					if err := writeRPCError(output, message.ID, -32000, workspaceErr.Error()); err != nil {
						return err
					}
					continue
				}
				surfaces, surfaceErr := workspace.client.ProviderSurfaces(ctx, workspace.value.ID)
				if surfaceErr != nil {
					if err := writeRPCError(output, message.ID, -32000, surfaceErr.Error()); err != nil {
						return err
					}
					continue
				}
				authenticated := false
				for _, surface := range surfaces {
					if surface.Available && len(surface.Models) != 0 {
						authenticated = true
						break
					}
				}
				statusMessage := "No configured Crux provider is available"
				if authenticated {
					statusMessage = "Crux provider access is configured"
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"isAuthenticated": authenticated, "statusMessage": statusMessage}); err != nil {
					return err
				}
			case "models.list":
				workspace, workspaceErr := bridge.base.workspace(request.WorkingDir)
				if workspaceErr != nil {
					if err := writeRPCError(output, message.ID, -32000, workspaceErr.Error()); err != nil {
						return err
					}
					continue
				}
				models, modelErr := bridge.modelCatalog(ctx, workspace)
				if modelErr != nil {
					if err := writeRPCError(output, message.ID, -32000, modelErr.Error()); err != nil {
						return err
					}
					continue
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"models": models}); err != nil {
					return err
				}
			case "session.model.getCurrent":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				if state == nil || state.native == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				modelID := state.native.model.Provider + "/" + state.native.model.Model
				if err := writeRPCResult(output, message.ID, map[string]any{"modelId": modelID}); err != nil {
					return err
				}
			case "session.model.switchTo":
				var params struct {
					SessionID string `json:"sessionId"`
					ModelID   string `json:"modelId"`
				}
				if json.Unmarshal(message.Params, &params) != nil || params.ModelID == "" {
					if err := writeRPCError(output, message.ID, -32602, "Invalid model switch params"); err != nil {
						return err
					}
					continue
				}
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				if state == nil || state.native == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				selection, modelErr := bridge.selectModel(ctx, state.native, params.ModelID)
				if modelErr != nil {
					if err := writeRPCError(output, message.ID, -32602, modelErr.Error()); err != nil {
						return err
					}
					continue
				}
				modelID := selection.Provider + "/" + selection.Model
				if err := writeRPCResult(output, message.ID, map[string]any{"modelId": modelID, "deferred": false}); err != nil {
					return err
				}
			case "session.model.list":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				if state == nil || state.native == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				models, modelErr := bridge.modelCatalog(ctx, state.native.workspace)
				if modelErr != nil {
					if err := writeRPCError(output, message.ID, -32000, modelErr.Error()); err != nil {
						return err
					}
					continue
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"list": models}); err != nil {
					return err
				}
			case "session.resume":
				var params struct {
					SessionID        string `json:"sessionId"`
					WorkingDirectory string `json:"workingDirectory"`
					Model            string `json:"model"`
					Streaming        bool   `json:"streaming"`
					Config           struct {
						CWD              string `json:"cwd"`
						WorkingDirectory string `json:"workingDirectory"`
						Streaming        bool   `json:"streaming"`
					} `json:"config"`
				}
				if err := json.Unmarshal(message.Params, &params); err != nil || params.SessionID == "" {
					if err := writeRPCError(output, message.ID, -32602, "Invalid session.resume params"); err != nil {
						return err
					}
					continue
				}
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				if state == nil {
					cwd := params.WorkingDirectory
					if cwd == "" {
						cwd = params.Config.CWD
					}
					if cwd == "" {
						cwd = params.Config.WorkingDirectory
					}
					if cwd == "" {
						cwd = request.WorkingDir
					}
					workspace, workspaceErr := bridge.base.workspace(cwd)
					if workspaceErr == nil {
						nativeID := params.SessionID
						if bindings, bindingErr := loadCopilotBindings(workspace.value); bindingErr == nil && bindings[params.SessionID] != "" {
							nativeID = bindings[params.SessionID]
						}
						nativeSession, getErr := bridge.getSession(ctx, workspace, nativeID)
						if getErr == nil {
							state = &sessionState{workingDir: cwd, native: nativeSession, streaming: params.Streaming || params.Config.Streaming}
							sessionsMu.Lock()
							sessions[params.SessionID] = state
							sessionsMu.Unlock()
						}
					}
				}
				if state == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				state.streaming = params.Streaming || params.Config.Streaming
				if params.Model != "" {
					if _, modelErr := bridge.selectModel(ctx, state.native, params.Model); modelErr != nil {
						if err := writeRPCError(output, message.ID, -32602, modelErr.Error()); err != nil {
							return err
						}
						continue
					}
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"sessionId": params.SessionID, "capabilities": map[string]any{}, "workspacePath": state.workingDir}); err != nil {
					return err
				}
			case "sessions.open", "session.create":
				var params struct {
					SessionID        string `json:"sessionId"`
					CWD              string `json:"cwd"`
					WorkingDirectory string `json:"workingDirectory"`
					Model            string `json:"model"`
					Streaming        bool   `json:"streaming"`
					Config           struct {
						CWD              string `json:"cwd"`
						WorkingDirectory string `json:"workingDirectory"`
						Model            string `json:"model"`
						Streaming        bool   `json:"streaming"`
					} `json:"config"`
				}
				if err := json.Unmarshal(message.Params, &params); err != nil {
					if err := writeRPCError(output, message.ID, -32602, "Invalid sessions.open params"); err != nil {
						return err
					}
					continue
				}
				workingDir := params.CWD
				if workingDir == "" {
					workingDir = params.WorkingDirectory
				}
				if workingDir == "" {
					workingDir = params.Config.CWD
				}
				if workingDir == "" {
					workingDir = params.Config.WorkingDirectory
				}
				if workingDir == "" {
					workingDir = request.WorkingDir
				}
				if !filepath.IsAbs(workingDir) {
					if err := writeRPCError(output, message.ID, -32602, "working directory must be absolute"); err != nil {
						return err
					}
					continue
				}
				model := params.Model
				if model == "" {
					model = params.Config.Model
				}
				streaming := params.Streaming || params.Config.Streaming
				nativeSession, createErr := bridge.createSession(ctx, workingDir, "Copilot SDK session", model)
				if nativeSession != nil && !request.Session.Persistent {
					bridge.markEphemeral(nativeSession)
				}
				if createErr != nil {
					if err := writeRPCError(output, message.ID, -32000, createErr.Error()); err != nil {
						return err
					}
					continue
				}
				sessionID := params.SessionID
				if sessionID == "" {
					sessionID = nativeSession.session.ID
				}
				sessionsMu.Lock()
				sessions[sessionID] = &sessionState{workingDir: workingDir, native: nativeSession, streaming: streaming}
				sessionsMu.Unlock()
				if params.SessionID != "" {
					if bindingErr := saveCopilotBinding(nativeSession.workspace.value, sessionID, nativeSession.session.ID); bindingErr != nil {
						if err := writeRPCError(output, message.ID, -32000, bindingErr.Error()); err != nil {
							return err
						}
						continue
					}
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"status": "ready", "sessionId": sessionID, "capabilities": map[string]any{}, "workspacePath": workingDir, "startupPrompts": []any{}}); err != nil {
					return err
				}
				if _, err := appendEvent(sessionID, "session.start", map[string]any{"sessionId": sessionID, "version": 1, "producer": "crux", "copilotVersion": "1.0.80", "startTime": time.Now().UTC().Format(time.RFC3339Nano)}, false); err != nil {
					return err
				}
			case "sessions.list", "session.list":
				_, nativeSessions, listErr := bridge.listSessions(ctx, request.WorkingDir)
				if listErr != nil {
					if err := writeRPCError(output, message.ID, -32000, listErr.Error()); err != nil {
						return err
					}
					continue
				}
				reverse := make(map[string]string)
				if workspace, workspaceErr := bridge.base.workspace(request.WorkingDir); workspaceErr == nil {
					if bindings, bindingErr := loadCopilotBindings(workspace.value); bindingErr == nil {
						for externalID, nativeID := range bindings {
							reverse[nativeID] = externalID
						}
					}
				}
				sessionsMu.Lock()
				for externalID, state := range sessions {
					if state.native != nil {
						reverse[state.native.session.ID] = externalID
					}
				}
				sessionsMu.Unlock()
				values := make([]any, 0, len(nativeSessions))
				for _, sess := range nativeSessions {
					sessionID := reverse[sess.ID]
					if sessionID == "" {
						sessionID = sess.ID
					}
					values = append(values, copilotSessionMetadata(sessionID, &sess))
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"sessions": values}); err != nil {
					return err
				}
			case "session.getLastId":
				_, nativeSessions, listErr := bridge.listSessions(ctx, request.WorkingDir)
				if listErr != nil {
					if err := writeRPCError(output, message.ID, -32000, listErr.Error()); err != nil {
						return err
					}
					continue
				}
				var sessionID any
				if len(nativeSessions) != 0 {
					lastID := nativeSessions[0].ID
					if workspace, workspaceErr := bridge.base.workspace(request.WorkingDir); workspaceErr == nil {
						if bindings, bindingErr := loadCopilotBindings(workspace.value); bindingErr == nil {
							for externalID, nativeID := range bindings {
								if nativeID == lastID {
									lastID = externalID
									break
								}
							}
						}
					}
					sessionID = lastID
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"sessionId": sessionID}); err != nil {
					return err
				}
			case "session.getMetadata":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				var sess *proto.Session
				if state != nil && state.native != nil {
					sess = state.native.session
				} else {
					workspace, workspaceErr := bridge.base.workspace(request.WorkingDir)
					if workspaceErr == nil {
						nativeID := params.SessionID
						if bindings, bindingErr := loadCopilotBindings(workspace.value); bindingErr == nil && bindings[params.SessionID] != "" {
							nativeID = bindings[params.SessionID]
						}
						sess, _ = workspace.client.GetSession(ctx, workspace.value.ID, nativeID)
					}
				}
				var metadata any
				if sess != nil {
					metadata = copilotSessionMetadata(params.SessionID, sess)
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"session": metadata}); err != nil {
					return err
				}
			case "sessions.close", "session.shutdown", "session.destroy", "session.delete":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if active != nil && active.sessionID == params.SessionID {
					active.cancel()
				}
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				delete(sessions, params.SessionID)
				sessionsMu.Unlock()
				if message.Method == "session.delete" && state == nil {
					if workspace, workspaceErr := bridge.base.workspace(request.WorkingDir); workspaceErr == nil {
						if bindings, bindingErr := loadCopilotBindings(workspace.value); bindingErr == nil {
							if nativeID := bindings[params.SessionID]; nativeID != "" {
								if nativeSession, getErr := bridge.getSession(ctx, workspace, nativeID); getErr == nil {
									state = &sessionState{workingDir: request.WorkingDir, native: nativeSession}
								}
							}
						}
					}
				}
				if message.Method == "session.delete" {
					if state == nil || state.native == nil {
						if err := writeRPCResult(output, message.ID, map[string]any{"success": false, "error": "Session not found"}); err != nil {
							return err
						}
						continue
					}
					if deleteErr := state.native.workspace.client.DeleteSession(ctx, state.native.workspace.value.ID, state.native.session.ID); deleteErr != nil {
						if err := writeRPCResult(output, message.ID, map[string]any{"success": false, "error": deleteErr.Error()}); err != nil {
							return err
						}
						continue
					}
					if bindingErr := deleteCopilotBinding(state.native.workspace.value, params.SessionID); bindingErr != nil {
						if err := writeRPCResult(output, message.ID, map[string]any{"success": false, "error": bindingErr.Error()}); err != nil {
							return err
						}
						continue
					}
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"ok": true, "success": true}); err != nil {
					return err
				}
			case "session.send":
				if active != nil {
					if err := writeRPCError(output, message.ID, -32003, "A prompt is already running"); err != nil {
						return err
					}
					continue
				}
				var params struct {
					SessionID string          `json:"sessionId"`
					Prompt    json.RawMessage `json:"prompt"`
				}
				if err := json.Unmarshal(message.Params, &params); err != nil {
					if err := writeRPCError(output, message.ID, -32602, "Invalid session.send params"); err != nil {
						return err
					}
					continue
				}
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				sessionsMu.Unlock()
				if state == nil {
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				var prompt string
				if json.Unmarshal(params.Prompt, &prompt) != nil {
					var err error
					prompt, err = protocolTextBlocks(params.Prompt)
					if err != nil {
						if err := writeRPCError(output, message.ID, -32602, err.Error()); err != nil {
							return err
						}
						continue
					}
				}
				messageID := uuid.NewString()
				turnID := uuid.NewString()
				if err := writeRPCResult(output, message.ID, map[string]any{"messageId": messageID}); err != nil {
					return err
				}
				if _, err := appendEvent(params.SessionID, "user.message", map[string]any{"content": prompt}, false); err != nil {
					return err
				}
				if _, err := appendEvent(params.SessionID, "assistant.turn_start", map[string]any{"turnId": turnID}, false); err != nil {
					return err
				}
				if _, err := appendEvent(params.SessionID, "assistant.message_start", map[string]any{"messageId": messageID, "turnId": turnID}, true); err != nil {
					return err
				}
				turnCtx, cancel := context.WithCancel(ctx)
				active = &activeTurn{sessionID: params.SessionID, messageID: messageID, cancel: cancel}
				go func(sessionID, messageID, turnID, prompt string) {
					turn := bridge.runTurn(turnCtx, state.native, prompt, nativeAgentPermissionMode(request), func(chunk string) error {
						if !state.streaming {
							return nil
						}
						_, err := appendEvent(sessionID, "assistant.message_delta", map[string]any{"messageId": messageID, "deltaContent": chunk}, true)
						return err
					})
					model := state.native.model.Provider + "/" + state.native.model.Model
					turnDone <- turnResult{sessionID: sessionID, messageID: messageID, turnID: turnID, model: model, text: turn.Text, usage: turn.Usage, cancelled: turn.Cancelled, err: turn.Err}
				}(params.SessionID, messageID, turnID, prompt)
			case "session.abort":
				var params struct {
					SessionID string `json:"sessionId"`
				}
				_ = json.Unmarshal(message.Params, &params)
				if active != nil && active.sessionID == params.SessionID {
					active.cancel()
				}
				if err := writeRPCResult(output, message.ID, map[string]any{"ok": true}); err != nil {
					return err
				}
			case "session.eventLog.read":
				var params struct {
					SessionID string `json:"sessionId"`
					Cursor    string `json:"cursor"`
					Max       int    `json:"max"`
					Limit     int    `json:"limit"`
				}
				if err := json.Unmarshal(message.Params, &params); err != nil {
					if err := writeRPCError(output, message.ID, -32602, "Invalid event-log params"); err != nil {
						return err
					}
					continue
				}
				cursor := 0
				if params.Cursor != "" {
					decoded, decodeErr := base64.RawURLEncoding.DecodeString(params.Cursor)
					parsed, err := strconv.Atoi(string(decoded))
					if decodeErr != nil || err != nil || parsed < 0 {
						if err := writeRPCError(output, message.ID, -32602, "Invalid cursor"); err != nil {
							return err
						}
						continue
					}
					cursor = parsed
				}
				if params.Max <= 0 {
					params.Max = params.Limit
				}
				if params.Max <= 0 {
					params.Max = 200
				}
				sessionsMu.Lock()
				state := sessions[params.SessionID]
				if state == nil {
					sessionsMu.Unlock()
					if err := writeRPCError(output, message.ID, -32002, "Session not found"); err != nil {
						return err
					}
					continue
				}
				if cursor > len(state.events) {
					cursor = len(state.events)
				}
				end := min(cursor+params.Max, len(state.events))
				events := append([]map[string]any(nil), state.events[cursor:end]...)
				hasMore := end < len(state.events)
				sessionsMu.Unlock()
				nextCursor := base64.RawURLEncoding.EncodeToString([]byte(strconv.Itoa(end)))
				if err := writeRPCResult(output, message.ID, map[string]any{"events": events, "cursor": nextCursor, "hasMore": hasMore, "cursorStatus": "ok"}); err != nil {
					return err
				}
			default:
				if len(message.ID) != 0 {
					if err := writeRPCError(output, message.ID, -32601, fmt.Sprintf("Method not found: %s", message.Method)); err != nil {
						return err
					}
				}
			}
		}
	}
	return <-scanErrors
}
