package localaddon

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/proto"
	validator "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if path := os.Getenv("CRUX_TEST_CAPTURE_ARGS"); path != "" {
		if err := os.WriteFile(path, []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o600); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestCodexExecutionPoliciesMapWithoutApproximation(t *testing.T) {
	tests := []struct {
		name     string
		approval string
		sandbox  string
		want     proto.AgentPermissionMode
	}{
		{name: "interactive", approval: "on-request", sandbox: `"workspace-write"`, want: proto.AgentPermissionInteractive},
		{name: "deny read only", approval: "never", sandbox: `"read-only"`, want: proto.AgentPermissionDeny},
		{name: "bypass", approval: "never", sandbox: `"danger-full-access"`, want: proto.AgentPermissionBypass},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseCodexExecutionPolicy(test.approval, "user", json.RawMessage(test.sandbox), codexExecutionPolicy{})
			require.NoError(t, err)
			require.Equal(t, test.want, policy.permissionMode)
			request := compatibility.Request{Source: "codex", Permissions: compatibility.PermissionPolicy{
				Mode: compatibility.PermissionMode(policy.approvalPolicy), Sandbox: compatibility.SandboxMode(policy.sandbox), Bypass: policy.permissionMode == proto.AgentPermissionBypass,
			}}
			require.Equal(t, test.want, nativeAgentPermissionMode(request))
		})
	}

	_, err := parseCodexExecutionPolicy("on-request", "user", json.RawMessage(`"read-only"`), codexExecutionPolicy{})
	require.ErrorContains(t, err, "cannot be enforced")
	_, err = parseCodexExecutionPolicy("on-failure", "user", json.RawMessage(`"workspace-write"`), codexExecutionPolicy{})
	require.ErrorContains(t, err, "cannot be enforced")
	_, err = parseCodexExecutionPolicy("never", "external", json.RawMessage(`"read-only"`), codexExecutionPolicy{})
	require.ErrorContains(t, err, "reviewer")
}

func TestCodexTurnSandboxPoliciesParseWithoutApproximation(t *testing.T) {
	inherited := defaultCodexExecutionPolicy()
	for _, test := range []struct {
		name       string
		policy     string
		approval   string
		wantMode   proto.AgentPermissionMode
		wantErr    string
		wantPolicy codexExecutionPolicy
	}{
		{name: "omitted inherits", wantPolicy: inherited},
		{name: "null inherits", policy: `null`, wantPolicy: inherited},
		{name: "read only", policy: `{"type":"readOnly"}`, approval: "never", wantMode: proto.AgentPermissionDeny},
		{name: "workspace write", policy: `{"type":"workspaceWrite","writableRoots":[],"networkAccess":false,"excludeTmpdirEnvVar":false,"excludeSlashTmp":false}`, wantMode: proto.AgentPermissionInteractive},
		{name: "danger full access", policy: `{"type":"dangerFullAccess"}`, approval: "never", wantMode: proto.AgentPermissionBypass},
		{name: "not object", policy: `"readOnly"`, wantErr: "sandboxPolicy must be an object with a type"},
		{name: "missing type", policy: `{}`, wantErr: "sandboxPolicy.type is required"},
		{name: "external sandbox", policy: `{"type":"externalSandbox"}`, wantErr: `sandbox policy "externalSandbox" is not supported`},
		{name: "network access", policy: `{"type":"readOnly","networkAccess":true}`, wantErr: "sandboxPolicy.networkAccess is not supported"},
		{name: "writable roots", policy: `{"type":"workspaceWrite","writableRoots":["/tmp"]}`, wantErr: "sandboxPolicy.writableRoots is not supported"},
		{name: "unknown field", policy: `{"type":"readOnly","unknown":false}`, wantErr: "sandboxPolicy.unknown is not supported"},
		{name: "invalid boolean", policy: `{"type":"readOnly","networkAccess":"false"}`, wantErr: "sandboxPolicy.networkAccess must be a boolean"},
		{name: "invalid roots", policy: `{"type":"workspaceWrite","writableRoots":false}`, wantErr: "sandboxPolicy.writableRoots must be an array of paths"},
	} {
		t.Run(test.name, func(t *testing.T) {
			policy, err := parseCodexTurnSandboxPolicy(json.RawMessage(test.policy), inherited)
			if test.wantErr != "" {
				require.ErrorContains(t, err, test.wantErr)
				return
			}
			require.NoError(t, err)
			if test.policy == "" || test.policy == "null" {
				require.Equal(t, test.wantPolicy, policy)
				return
			}
			policy, err = parseCodexExecutionPolicy(test.approval, "", nil, policy)
			require.NoError(t, err)
			require.Equal(t, test.wantMode, policy.permissionMode)
		})
	}
}

func TestCodexAppServerReportsEnforcedPoliciesAndRejectsUnsupportedPolicy(t *testing.T) {
	for _, test := range []struct {
		name      string
		approval  string
		sandbox   string
		wantType  string
		wantError bool
		ephemeral bool
	}{
		{name: "deny", approval: "never", sandbox: "read-only", wantType: "readOnly"},
		{name: "bypass", approval: "never", sandbox: "danger-full-access", wantType: "dangerFullAccess"},
		{name: "unsupported", approval: "on-request", sandbox: "read-only", wantError: true},
		{name: "ephemeral", approval: "never", sandbox: "read-only", wantType: "readOnly", ephemeral: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			workingDir := codexNativeAPIFixture(t)
			var input bytes.Buffer
			encoder := json.NewEncoder(&input)
			require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "test", "version": "1"}}}))
			require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{
				"cwd": workingDir, "approvalPolicy": test.approval, "approvalsReviewer": "user", "sandbox": test.sandbox, "ephemeral": test.ephemeral,
			}}))
			var output bytes.Buffer
			stdin := io.NopCloser(&input)
			request := compatibility.Request{
				Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless,
				WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: stdin},
				Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
			}
			require.NoError(t, runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, stdin, &output), request))
			responses := decodeJSONLines(t, output.Bytes())
			require.GreaterOrEqual(t, len(responses), 2)
			if test.wantError {
				require.Contains(t, responses[1]["error"].(map[string]any)["message"], "cannot be enforced")
				return
			}
			result := responses[1]["result"].(map[string]any)
			require.Equal(t, test.approval, result["approvalPolicy"])
			require.Equal(t, test.wantType, result["sandbox"].(map[string]any)["type"])
			require.Equal(t, test.ephemeral, result["thread"].(map[string]any)["ephemeral"])
			if test.ephemeral {
				require.Empty(t, listFixtureSessions(t, workingDir))
			}
		})
	}
}

func TestCodexItemStatusesValidateAgainstGeneratedSchema(t *testing.T) {
	schemaData, err := json.Marshal(codexItemSchema())
	require.NoError(t, err)
	compiled, err := validator.NewCompiler().Compile(schemaData)
	require.NoError(t, err)
	for _, status := range []string{"inProgress", "completed", "failed", "interrupted"} {
		item, marshalErr := json.Marshal(map[string]any{"id": "item", "type": "agentMessage", "status": status, "text": "text"})
		require.NoError(t, marshalErr)
		result := compiled.ValidateJSON(item)
		require.True(t, result.IsValid(), "status %s: %+v", status, result.Errors)
	}
	userMessage, err := json.Marshal(map[string]any{"id": "user", "type": "userMessage", "clientId": nil, "content": []any{
		map[string]any{"type": "text", "text": "hello", "textElements": []any{"mention", map[string]any{"kind": "file"}}},
		map[string]any{"type": "skill", "name": "fixture", "path": "/skill"},
	}})
	require.NoError(t, err)
	require.True(t, compiled.ValidateJSON(userMessage).IsValid())
	compaction, err := json.Marshal(map[string]any{"id": "item", "type": "contextCompaction"})
	require.NoError(t, err)
	require.True(t, compiled.ValidateJSON(compaction).IsValid())
	compactionWithStatus, err := json.Marshal(map[string]any{"id": "item", "type": "contextCompaction", "status": "completed"})
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(compactionWithStatus).IsValid())
	missing, err := json.Marshal(map[string]any{"id": "item", "type": "agentMessage", "text": "text"})
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(missing).IsValid())
}

func TestCodexRuntimeNotificationsValidateAgainstGeneratedSchema(t *testing.T) {
	schemaData, err := json.Marshal(codexServerNotificationSchema())
	require.NoError(t, err)
	compiled, err := validator.NewCompiler().Compile(schemaData)
	require.NoError(t, err)

	thread := map[string]any{
		"id": "thread", "sessionId": "session", "forkedFromId": nil, "parentThreadId": nil,
		"name": nil, "model": nil, "modelProvider": "fixture", "status": map[string]any{"type": "idle"}, "turns": []any{},
	}
	turn := map[string]any{"id": "turn", "status": "inProgress", "items": []any{}, "error": nil}
	userItem := map[string]any{
		"id": "user", "type": "userMessage", "clientId": nil,
		"content": []any{
			map[string]any{"type": "text", "text": "hello", "textElements": []any{"mention", map[string]any{"kind": "file"}}},
			map[string]any{"type": "skill", "name": "fixture", "path": "/skill"},
		},
	}
	agentItem := map[string]any{"id": "agent", "type": "agentMessage", "status": "inProgress", "text": ""}
	for _, notification := range []map[string]any{
		{"method": "thread/started", "params": map[string]any{"thread": thread}},
		{"method": "thread/status/changed", "params": map[string]any{"threadId": "thread", "status": map[string]any{"type": "active", "activeFlags": []any{}}}},
		{"method": "thread/closed", "params": map[string]any{"threadId": "thread"}},
		{"method": "turn/started", "params": map[string]any{"threadId": "thread", "turn": turn}},
		{"method": "item/started", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": userItem}},
		{"method": "item/agentMessage/delta", "params": map[string]any{"threadId": "thread", "turnId": "turn", "itemId": "agent", "delta": "hello"}},
		{"method": "item/commandExecution/outputDelta", "params": map[string]any{"threadId": "thread", "turnId": "turn", "itemId": "command", "delta": "output"}},
		{"method": "item/completed", "params": map[string]any{"threadId": "thread", "turnId": "turn", "item": agentItem}},
		{"method": "turn/completed", "params": map[string]any{"threadId": "thread", "turn": turn}},
	} {
		data, marshalErr := json.Marshal(notification)
		require.NoError(t, marshalErr)
		result := compiled.ValidateJSON(data)
		require.True(t, result.IsValid(), "%s: %+v", notification["method"], result.Errors)
	}
}

func TestCodexAppServerAdapterPreservesGlobalEphemeralFlag(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{"--ephemeral", "app-server", "--stdio"}, WorkingDir: "/workspace", Stdin: bytes.NewReader(nil),
	})
	require.NoError(t, err)
	require.False(t, request.Session.Persistent)
}

func TestEphemeralNativeRunsLeaveNoSessionAfterSuccessOrFailure(t *testing.T) {
	for _, test := range []struct {
		name  string
		model string
	}{
		{name: "success"},
		{name: "failure", model: "missing-model"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workingDir := nativeAPIFixture(t, "response")
			request := compatibility.Request{
				Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
				Prompt: compatibility.Prompt{Text: "prompt"}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: false},
				Permissions: compatibility.PermissionPolicy{Bypass: true}, Output: compatibility.Output{Mode: compatibility.OutputJSON}, Model: test.model,
			}
			err := (nativeRuntime{}).Execute(t.Context(), protocolInvocation("unused", workingDir, bytes.NewReader(nil), io.Discard), request)
			if test.model == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
			require.Empty(t, listFixtureSessions(t, workingDir))
		})
	}
}

func TestClaudeSessionIDResolvesNativeSessionAndShapesUnknownIDError(t *testing.T) {
	workingDir := nativeAPIFixture(t, "response")
	seed := compatibility.Request{
		Source: "claude", Style: compatibility.ExecutionHeadless, WorkingDir: workingDir,
		Prompt: compatibility.Prompt{Text: "seed"}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
		Permissions: compatibility.PermissionPolicy{Bypass: true}, Output: compatibility.Output{Mode: compatibility.OutputJSON},
	}
	require.NoError(t, (nativeRuntime{}).Execute(t.Context(), protocolInvocation("unused", workingDir, bytes.NewReader(nil), io.Discard), seed))

	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{"--print", "--output-format", "json", "--session-id", "thread-1", "prompt"}, WorkingDir: workingDir,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "thread-1", request.Session.ID)
	var output bytes.Buffer
	require.NoError(t, (nativeRuntime{}).Execute(t.Context(), protocolInvocation("unused", workingDir, bytes.NewReader(nil), &output), request))
	var result map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, "thread-1", result["session_id"])

	unknownDir := nativeAPIFixture(t, "response")
	request.WorkingDir = unknownDir
	request.Session.ID = "native-session"
	output.Reset()
	err = (nativeRuntime{}).Execute(t.Context(), protocolInvocation("unused", unknownDir, bytes.NewReader(nil), &output), request)
	require.Error(t, err)
	require.NoError(t, json.Unmarshal(output.Bytes(), &result))
	require.Equal(t, "error_during_execution", result["subtype"])
	require.Contains(t, result["errors"].([]any)[0], "failed to get session")
}

func TestInteractiveCodexForkRunsForkedNativeSession(t *testing.T) {
	workingDir := codexNativeAPIFixture(t)
	executable, err := os.Executable()
	require.NoError(t, err)
	argsPath := filepath.Join(workingDir, "args")
	request := compatibility.Request{
		Source: "codex", Style: compatibility.ExecutionInteractive, WorkingDir: workingDir,
		Session: compatibility.Session{Mode: compatibility.SessionFork, ID: "thread-1", Persistent: true}, Prompt: compatibility.Prompt{Text: "continue"},
	}
	invocation := protocolInvocation(executable, workingDir, bytes.NewReader(nil), io.Discard)
	invocation.Env = append(invocation.Env, "CRUX_TEST_CAPTURE_ARGS="+argsPath)
	require.NoError(t, (nativeRuntime{}).Execute(t.Context(), invocation, request))
	arguments, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	require.Contains(t, string(arguments), "--session\nfork-thread-1\n")
	require.NotContains(t, string(arguments), "--continue")
}

func TestCodexSchemaGenerationIsByteDeterministic(t *testing.T) {
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	require.NoError(t, writeCodexJSONSchemas(first))
	require.NoError(t, writeCodexJSONSchemas(second))
	firstFiles := readGeneratedTree(t, first)
	secondFiles := readGeneratedTree(t, second)
	require.Equal(t, firstFiles, secondFiles)

	var clientSchema map[string]any
	require.NoError(t, json.Unmarshal(firstFiles["ClientRequest.json"], &clientSchema))
	oneOf := clientSchema["oneOf"].([]any)
	methods := make([]string, 0, len(oneOf))
	for _, raw := range oneOf {
		method := raw.(map[string]any)["properties"].(map[string]any)["method"].(map[string]any)["enum"].([]any)[0].(string)
		methods = append(methods, method)
	}
	require.IsIncreasing(t, methods)
}

func TestCodexGeneratedMethodsMatchSupportedManifest(t *testing.T) {
	generated := t.TempDir()
	require.NoError(t, writeCodexJSONSchemas(generated))

	var clientSchema map[string]any
	clientData, err := os.ReadFile(filepath.Join(generated, "ClientRequest.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(clientData, &clientSchema))
	requestMethods := make([]string, 0)
	requestMethodSet := make(map[string]struct{})
	for _, raw := range clientSchema["oneOf"].([]any) {
		method := raw.(map[string]any)["properties"].(map[string]any)["method"].(map[string]any)["enum"].([]any)[0].(string)
		requestMethods = append(requestMethods, method)
		requestMethodSet[method] = struct{}{}
	}
	require.Len(t, requestMethodSet, len(requestMethods))

	supportedRequests := make([]string, 0)
	for _, method := range codexSupportedMethods() {
		if method != "initialized" {
			supportedRequests = append(supportedRequests, method)
		}
	}
	require.ElementsMatch(t, supportedRequests, requestMethods)

	var manifest struct {
		Methods []string `json:"methods"`
	}
	manifestData, err := os.ReadFile(filepath.Join(generated, "crux_compatibility.json"))
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(manifestData, &manifest))
	manifestMethodSet := make(map[string]struct{}, len(manifest.Methods))
	for _, method := range manifest.Methods {
		manifestMethodSet[method] = struct{}{}
	}
	require.Len(t, manifestMethodSet, len(manifest.Methods))
	require.ElementsMatch(t, codexSupportedMethods(), manifest.Methods)
}

func listFixtureSessions(t *testing.T, workingDir string) []proto.Session {
	t.Helper()
	client, err := codexClientFactory(workingDir)
	require.NoError(t, err)
	workspace, err := client.CreateWorkspace(t.Context(), proto.Workspace{Path: workingDir})
	require.NoError(t, err)
	sessions, err := client.ListSessions(t.Context(), workspace.ID)
	require.NoError(t, err)
	return sessions
}

func readGeneratedTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	result := make(map[string][]byte)
	require.NoError(t, filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		result[filepath.ToSlash(relative)], err = os.ReadFile(path)
		return err
	}))
	return result
}
