package localaddon

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func protocolFixture(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	workingDir := t.TempDir()
	executable := filepath.Join(workingDir, "crux")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\nprintf 'streamed response\\n'\n"), 0o700))
	return executable, workingDir
}

func protocolInvocation(executable, workingDir string, stdin io.Reader, stdout io.Writer) compatibility.Invocation {
	return compatibility.Invocation{
		Executable: executable,
		WorkingDir: workingDir,
		Env:        []string{compatibility.BypassEnvironment + "=1"},
		Stdin:      stdin,
		Stdout:     stdout,
		Stderr:     io.Discard,
	}
}

func codexNativeAPIFixture(t *testing.T) string {
	return nativeAPIFixture(t, "streamed response")
}

func nativeAPIFixture(t *testing.T, responseText string) string {
	t.Helper()
	workingDir := t.TempDir()
	var mu sync.Mutex
	sessions := make(map[string]map[string]any)
	cancelled := make(map[string]bool)
	events := make(chan map[string]any, 16)
	providerSurfaces := func() []any {
		return []any{
			map[string]any{"id": "openai", "name": "OpenAI", "owner": map[string]any{"provider_id": "openai", "construction": "openai-compat"}, "available": true, "availability": "available", "models": []any{
				map[string]any{"id": "gpt-5", "name": "GPT 5", "default_reasoning_effort": "high", "reasoning_levels": []string{"medium", "high"}, "supports_images": true},
				map[string]any{"id": "gpt-4.1", "name": "GPT 4.1"},
			}},
			map[string]any{"id": "anthropic", "name": "Anthropic", "owner": map[string]any{"provider_id": "anthropic", "construction": "openai-compat"}, "available": true, "availability": "available", "models": []any{
				map[string]any{"id": "claude-sonnet", "name": "Claude Sonnet"},
			}},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1")
		switch {
		case path == "/health":
			w.WriteHeader(http.StatusOK)
		case path == "/workspaces" && r.Method == http.MethodPost:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "workspace-1", "path": workingDir})
		case path == "/workspaces/workspace-1" && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "workspace-1", "path": workingDir,
				"config":            map[string]any{"models": map[string]any{"large": map[string]any{"provider": "openai", "model": "gpt-5"}}},
				"provider_surfaces": providerSurfaces(),
			})
		case strings.HasSuffix(path, "/providers"):
			_ = json.NewEncoder(w).Encode(providerSurfaces())
		case strings.HasSuffix(path, "/config") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"models": map[string]any{"large": map[string]any{"provider": "openai", "model": "gpt-5"}}})
		case strings.HasSuffix(path, "/config/model") && r.Method == http.MethodPost:
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			model, _ := request["model"].(map[string]any)
			owner, _ := request["owner"].(map[string]any)
			if model["provider"] != "anthropic" || model["model"] != "claude-sonnet" {
				http.Error(w, "compatibility model was not translated", http.StatusBadRequest)
				return
			}
			if owner["provider_id"] != "anthropic" {
				http.Error(w, "compatibility provider owner was not retained", http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"large": map[string]any{
					"model": model,
					"owner": owner,
				},
			})
		case strings.HasSuffix(path, "/events"):
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			flusher.Flush()
			for {
				select {
				case event := <-events:
					payload, _ := json.Marshal(map[string]any{"type": "run_complete", "payload": map[string]any{"type": "updated", "payload": event}})
					_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
					flusher.Flush()
				case <-r.Context().Done():
					return
				}
			}
		case strings.HasSuffix(path, "/agent/init") || strings.HasSuffix(path, "/agent/update"):
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]any{"is_ready": true})
		case strings.HasSuffix(path, "/agent") && r.Method == http.MethodPost:
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			sessionID, _ := request["session_id"].(string)
			if sessionID != "thread-1" {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			runID, _ := request["run_id"].(string)
			go func() {
				time.Sleep(25 * time.Millisecond)
				mu.Lock()
				wasCancelled := cancelled[sessionID]
				mu.Unlock()
				events <- map[string]any{"session_id": sessionID, "run_id": runID, "message_id": "message-1", "text": responseText, "cancelled": wasCancelled}
			}()
			w.WriteHeader(http.StatusAccepted)
		case strings.HasSuffix(path, "/cancel"):
			parts := strings.Split(path, "/")
			sessionID := parts[len(parts)-2]
			if sessionID != "thread-1" {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			mu.Lock()
			cancelled[sessionID] = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/fork"):
			parts := strings.Split(path, "/")
			sourceID := parts[len(parts)-2]
			if sourceID != "thread-1" {
				http.Error(w, "compatibility ID was not translated", http.StatusBadRequest)
				return
			}
			mu.Lock()
			source := sessions[sourceID]
			forked := map[string]any{"id": "fork-" + sourceID, "title": source["title"], "created_at": int64(1), "updated_at": int64(1)}
			sessions[forked["id"].(string)] = forked
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(forked)
		case strings.Contains(path, "/sessions/") && r.Method == http.MethodGet:
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			mu.Lock()
			sess := sessions[id]
			mu.Unlock()
			if sess == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(sess)
		case strings.Contains(path, "/sessions/") && r.Method == http.MethodDelete:
			parts := strings.Split(path, "/")
			id := parts[len(parts)-1]
			mu.Lock()
			delete(sessions, id)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(path, "/sessions") && r.Method == http.MethodPost:
			id := "thread-1"
			mu.Lock()
			sess := map[string]any{"id": id, "title": "Codex", "created_at": int64(1), "updated_at": int64(1)}
			sessions[id] = sess
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(sess)
		case strings.HasSuffix(path, "/sessions") && r.Method == http.MethodGet:
			mu.Lock()
			values := make([]map[string]any, 0, len(sessions))
			for _, sess := range sessions {
				values = append(values, sess)
			}
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(values)
		case strings.HasPrefix(path, "/clients/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	address := strings.TrimPrefix(server.URL, "http://")
	previous := codexClientFactory
	codexClientFactory = func(path string) (*client.Client, error) { return client.NewClient(path, "tcp", address) }
	t.Cleanup(func() { codexClientFactory = previous })
	return workingDir
}

func decodeJSONLines(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var values []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		var value map[string]any
		require.NoError(t, json.Unmarshal(scanner.Bytes(), &value))
		values = append(values, value)
	}
	require.NoError(t, scanner.Err())
	return values
}

func TestAgyStreamJSONMaintainsConversationAcrossTurns(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"event\":\"user\",\"message\":{\"content\":\"first\"}}\n{\"event\":\"user\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"second\"}]}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{
		Source: "agy", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:  compatibility.Output{Mode: compatibility.OutputStreamJSON},
	}
	require.NoError(t, runAgyStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Equal(t, "init", lines[0]["event"])
	require.Equal(t, "thread-1", lines[0]["conversation_id"])
	var results []map[string]any
	for _, line := range lines {
		if line["event"] == "result" {
			results = append(results, line["result"].(map[string]any))
		}
	}
	require.Len(t, results, 2)
	require.Equal(t, results[0]["conversation_id"], results[1]["conversation_id"])
	require.Equal(t, float64(2), results[1]["num_turns"])
}

func TestAgyStreamJSONAppliesPerTurnTimeout(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"event\":\"user\",\"message\":{\"content\":\"hello\"}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{Source: "agy", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}, Limits: compatibility.Limits{Timeout: 10 * time.Millisecond}}
	err := runAgyStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request)
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, 1, exitError.Code)
	lines := decodeJSONLines(t, stdout.Bytes())
	result := lines[len(lines)-1]["result"].(map[string]any)
	require.Equal(t, "ERROR", result["status"])
	require.Contains(t, result["error"], "timed out")
}

func TestClaudeStreamJSONEmitsControlAndPartialEvents(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"type\":\"control_request\",\"request_id\":\"r1\",\"request\":{\"subtype\":\"initialize\"}}\n{\"type\":\"user\",\"uuid\":\"u1\",\"message\":{\"role\":\"user\",\"content\":\"hello\"},\"parent_tool_use_id\":null}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{
		Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:   compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin},
		Session:  compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Output:   compatibility.Output{Mode: compatibility.OutputStreamJSON},
		Metadata: map[string]string{"include-partial-messages": "true", "replay-user-messages": "true"},
	}
	require.NoError(t, runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Equal(t, "system", lines[0]["type"])
	require.Equal(t, "thread-1", lines[0]["session_id"])
	types := make(map[string]bool)
	streamEvents := make(map[string]bool)
	for _, line := range lines {
		types[line["type"].(string)] = true
		if line["type"] == "stream_event" {
			streamEvents[line["event"].(map[string]any)["type"].(string)] = true
		}
	}
	require.True(t, types["control_response"])
	require.True(t, types["user"])
	require.True(t, types["stream_event"])
	require.True(t, types["assistant"])
	require.True(t, types["result"])
	for _, eventType := range []string{"message_start", "content_block_start", "content_block_delta", "content_block_stop", "message_delta", "message_stop"} {
		require.True(t, streamEvents[eventType], eventType)
	}
}

func TestClaudeStreamJSONReturnsSchemaShapedControlResponses(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	stdin := bytes.NewBufferString("{\"type\":\"control_request\",\"request_id\":\"context\",\"request\":{\"subtype\":\"get_context_usage\"}}\n{\"type\":\"control_request\",\"request_id\":\"cancel\",\"request\":{\"subtype\":\"cancel_async_message\",\"message_uuid\":\"missing\"}}\n{\"type\":\"control_request\",\"request_id\":\"unsupported\",\"request\":{\"subtype\":\"mcp_message\"}}\n")
	var stdout bytes.Buffer
	request := compatibility.Request{Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}}
	require.NoError(t, runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, stdin, &stdout), request))
	lines := decodeJSONLines(t, stdout.Bytes())
	require.Len(t, lines, 4)
	contextResponse := lines[1]["response"].(map[string]any)
	contextResult := contextResponse["response"].(map[string]any)
	require.Contains(t, contextResult, "categories")
	require.Contains(t, contextResult, "gridRows")
	cancelResponse := lines[2]["response"].(map[string]any)["response"].(map[string]any)
	require.Equal(t, false, cancelResponse["cancelled"])
	unsupported := lines[3]["response"].(map[string]any)
	require.Equal(t, "error", unsupported["subtype"])
}

func TestClaudeStreamJSONInterruptsActiveTurn(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}}
	done := make(chan error, 1)
	go func() {
		done <- runClaudeStream(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "system", response["type"])
	require.NoError(t, encoder.Encode(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello"}, "parent_tool_use_id": nil}))
	require.NoError(t, encoder.Encode(map[string]any{"type": "control_request", "request_id": "r1", "request": map[string]any{"subtype": "interrupt", "cancel_queued": true}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "control_response", response["type"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "result", response["type"])
	require.Equal(t, "error_during_execution", response["subtype"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestClaudeSDKWebSocketTransport(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	resultReceived := make(chan bool, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}).Upgrade(writer, request, nil)
		require.NoError(t, err)
		defer connection.Close()
		_, initial, err := connection.ReadMessage()
		require.NoError(t, err)
		var message map[string]any
		require.NoError(t, json.Unmarshal(initial, &message))
		require.Equal(t, "system", message["type"])
		require.NoError(t, connection.WriteJSON(map[string]any{"type": "user", "uuid": "u1", "message": map[string]any{"role": "user", "content": "hello"}}))
		for {
			_, data, err := connection.ReadMessage()
			require.NoError(t, err)
			require.NoError(t, json.Unmarshal(data, &message))
			if message["type"] == "result" {
				resultReceived <- true
				require.NoError(t, connection.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second)))
				return
			}
		}
	}))
	defer server.Close()
	request := compatibility.Request{Source: "claude", Protocol: compatibility.ProtocolClaudeSDK, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}, Output: compatibility.Output{Mode: compatibility.OutputStreamJSON}, Metadata: map[string]string{"sdk-url": "ws" + server.URL[4:]}}
	err := runClaudeSDK(t.Context(), protocolInvocation(executable, workingDir, bytes.NewReader(nil), io.Discard), request)
	require.NoError(t, err, "%T %#v", err, err)
	require.True(t, <-resultReceived)
}

func TestCopilotACPStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "copilot", Protocol: compatibility.ProtocolCopilotACP, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotACP(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(1), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": workingDir, "mcpServers": []any{}}}))
	require.NoError(t, decoder.Decode(&response))
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "session/update", response["method"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "usage_update", response["params"].(map[string]any)["update"].(map[string]any)["sessionUpdate"])
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(3), response["id"])
	require.Equal(t, "end_turn", response["result"].(map[string]any)["stopReason"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCopilotACPCancelsActivePrompt(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "copilot", Protocol: compatibility.ProtocolCopilotACP, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotACP(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session/new", "params": map[string]any{"cwd": workingDir, "mcpServers": []any{}}}))
	require.NoError(t, decoder.Decode(&response))
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session/prompt", "params": map[string]any{"sessionId": sessionID, "prompt": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "session/cancel", "params": map[string]any{"sessionId": sessionID}}))
	for response["id"] != float64(3) {
		response = nil
		require.NoError(t, decoder.Decode(&response))
	}
	require.Equal(t, "cancelled", response["result"].(map[string]any)["stopReason"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCopilotSDKStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "crux"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "copilot", Protocol: compatibility.ProtocolCopilotSDK, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCopilotSDK(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := &protocolOutput{writer: inputWriter, contentLength: true}
	reader := bufio.NewReader(outputReader)
	decode := func(response *map[string]any) {
		payload, err := readCopilotSDKFrame(reader)
		require.NoError(t, err)
		require.NoError(t, json.Unmarshal(payload, response))
	}
	var response map[string]any
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "connect", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, float64(3), response["result"].(map[string]any)["protocolVersion"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "status.get", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, float64(3), response["result"].(map[string]any)["protocolVersion"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 11, "method": "auth.getStatus", "params": map[string]any{}}))
	decode(&response)
	require.Equal(t, true, response["result"].(map[string]any)["isAuthenticated"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 12, "method": "models.list", "params": map[string]any{}}))
	decode(&response)
	models := response["result"].(map[string]any)["models"].([]any)
	require.NotEmpty(t, models)
	require.Contains(t, models[0].(map[string]any), "capabilities")
	requestedSessionID := "copilot-client-session"
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "session.create", "params": map[string]any{"sessionId": requestedSessionID, "workingDirectory": workingDir, "streaming": true}}))
	decode(&response)
	sessionID := response["result"].(map[string]any)["sessionId"].(string)
	require.Equal(t, requestedSessionID, sessionID)
	decode(&response)
	require.Equal(t, "session.start", response["params"].(map[string]any)["event"].(map[string]any)["type"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 13, "method": "session.model.getCurrent", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.Equal(t, "openai/gpt-5", response["result"].(map[string]any)["modelId"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 14, "method": "session.model.list", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["list"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 15, "method": "session.model.switchTo", "params": map[string]any{"sessionId": sessionID, "modelId": "anthropic/claude-sonnet"}}))
	decode(&response)
	require.Equal(t, "anthropic/claude-sonnet", response["result"].(map[string]any)["modelId"])
	require.Equal(t, false, response["result"].(map[string]any)["deferred"])
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "session.send", "params": map[string]any{"sessionId": sessionID, "prompt": "hello"}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["messageId"])
	idle := false
	for !idle {
		decode(&response)
		if response["method"] == "session.event" {
			idle = response["params"].(map[string]any)["event"].(map[string]any)["type"] == "session.idle"
		}
	}
	require.NoError(t, encoder.write(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "session.eventLog.read", "params": map[string]any{"sessionId": sessionID}}))
	decode(&response)
	require.NotEmpty(t, response["result"].(map[string]any)["events"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexUUIDv7IsStableForOpaqueNativeSessionIDs(t *testing.T) {
	sess := &proto.Session{ID: "48291", CreatedAt: 1_700_000_000}
	first := codexUUIDv7("/workspace", sess)
	second := codexUUIDv7("/workspace", sess)
	require.Equal(t, first, second)
	require.NotEqual(t, sess.ID, first)
	parsed, err := uuid.Parse(first)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsed.Version())
	require.NotEqual(t, first, codexUUIDv7("/other-workspace", sess))
}

func TestCodexAppServerInterruptsActiveTurn(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "unused"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	var response map[string]any
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": workingDir}}))
	require.NoError(t, decoder.Decode(&response))
	threadID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	parsedThreadID, err := uuid.Parse(threadID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedThreadID.Version())
	require.NotEqual(t, "thread-1", threadID)
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	require.NoError(t, decoder.Decode(&response))
	turnID := response["result"].(map[string]any)["turn"].(map[string]any)["id"].(string)
	parsedTurnID, err := uuid.Parse(turnID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedTurnID.Version())
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, decoder.Decode(&response))
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "turn/interrupt", "params": map[string]any{"threadId": threadID, "turnId": turnID}}))
	status := ""
	itemStatus := ""
	for status == "" {
		require.NoError(t, decoder.Decode(&response))
		if response["method"] == "item/completed" {
			itemStatus = response["params"].(map[string]any)["item"].(map[string]any)["status"].(string)
		}
		if response["method"] == "turn/completed" {
			status = response["params"].(map[string]any)["turn"].(map[string]any)["status"].(string)
		}
	}
	require.Equal(t, "interrupted", itemStatus)
	require.Equal(t, "interrupted", status)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}

func TestCodexAppServerStdioLifecycle(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable := "unused"
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt:  compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader},
		Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(t.Context(), protocolInvocation(executable, workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder := json.NewEncoder(inputWriter)
	decoder := json.NewDecoder(outputReader)
	require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
	var response map[string]any
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, float64(1), response["id"])
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}))
	require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "config/read", "params": map[string]any{"cwd": workingDir, "includeLayers": true}}))
	require.NoError(t, decoder.Decode(&response))
	configResult := response["result"].(map[string]any)
	require.Equal(t, "gpt-5", configResult["config"].(map[string]any)["model"])
	require.Equal(t, "openai", configResult["config"].(map[string]any)["model_provider"])
	require.NotNil(t, configResult["origins"])
	require.NotNil(t, configResult["layers"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 3, "method": "plugin/list", "params": map[string]any{"cwds": []string{workingDir}}}))
	require.NoError(t, decoder.Decode(&response))
	require.Empty(t, response["result"].(map[string]any)["marketplaces"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 4, "method": "model/list", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	models := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, models, 3)
	require.Equal(t, "ANTHROPIC-CLAUDE-SONNET", models[0].(map[string]any)["id"])
	require.Equal(t, "OPENAI-GPT-4-1", models[1].(map[string]any)["id"])
	require.Equal(t, "OPENAI-GPT-5", models[2].(map[string]any)["id"])
	require.NoError(t, encoder.Encode(map[string]any{"id": 5, "method": "thread/start", "params": map[string]any{"cwd": workingDir, "model": "claude-sonnet", "modelProvider": "anthropic", "approvalPolicy": "never", "approvalsReviewer": "user", "sandbox": "read-only", "experimentalRawEvents": true, "persistExtendedHistory": true}}))
	require.NoError(t, decoder.Decode(&response))
	startResult := response["result"].(map[string]any)
	require.Equal(t, "claude-sonnet", startResult["model"])
	require.Equal(t, "anthropic", startResult["modelProvider"])
	startThread := startResult["thread"].(map[string]any)
	require.Equal(t, "anthropic", startThread["modelProvider"])
	threadID := startThread["id"].(string)
	parsedThreadID, err := uuid.Parse(threadID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedThreadID.Version())
	require.NotEqual(t, "thread-1", threadID)
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, "thread/started", response["method"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 6, "method": "thread/read", "params": map[string]any{"threadId": threadID}}))
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, threadID, response["result"].(map[string]any)["thread"].(map[string]any)["id"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 7, "method": "thread/resume", "params": map[string]any{"threadId": threadID, "model": "claude-sonnet", "modelProvider": "anthropic"}}))
	require.NoError(t, decoder.Decode(&response))
	resumeResult := response["result"].(map[string]any)
	for _, field := range []string{"thread", "model", "modelProvider", "approvalPolicy", "approvalsReviewer", "cwd", "sandbox"} {
		require.Contains(t, resumeResult, field)
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 8, "method": "thread/fork", "params": map[string]any{"threadId": threadID}}))
	require.NoError(t, decoder.Decode(&response))
	forkID := response["result"].(map[string]any)["thread"].(map[string]any)["id"].(string)
	parsedForkID, err := uuid.Parse(forkID)
	require.NoError(t, err)
	require.Equal(t, uuid.Version(7), parsedForkID.Version())
	require.NotEqual(t, threadID, forkID)
	require.NotContains(t, forkID, "fork-thread-1")
	require.NoError(t, decoder.Decode(&response))
	require.Equal(t, forkID, response["params"].(map[string]any)["thread"].(map[string]any)["id"])

	require.NoError(t, encoder.Encode(map[string]any{"id": 9, "method": "thread/list", "params": map[string]any{}}))
	require.NoError(t, decoder.Decode(&response))
	listed := response["result"].(map[string]any)["data"].([]any)
	require.Len(t, listed, 2)
	for _, value := range listed {
		listedID := value.(map[string]any)["id"].(string)
		parsedListedID, parseErr := uuid.Parse(listedID)
		require.NoError(t, parseErr)
		require.Equal(t, uuid.Version(7), parsedListedID.Version())
		require.NotContains(t, listedID, "thread-1")
	}

	require.NoError(t, encoder.Encode(map[string]any{"id": 10, "method": "turn/start", "params": map[string]any{"threadId": threadID, "model": "ANTHROPIC-CLAUDE-SONNET", "input": []any{map[string]any{"type": "text", "text": "hello"}}}}))
	methods := make(map[string]bool)
	itemStatuses := make(map[string]string)
	for {
		require.NoError(t, decoder.Decode(&response))
		if method, ok := response["method"].(string); ok {
			methods[method] = true
			if method == "item/started" || method == "item/completed" {
				itemStatuses[method] = response["params"].(map[string]any)["item"].(map[string]any)["status"].(string)
			}
		}
		if response["method"] == "turn/completed" {
			break
		}
	}
	require.Equal(t, "inProgress", itemStatuses["item/started"])
	require.Equal(t, "completed", itemStatuses["item/completed"])
	require.True(t, methods["turn/started"])
	require.True(t, methods["item/started"])
	require.True(t, methods["item/agentMessage/delta"])
	require.True(t, methods["item/completed"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}
