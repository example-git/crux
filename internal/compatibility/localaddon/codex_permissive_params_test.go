package localaddon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	validator "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestCodexAppServerIgnoresUnusedReadParams(t *testing.T) {
	requirements := map[string]any{"allowedApprovalPolicies": []any{"never"}, "allowedSandboxModes": []any{"read-only"}}
	var nativeCalls atomic.Int64
	workingDir := nativeAPIFixtureWithSessionAndTaskSeedsAndFileSearch(t, proto.RunComplete{}, 25*time.Millisecond, true, nil, nil, nil, nil, nil, func(w http.ResponseWriter, r *http.Request) {
		nativeCalls.Add(1)
		require.Equal(t, http.MethodGet, r.Method)
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(proto.ConfigRequirements{Requirements: requirements}))
	}, nil, nil)
	schemaDirectory := t.TempDir()
	require.NoError(t, writeCodexJSONSchemas(schemaDirectory))
	schemaBytes, err := os.ReadFile(filepath.Join(schemaDirectory, "ClientRequest.json"))
	require.NoError(t, err)
	schema, err := validator.NewCompiler().Compile(schemaBytes)
	require.NoError(t, err)
	params := []string{"", "null", "{}", "{ \t }", `{"refreshToken":true,"future":{"value":1}}`, `[]`, `"ignored"`, `true`, `42`}
	var input strings.Builder
	input.WriteString("{\"id\":\"initialize\",\"method\":\"initialize\",\"params\":{\"clientInfo\":{\"name\":\"desktop\",\"version\":\"1\"}}}\n{\"method\":\"initialized\"}\n")
	for _, method := range []string{"configRequirements/read", "account/read"} {
		for index, value := range params {
			line := fmt.Sprintf(`{"id":%q,"method":%q`, fmt.Sprintf("%s-%d", method, index), method)
			if value != "" {
				line += `,"params":` + value
			}
			line += "}"
			validation := schema.ValidateJSON([]byte(line))
			require.True(t, validation.IsValid(), "%s: %+v", line, validation.Errors)
			input.WriteString(line + "\n")
		}
	}
	for index, value := range []any{false, true, nil, "ignored", 42, []any{"ignored"}, map[string]any{"future": true}} {
		line, marshalErr := json.Marshal(map[string]any{"id": fmt.Sprintf("config/read-%d", index), "method": "config/read", "params": map[string]any{"cwd": workingDir, "includeLayers": value, "future": value}})
		require.NoError(t, marshalErr)
		validation := schema.ValidateJSON(line)
		require.True(t, validation.IsValid(), "%s: %+v", line, validation.Errors)
		input.Write(line)
		input.WriteByte('\n')
	}
	reader := io.NopCloser(strings.NewReader(input.String()))
	var output bytes.Buffer
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: reader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	require.NoError(t, runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, reader, &output), request))
	responses := decodeJSONLines(t, output.Bytes())
	require.Len(t, responses, 1+2*len(params)+7)
	var configResult any
	for _, response := range responses {
		require.NotContains(t, response, "error", "%+v", response)
		id := response["id"].(string)
		switch {
		case strings.HasPrefix(id, "configRequirements/read-"):
			require.Equal(t, map[string]any{"requirements": requirements}, response["result"])
		case strings.HasPrefix(id, "account/read-"):
			require.Equal(t, codexAccountReadResult(), response["result"])
		case strings.HasPrefix(id, "config/read-"):
			if configResult == nil {
				configResult = response["result"]
			}
			require.Equal(t, configResult, response["result"])
			result := response["result"].(map[string]any)
			require.Equal(t, "OPENAI-GPT-5", result["config"].(map[string]any)["model"])
			require.Equal(t, "crux", result["config"].(map[string]any)["model_provider"])
			require.NotNil(t, result["origins"])
			require.NotContains(t, result, "layers")
		}
	}
	require.Equal(t, int64(len(params)), nativeCalls.Load())
}

func TestCodexAppServerIgnoresUnusedThreadAndTurnParams(t *testing.T) {
	type observedRun struct {
		provider string
		model    string
		message  proto.AgentMessage
	}
	runs := make(chan observedRun, 1)
	workingDir := nativeAPIFixture(t, "native extra-parameter response", func(_ string, provider, model string, message proto.AgentMessage) {
		runs <- observedRun{provider: provider, model: model, message: message}
	})
	schemaDirectory := t.TempDir()
	require.NoError(t, writeCodexJSONSchemas(schemaDirectory))
	schemaBytes, err := os.ReadFile(filepath.Join(schemaDirectory, "ClientRequest.json"))
	require.NoError(t, err)
	schema, err := validator.NewCompiler().Compile(schemaBytes)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	request := compatibility.Request{
		Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
	}
	done := make(chan error, 1)
	go func() {
		done <- runCodexAppServer(ctx, protocolInvocation("unused", workingDir, inputReader, outputWriter), request)
		_ = outputWriter.Close()
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = inputWriter.Close()
		_ = outputReader.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("app-server did not stop")
		}
	})
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	call := func(method string, params map[string]any) map[string]any {
		t.Helper()
		message := map[string]any{"id": method, "method": method, "params": params}
		encoded, marshalErr := json.Marshal(message)
		require.NoError(t, marshalErr)
		validation := schema.ValidateJSON(encoded)
		require.True(t, validation.IsValid(), "%s: %+v", encoded, validation.Errors)
		require.NoError(t, encoder.Encode(message))
		for {
			var response map[string]any
			require.NoError(t, decoder.Decode(&response))
			if response["id"] == method {
				require.NotContains(t, response, "error", "%+v", response)
				return response["result"].(map[string]any)
			}
		}
	}
	withUnused := func(params map[string]any, fields string) map[string]any {
		values := []any{true, "ignored", 42, []any{"ignored"}, map[string]any{"future": true}}
		for index, field := range strings.Fields(fields) {
			params[field] = values[index%len(values)]
		}
		params["futureParameter"] = map[string]any{"nested": "ignored"}
		return params
	}
	call("initialize", map[string]any{"clientInfo": map[string]any{"name": "desktop", "version": "1"}})
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized"}))
	started := call("thread/start", withUnused(map[string]any{"cwd": workingDir, "model": "gpt-5", "modelProvider": "crux", "approvalPolicy": "never", "sandbox": "read-only"}, "allowProviderModelFallback serviceTier runtimeWorkspaceRoots permissions config serviceName baseInstructions developerInstructions personality multiAgentMode historyMode sessionStartSource threadSource environments dynamicTools selectedCapabilityRoots experimentalRawEvents persistExtendedHistory"))
	threadID := started["thread"].(map[string]any)["id"].(string)
	require.NotEmpty(t, threadID)
	require.Equal(t, "OPENAI-GPT-5", started["model"])
	require.Equal(t, "never", started["approvalPolicy"])
	listed := call("thread/list", withUnused(map[string]any{}, "sourceKinds isPinned sectionId useStateDbOnly parentThreadId ancestorThreadId"))
	require.Len(t, listed["data"], 1)
	require.Equal(t, threadID, listed["data"].([]any)[0].(map[string]any)["id"])
	resumed := call("thread/resume", withUnused(map[string]any{"threadId": threadID, "model": "gpt-4.1", "modelProvider": "crux"}, "approvalPolicy approvalsReviewer baseInstructions config developerInstructions history initialTurnsPage path permissions personality runtimeWorkspaceRoots sandbox serviceTier"))
	require.Equal(t, threadID, resumed["thread"].(map[string]any)["id"])
	require.Equal(t, "OPENAI-GPT-4-1", resumed["model"])
	require.Equal(t, "never", resumed["approvalPolicy"])
	turn := call("turn/start", withUnused(map[string]any{"threadId": threadID, "input": []any{map[string]any{"type": "text", "text": "preserve this native prompt"}}}, "cwd summary personality outputSchema sandbox toolOutput collaborationMode modelProvider"))
	require.NotEmpty(t, turn["turn"].(map[string]any)["id"])
	var text strings.Builder
	for {
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		require.NotContains(t, response, "error", "%+v", response)
		if response["method"] == "item/agentMessage/delta" {
			text.WriteString(response["params"].(map[string]any)["delta"].(string))
		}
		if response["method"] == "turn/completed" {
			require.Equal(t, "completed", response["params"].(map[string]any)["turn"].(map[string]any)["status"])
			break
		}
	}
	require.Equal(t, "native extra-parameter response", text.String())
	select {
	case run := <-runs:
		require.Equal(t, "openai", run.provider)
		require.Equal(t, "gpt-4.1", run.model)
		require.Empty(t, run.message.Prompt)
		require.Equal(t, []proto.AgentInputPart{{Type: "text", Text: "preserve this native prompt"}}, run.message.InputParts)
		require.Equal(t, proto.AgentPermissionDeny, run.message.PermissionMode)
	default:
		t.Fatal("turn did not reach native execution")
	}
	forked := call("thread/fork", withUnused(map[string]any{"threadId": threadID}, "approvalPolicy approvalsReviewer baseInstructions beforeTurnId config deferGoalContinuation developerInstructions ephemeral excludeTurns lastTurnId model modelProvider path permissions runtimeWorkspaceRoots sandbox serviceTier threadSource"))
	fork := forked["thread"].(map[string]any)
	require.NotEqual(t, threadID, fork["id"])
	require.Equal(t, threadID, fork["forkedFromId"])
	require.NotEmpty(t, fork["turns"])
	require.Equal(t, "OPENAI-GPT-4-1", forked["model"])
	require.Equal(t, "never", forked["approvalPolicy"])
	var notification map[string]any
	require.NoError(t, decoder.Decode(&notification))
	require.Equal(t, "thread/started", notification["method"])
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
}
