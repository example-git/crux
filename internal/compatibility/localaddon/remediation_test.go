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
			require.NoError(t, encoder.Encode(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{}}))
			require.NoError(t, encoder.Encode(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{
				"cwd": workingDir, "approvalPolicy": test.approval, "approvalsReviewer": "user", "sandbox": test.sandbox, "ephemeral": test.ephemeral,
			}}))
			var output bytes.Buffer
			request := compatibility.Request{
				Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless,
				WorkingDir: workingDir, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: &input},
				Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true},
			}
			require.NoError(t, runCodexAppServer(t.Context(), protocolInvocation("unused", workingDir, &input, &output), request))
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
	missing, err := json.Marshal(map[string]any{"id": "item", "type": "agentMessage", "text": "text"})
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(missing).IsValid())
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
