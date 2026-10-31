package localaddon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

func TestCodexAdapterTranslatesExecInvocation(t *testing.T) {
	adapter := codexAdapter{}
	request, err := adapter.Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--model", "gpt-test", "--cd", "sub", "--sandbox", "workspace-write", "--json", "review this"},
		WorkingDir: "/workspace",
		Stdin:      bytes.NewBufferString("extra context"),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "gpt-test", request.Model)
	require.Equal(t, filepath.Join("/workspace", "sub"), request.WorkingDir)
	require.Equal(t, compatibility.SandboxMode("workspace-write"), request.Permissions.Sandbox)
	require.Equal(t, compatibility.OutputJSONLines, request.Output.Mode)
	require.Equal(t, compatibility.PromptCombined, request.Prompt.Source)
	require.Contains(t, request.Prompt.Text, "review this")
	require.Contains(t, request.Prompt.Text, "extra context")
}

func TestCodexAdapterAcceptsGlobalOptionsBeforeExec(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--model", "gpt-test", "exec", "--dangerously-bypass-approvals-and-sandbox", "prompt"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "gpt-test", request.Model)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.True(t, request.Permissions.Bypass)
}

func TestCodexAdapterEnablesAppServerStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"id\":1,\"method\":\"initialize\"}\n")
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"app-server", "--listen", "stdio://"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCodexAdapterGeneratesAppServerSchemas(t *testing.T) {
	workingDir := t.TempDir()
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "generate-json-schema", "--out", "schemas", "--experimental"}, WorkingDir: workingDir})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexSchema, request.Protocol)
	require.NoError(t, runCodexSchemaGenerator(request))
	data, err := os.ReadFile(filepath.Join(workingDir, "schemas", "codex_app_server_protocol.v2.schemas.json"))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(data))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchSessionStartResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchSessionUpdateResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchSessionStopResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchSessionUpdatedNotification.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "FuzzyFileSearchSessionCompletedNotification.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadStartResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadTurnsListResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadLoadedListResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadBackgroundTerminalsCleanResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadBackgroundTerminalsListResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadBackgroundTerminalsTerminateResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadArchiveResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadUnarchiveResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadDeleteResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadRollbackResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadSetNameResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadNameUpdatedNotification.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "DeprecationNoticeNotification.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadCompactStartResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadShellCommandResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "CommandExecutionOutputDeltaNotification.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ThreadUnsubscribeResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ConfigReadResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "ConfigRequirementsReadResponse.json"))
	require.FileExists(t, filepath.Join(workingDir, "schemas", "v2", "SkillsListResponse.json"))
	require.NoFileExists(t, filepath.Join(workingDir, "schemas", "v2", "PluginListResponse.json"))
	clientSchema, err := os.ReadFile(filepath.Join(workingDir, "schemas", "ClientRequest.json"))
	require.NoError(t, err)
	serverSchema, err := os.ReadFile(filepath.Join(workingDir, "schemas", "ServerNotification.json"))
	require.NoError(t, err)
	require.Contains(t, string(serverSchema), `"fuzzyFileSearch/sessionUpdated"`)
	require.Contains(t, string(serverSchema), `"fuzzyFileSearch/sessionCompleted"`)
	require.Contains(t, string(serverSchema), `"thread/status/changed"`)
	require.Contains(t, string(serverSchema), `"thread/archived"`)
	require.Contains(t, string(serverSchema), `"thread/unarchived"`)
	require.Contains(t, string(serverSchema), `"thread/deleted"`)
	require.Contains(t, string(serverSchema), `"thread/name/updated"`)
	require.Contains(t, string(serverSchema), `"deprecationNotice"`)
	require.Contains(t, string(serverSchema), `"thread/closed"`)
	require.Contains(t, string(serverSchema), `"item/commandExecution/outputDelta"`)
	require.Contains(t, string(clientSchema), `"config/read"`)
	require.Contains(t, string(clientSchema), `"configRequirements/read"`)
	require.Contains(t, string(clientSchema), `"skills/list"`)
	require.Contains(t, string(clientSchema), `"fuzzyFileSearch"`)
	require.Contains(t, string(clientSchema), `"fuzzyFileSearch/sessionStart"`)
	require.Contains(t, string(clientSchema), `"fuzzyFileSearch/sessionUpdate"`)
	require.Contains(t, string(clientSchema), `"fuzzyFileSearch/sessionStop"`)
	require.Contains(t, string(clientSchema), `"thread/rollback"`)
	require.Contains(t, string(clientSchema), `"thread/name/set"`)
	require.NotContains(t, string(clientSchema), `"plugin/list"`)
	require.NotContains(t, string(clientSchema), `"thread/goal/set"`)
	require.NotContains(t, string(clientSchema), `"thread/goal/get"`)
	require.NotContains(t, string(clientSchema), `"thread/goal/clear"`)
	require.Contains(t, string(clientSchema), `"turn/steer"`)
	require.Contains(t, string(clientSchema), `"modelProvider"`)
	var schema map[string]any
	require.NoError(t, json.Unmarshal(clientSchema, &schema))
	methodSchema := func(method string) map[string]any {
		t.Helper()
		for _, candidate := range schema["oneOf"].([]any) {
			value := candidate.(map[string]any)
			properties := value["properties"].(map[string]any)
			methodValues := properties["method"].(map[string]any)["enum"].([]any)
			if methodValues[0] == method {
				return value
			}
		}
		t.Fatalf("missing schema for %s", method)
		return nil
	}
	initializeParams := methodSchema("initialize")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, initializeParams["required"], "clientInfo")
	accountParams := methodSchema("account/read")["properties"].(map[string]any)["params"].(map[string]any)
	require.Empty(t, accountParams)
	configRequirementsParams := methodSchema("configRequirements/read")["properties"].(map[string]any)["params"].(map[string]any)
	require.Empty(t, configRequirementsParams)
	configRequirementsData, err := os.ReadFile(filepath.Join(workingDir, "schemas", "v2", "ConfigRequirementsReadResponse.json"))
	require.NoError(t, err)
	var configRequirementsSchema map[string]any
	require.NoError(t, json.Unmarshal(configRequirementsData, &configRequirementsSchema))
	require.Equal(t, []any{"requirements"}, configRequirementsSchema["required"])
	require.Equal(t, "null", configRequirementsSchema["properties"].(map[string]any)["requirements"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	skillsParams := methodSchema("skills/list")["properties"].(map[string]any)["params"].(map[string]any)
	skillsParamProperties := skillsParams["properties"].(map[string]any)
	require.Equal(t, "array", skillsParamProperties["cwds"].(map[string]any)["type"])
	require.Equal(t, "string", skillsParamProperties["cwds"].(map[string]any)["items"].(map[string]any)["type"])
	require.Equal(t, "boolean", skillsParamProperties["forceReload"].(map[string]any)["type"])
	skillsData, err := os.ReadFile(filepath.Join(workingDir, "schemas", "v2", "SkillsListResponse.json"))
	require.NoError(t, err)
	var skillsSchema map[string]any
	require.NoError(t, json.Unmarshal(skillsData, &skillsSchema))
	skillsEntry := skillsSchema["properties"].(map[string]any)["data"].(map[string]any)["items"].(map[string]any)
	require.ElementsMatch(t, []any{"cwd", "skills", "errors"}, skillsEntry["required"])
	skillMetadata := skillsEntry["properties"].(map[string]any)["skills"].(map[string]any)["items"].(map[string]any)
	require.ElementsMatch(t, []any{"name", "description", "path", "scope", "enabled"}, skillMetadata["required"])
	skillProperties := skillMetadata["properties"].(map[string]any)
	require.Equal(t, []any{"user", "repo", "system", "admin"}, skillProperties["scope"].(map[string]any)["enum"])
	require.Equal(t, "null", skillProperties["pluginId"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	fuzzyParams := methodSchema("fuzzyFileSearch")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"query", "roots"}, fuzzyParams["required"])
	require.Equal(t, "string", fuzzyParams["properties"].(map[string]any)["cancellationToken"].(map[string]any)["type"])
	fuzzyStartParams := methodSchema("fuzzyFileSearch/sessionStart")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"sessionId", "roots"}, fuzzyStartParams["required"])
	fuzzyUpdateParams := methodSchema("fuzzyFileSearch/sessionUpdate")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"sessionId", "query"}, fuzzyUpdateParams["required"])
	fuzzyStopParams := methodSchema("fuzzyFileSearch/sessionStop")["properties"].(map[string]any)["params"].(map[string]any)
	require.Equal(t, []any{"sessionId"}, fuzzyStopParams["required"])
	readParams := methodSchema("thread/read")["properties"].(map[string]any)["params"].(map[string]any)
	require.Equal(t, "boolean", readParams["properties"].(map[string]any)["includeTurns"].(map[string]any)["type"])
	threadListParams := methodSchema("thread/list")["properties"].(map[string]any)["params"].(map[string]any)
	threadListProperties := threadListParams["properties"].(map[string]any)
	require.Equal(t, float64(0), threadListProperties["limit"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["minimum"])
	require.Equal(t, []any{"created_at", "updated_at"}, threadListProperties["sortKey"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["enum"])
	require.Equal(t, "boolean", threadListProperties["archived"].(map[string]any)["type"])
	archiveParams := methodSchema("thread/archive")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, archiveParams["required"], "threadId")
	unarchiveParams := methodSchema("thread/unarchive")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, unarchiveParams["required"], "threadId")
	deleteParams := methodSchema("thread/delete")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, deleteParams["required"], "threadId")
	rollbackParams := methodSchema("thread/rollback")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"threadId", "numTurns"}, rollbackParams["required"])
	rollbackNumTurns := rollbackParams["properties"].(map[string]any)["numTurns"].(map[string]any)
	require.Equal(t, "integer", rollbackNumTurns["type"])
	require.Equal(t, float64(0), rollbackNumTurns["minimum"])
	setNameParams := methodSchema("thread/name/set")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"threadId", "name"}, setNameParams["required"])
	setNameProperties := setNameParams["properties"].(map[string]any)
	require.Equal(t, "string", setNameProperties["threadId"].(map[string]any)["type"])
	require.Equal(t, "string", setNameProperties["name"].(map[string]any)["type"])
	nameUpdatedData, err := os.ReadFile(filepath.Join(workingDir, "schemas", "v2", "ThreadNameUpdatedNotification.json"))
	require.NoError(t, err)
	var nameUpdatedSchema map[string]any
	require.NoError(t, json.Unmarshal(nameUpdatedData, &nameUpdatedSchema))
	require.Equal(t, []any{"threadId"}, nameUpdatedSchema["required"])
	nameUpdatedProperties := nameUpdatedSchema["properties"].(map[string]any)
	require.Equal(t, "string", nameUpdatedProperties["threadId"].(map[string]any)["type"])
	nameUpdatedThreadName := nameUpdatedProperties["threadName"].(map[string]any)["anyOf"].([]any)
	require.Equal(t, "string", nameUpdatedThreadName[0].(map[string]any)["type"])
	require.Equal(t, "null", nameUpdatedThreadName[1].(map[string]any)["type"])
	var notificationSchema map[string]any
	require.NoError(t, json.Unmarshal(serverSchema, &notificationSchema))
	var aggregateNameUpdated map[string]any
	for _, candidate := range notificationSchema["oneOf"].([]any) {
		value := candidate.(map[string]any)
		properties := value["properties"].(map[string]any)
		methodValues := properties["method"].(map[string]any)["enum"].([]any)
		if methodValues[0] == "thread/name/updated" {
			aggregateNameUpdated = properties["params"].(map[string]any)
			break
		}
	}
	require.NotNil(t, aggregateNameUpdated)
	require.Equal(t, []any{"threadId"}, aggregateNameUpdated["required"])
	aggregateThreadName := aggregateNameUpdated["properties"].(map[string]any)["threadName"].(map[string]any)["anyOf"].([]any)
	require.Equal(t, "string", aggregateThreadName[0].(map[string]any)["type"])
	require.Equal(t, "null", aggregateThreadName[1].(map[string]any)["type"])
	compactParams := methodSchema("thread/compact/start")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, compactParams["required"], "threadId")
	shellCommandParams := methodSchema("thread/shellCommand")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"threadId", "command"}, shellCommandParams["required"])
	timeoutSchema := shellCommandParams["properties"].(map[string]any)["timeoutMs"].(map[string]any)
	require.Equal(t, "integer", timeoutSchema["anyOf"].([]any)[0].(map[string]any)["type"])
	require.Equal(t, "null", timeoutSchema["anyOf"].([]any)[1].(map[string]any)["type"])
	loadedListParams := methodSchema("thread/loaded/list")["properties"].(map[string]any)["params"].(map[string]any)
	loadedListProperties := loadedListParams["properties"].(map[string]any)
	require.Equal(t, float64(0), loadedListProperties["limit"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["minimum"])
	require.Contains(t, loadedListProperties, "cursor")
	backgroundCleanParams := methodSchema("thread/backgroundTerminals/clean")["properties"].(map[string]any)["params"].(map[string]any)
	require.Equal(t, []any{"threadId"}, backgroundCleanParams["required"])
	backgroundListParams := methodSchema("thread/backgroundTerminals/list")["properties"].(map[string]any)["params"].(map[string]any)
	require.Equal(t, []any{"threadId"}, backgroundListParams["required"])
	backgroundListProperties := backgroundListParams["properties"].(map[string]any)
	require.Equal(t, float64(0), backgroundListProperties["limit"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["minimum"])
	require.Contains(t, backgroundListProperties, "cursor")
	backgroundTerminateParams := methodSchema("thread/backgroundTerminals/terminate")["properties"].(map[string]any)["params"].(map[string]any)
	require.ElementsMatch(t, []any{"threadId", "processId"}, backgroundTerminateParams["required"])
	backgroundListSchemaData, err := os.ReadFile(filepath.Join(workingDir, "schemas", "v2", "ThreadBackgroundTerminalsListResponse.json"))
	require.NoError(t, err)
	var backgroundListSchema map[string]any
	require.NoError(t, json.Unmarshal(backgroundListSchemaData, &backgroundListSchema))
	terminalSchema := backgroundListSchema["properties"].(map[string]any)["data"].(map[string]any)["items"].(map[string]any)
	require.ElementsMatch(t, []any{"itemId", "processId", "command", "cwd", "osPid", "cpuPercent", "rssKb"}, terminalSchema["required"])
	terminalProperties := terminalSchema["properties"].(map[string]any)
	require.Equal(t, "null", terminalProperties["osPid"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	require.Equal(t, "null", terminalProperties["cpuPercent"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	require.Equal(t, "null", terminalProperties["rssKb"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	unsubscribeParams := methodSchema("thread/unsubscribe")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, unsubscribeParams["required"], "threadId")
	turnsListParams := methodSchema("thread/turns/list")["properties"].(map[string]any)["params"].(map[string]any)
	require.Contains(t, turnsListParams["required"], "threadId")
	turnsListProperties := turnsListParams["properties"].(map[string]any)
	require.Equal(t, float64(0), turnsListProperties["limit"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["minimum"])
	require.Equal(t, []any{"notLoaded", "summary", "full"}, turnsListProperties["itemsView"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["enum"])
	turnStartParams := methodSchema("turn/start")["properties"].(map[string]any)["params"].(map[string]any)
	turnStartProperties := turnStartParams["properties"].(map[string]any)
	require.Contains(t, turnStartProperties, "sandboxPolicy")
	require.Contains(t, turnStartProperties, "effort")
	require.NotContains(t, turnStartProperties, "sandbox")
	require.Equal(t, "null", turnStartProperties["sandboxPolicy"].(map[string]any)["anyOf"].([]any)[1].(map[string]any)["type"])
	require.Equal(t, []any{"none", "low", "medium", "high", "xhigh", "max"}, turnStartProperties["effort"].(map[string]any)["anyOf"].([]any)[0].(map[string]any)["enum"])
	turnInputSchema := turnStartProperties["input"].(map[string]any)["items"].(map[string]any)
	turnInputVariants := turnInputSchema["oneOf"].([]any)
	require.Len(t, turnInputVariants, 3)
	textInput := turnInputVariants[0].(map[string]any)
	localImageInput := turnInputVariants[1].(map[string]any)
	skillInput := turnInputVariants[2].(map[string]any)
	require.Equal(t, []any{"text"}, textInput["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	require.ElementsMatch(t, []any{"type", "text"}, textInput["required"])
	require.Contains(t, textInput["properties"], "textElements")
	require.Equal(t, false, textInput["additionalProperties"])
	require.Equal(t, []any{"localImage"}, localImageInput["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	require.ElementsMatch(t, []any{"type", "path"}, localImageInput["required"])
	require.Equal(t, false, localImageInput["additionalProperties"])
	require.Equal(t, []any{"skill"}, skillInput["properties"].(map[string]any)["type"].(map[string]any)["enum"])
	require.ElementsMatch(t, []any{"type", "name", "path"}, skillInput["required"])
	require.Equal(t, false, skillInput["additionalProperties"])
	turnSteerParams := methodSchema("turn/steer")["properties"].(map[string]any)["params"].(map[string]any)
	require.Equal(t, turnInputSchema, turnSteerParams["properties"].(map[string]any)["input"].(map[string]any)["items"])

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "generate-ts", "--out", "types"}, WorkingDir: workingDir})
	require.NoError(t, err)
	require.NoError(t, runCodexSchemaGenerator(request))
	typeScript, err := os.ReadFile(filepath.Join(workingDir, "types", "ClientRequest.ts"))
	require.NoError(t, err)
	require.Contains(t, string(typeScript), `{ type: "skill"; name: string; path: string }`)
	serverTypeScript, err := os.ReadFile(filepath.Join(workingDir, "types", "ServerNotification.ts"))
	require.NoError(t, err)
	require.Contains(t, string(serverTypeScript), `{ type: "skill"; name: string; path: string }`)
	require.Contains(t, string(serverTypeScript), `method: "fuzzyFileSearch/sessionUpdated"; params: { sessionId: string; query: string; files: FuzzyFile[] }`)
	require.Contains(t, string(serverTypeScript), `method: "fuzzyFileSearch/sessionCompleted"; params: { sessionId: string }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/status/changed"; params: { threadId: string; status: ThreadStatus }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/archived"; params: { threadId: string }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/unarchived"; params: { threadId: string }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/deleted"; params: { threadId: string }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/name/updated"; params: { threadId: string; threadName?: string | null }`)
	require.Contains(t, string(serverTypeScript), `method: "deprecationNotice"; params: { summary: string; details?: string | null }`)
	require.Contains(t, string(serverTypeScript), `method: "thread/closed"; params: { threadId: string }`)
	require.Contains(t, string(typeScript), `method: "initialize"; params: InitializeParams`)
	require.Contains(t, string(typeScript), `method: "fuzzyFileSearch"; params: FuzzyFileSearchParams`)
	require.Contains(t, string(typeScript), `method: "fuzzyFileSearch/sessionStart"; params: FuzzyFileSearchSessionStartParams`)
	require.Contains(t, string(typeScript), `export type FuzzyFile = { root: string; path: string; match_type: "file" | "directory"; file_name: string; score: number; indices?: number[] };`)
	require.Contains(t, string(typeScript), `method: "account/read"; params?: unknown`)
	require.Contains(t, string(typeScript), `method: "configRequirements/read"; params?: unknown`)
	require.Contains(t, string(typeScript), `ConfigRequirementsReadResponse = { requirements: Record<string, unknown> | null }`)
	require.Contains(t, string(typeScript), `method: "skills/list"; params?: SkillsListParams`)
	require.Contains(t, string(typeScript), `SkillsListParams = { cwds?: string[]; forceReload?: boolean }`)
	require.Contains(t, string(typeScript), `SkillScope = "user" | "repo" | "system" | "admin"`)
	require.Contains(t, string(typeScript), `SkillMetadata = { name: string; description: string;`)
	require.Contains(t, string(typeScript), `pluginId: string | null }`)
	require.Contains(t, string(typeScript), `SkillsListResponse = { data: SkillsListEntry[] }`)
	require.Contains(t, string(typeScript), `method: "thread/rollback"; params: ThreadRollbackParams`)
	require.Contains(t, string(typeScript), `export type ThreadRollbackParams = { threadId: string; numTurns: number };`)
	require.Contains(t, string(typeScript), `export type ThreadRollbackResponse = { thread: Thread };`)
	require.Contains(t, string(typeScript), `method: "thread/name/set"; params: ThreadSetNameParams`)
	require.Contains(t, string(typeScript), `ThreadSetNameParams = { threadId: string; name: string }`)
	require.Contains(t, string(typeScript), `ThreadReadParams = { threadId: string; includeTurns?: boolean }`)
	require.Contains(t, string(typeScript), `UserInput = { type: "text"; text: string; textElements?: unknown[] }`)
	require.Contains(t, string(typeScript), `TurnStartParams = { threadId: string; clientUserMessageId?: string | null; input: UserInput[];`)
	require.Contains(t, string(typeScript), `TurnSteerParams = { threadId: string; expectedTurnId: string; clientUserMessageId?: string | null; input: UserInput[];`)
	require.Contains(t, string(typeScript), `effort?: ReasoningEffort | null;`)
	require.Contains(t, string(typeScript), `sandboxPolicy?: SandboxPolicy | null`)
	require.NotContains(t, string(typeScript), `TurnStartParams = { threadId: string; clientUserMessageId?: string | null; input: Array<{ type: "text"; text: string }>; model?: string; approvalPolicy?: ApprovalPolicy; approvalsReviewer?: "user"; sandbox?:`)
	require.Contains(t, string(typeScript), `ThreadListParams = { cursor?: string | null; limit?: number | null; sortKey?: "created_at" | "updated_at" | null; sortDirection?: "asc" | "desc" | null; searchTerm?: string | null; archived?: boolean; [key: string]: unknown }`)
	require.Contains(t, string(typeScript), `method: "thread/archive"; params: ThreadArchiveParams`)
	require.Contains(t, string(typeScript), `ThreadArchiveParams = { threadId: string }`)
	require.Contains(t, string(typeScript), `ThreadArchiveResponse = Record<string, never>`)
	require.Contains(t, string(typeScript), `method: "thread/unarchive"; params: ThreadUnarchiveParams`)
	require.Contains(t, string(typeScript), `ThreadUnarchiveParams = { threadId: string }`)
	require.Contains(t, string(typeScript), `ThreadUnarchiveResponse = { thread: Thread }`)
	require.Contains(t, string(typeScript), `method: "thread/delete"; params: ThreadDeleteParams`)
	require.Contains(t, string(typeScript), `ThreadDeleteResponse = Record<string, never>`)
	require.Contains(t, string(typeScript), `method: "thread/compact/start"; params: ThreadCompactStartParams`)
	require.Contains(t, string(typeScript), `ThreadCompactStartParams = { threadId: string }`)
	require.Contains(t, string(typeScript), `ThreadCompactStartResponse = Record<string, never>`)
	require.Contains(t, string(typeScript), `method: "thread/shellCommand"; params: ThreadShellCommandParams`)
	require.Contains(t, string(typeScript), `ThreadShellCommandParams = { threadId: string; command: string; timeoutMs?: number | null }`)
	require.Contains(t, string(typeScript), `ThreadShellCommandResponse = Record<string, never>`)
	require.Contains(t, string(serverTypeScript), `method: "item/commandExecution/outputDelta"; params: { threadId: string; turnId: string; itemId: string; delta: string }`)
	require.Contains(t, string(serverTypeScript), `type: "commandExecution"; pluginId: string | null; scriptPath: string | null; command: string; cwd: string; processId: string | null; source: "agent" | "userShell" | "unifiedExecStartup" | "unifiedExecInteraction"; status: "inProgress" | "completed" | "failed" | "declined"`)
	require.Contains(t, string(serverTypeScript), `type: "contextCompaction" }`)
	require.NotContains(t, string(serverTypeScript), `type: "contextCompaction"; status`)
	require.Contains(t, string(typeScript), `method: "thread/loaded/list"; params?: ThreadLoadedListParams`)
	require.Contains(t, string(typeScript), `ThreadLoadedListParams = { cursor?: string | null; limit?: number | null }`)
	require.Contains(t, string(typeScript), `method: "thread/backgroundTerminals/clean"; params: ThreadBackgroundTerminalsCleanParams`)
	require.Contains(t, string(typeScript), `ThreadBackgroundTerminalsCleanResponse = Record<string, never>`)
	require.Contains(t, string(typeScript), `method: "thread/backgroundTerminals/list"; params: ThreadBackgroundTerminalsListParams`)
	require.Contains(t, string(typeScript), `ThreadBackgroundTerminalsListResponse = { data: BackgroundTerminal[]; nextCursor: string | null }`)
	require.Contains(t, string(typeScript), `BackgroundTerminal = { itemId: string; processId: string; command: string; cwd: string; osPid: number | null; cpuPercent: number | null; rssKb: number | null }`)
	require.Contains(t, string(typeScript), `method: "thread/backgroundTerminals/terminate"; params: ThreadBackgroundTerminalsTerminateParams`)
	require.Contains(t, string(typeScript), `ThreadBackgroundTerminalsTerminateResponse = { terminated: boolean }`)
	require.Contains(t, string(typeScript), `method: "thread/unsubscribe"; params: ThreadUnsubscribeParams`)
	require.Contains(t, string(typeScript), `ThreadUnsubscribeStatus = "notLoaded" | "notSubscribed" | "unsubscribed"`)
	require.Contains(t, string(typeScript), `method: "thread/turns/list"; params: ThreadTurnsListParams`)
	require.Contains(t, string(typeScript), `ThreadTurnsListParams = { threadId: string; cursor?: string | null; limit?: number | null; sortDirection?: SortDirection | null; itemsView?: TurnItemsView | null }`)
	require.NotContains(t, string(typeScript), `method: "initialize"; params?: unknown`)
	require.NotContains(t, string(typeScript), `method: "thread/goal/set"`)
	require.NotContains(t, string(typeScript), `method: "thread/goal/get"`)
	require.NotContains(t, string(typeScript), `method: "thread/goal/clear"`)
}

func TestCodexAdapterAcceptsAppServerStdioFlag(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "--stdio"}, WorkingDir: "/workspace", Stdin: bytes.NewReader(nil)})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
}

func TestCodexAdapterDetectsSubcommandsAfterLeadingGlobalFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"-c", "features.code_mode_host=true",
			"-c", `chatgpt_base_url="https://chatgpt.example.invalid"`,
			"-c", `openai_base_url="https://api.example.invalid"`,
			"--enable", "feature", "-m", "gpt-test", "app-server", "--analytics-default-enabled",
		},
		WorkingDir: "/workspace", Stdin: bytes.NewReader(nil),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
	require.Equal(t, "gpt-test", request.Model)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "exec", "review this"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "review this", request.Prompt.Text)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "resume", "session-id"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "fork", "session-id"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionFork, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "review"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "Review the current changes.", request.Prompt.Text)

	request, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "exec", "resume", "session-id", "continue please"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-id", request.Session.ID)
	require.Equal(t, "continue please", request.Prompt.Text)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"-c", `model="gpt-test"`, "login"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 2, `command "login" is not supported`)
}

func TestCodexAdapterIgnoresLeadingAppServerConfig(t *testing.T) {
	for _, override := range []string{"features.code_mode_host=false", "model=\"o3\"", "unknown=true"} {
		request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
			Args:       []string{"-c", override, "app-server", "--stdio"},
			WorkingDir: "/workspace",
		})
		require.NoError(t, err)
		require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)
	}
}

func TestCodexAdapterAcceptsResearchedAppServerNoopFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"app-server", "--stdio", "-c", "model=\"o3\"", "--enable", "feature", "--disable", "other",
			"--strict-config", "--analytics-default-enabled", "--code-mode-host", "https://example.invalid",
			"--ws-auth", "capability-token", "--ws-token-file", "/tmp/token", "--ws-token-sha256", "deadbeef",
			"--ws-shared-secret-file", "/tmp/secret", "--ws-issuer", "issuer", "--ws-audience", "audience",
			"--ws-max-clock-skew-seconds", "30",
		},
		WorkingDir: "/workspace", Stdin: bytes.NewReader(nil),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCodexAppServer, request.Protocol)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "--ws-auth", "not-a-real-mode"}, WorkingDir: "/workspace"})
	require.NoError(t, err)

	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "daemon", "start"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, `app-server command "daemon" is not supported`)
	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"app-server", "proxy"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, `app-server command "proxy" is not supported`)
}

func TestCodexAdapterRejectsInvalidOutputSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.json")
	require.NoError(t, os.WriteFile(path, []byte("invalid"), 0o600))
	_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"exec", "--output-schema", path, "prompt"}, WorkingDir: "/workspace"})
	requireExitError(t, err, 2, "not valid JSON")
}

func TestCodexAdapterUsesExitCodeTwoForInvalidValues(t *testing.T) {
	_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--sandbox", "invalid", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 2, "invalid value")
}

func TestClaudeAdapterTranslatesPrintAndValidatesStreams(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--model", "claude-test", "--effort", "high", "--permission-mode", "bypassPermissions", "--output-format", "json", "prompt"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "claude-test", request.Model)
	require.Equal(t, "high", request.Effort)
	require.True(t, request.Permissions.Bypass)
	require.Equal(t, compatibility.OutputJSON, request.Output.Mode)

	_, err = (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--input-format", "stream-json", "--output-format", "text"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires --output-format stream-json")
}

func TestClaudeAdapterCombinesStdinAndPrompt(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "prompt"},
		WorkingDir: "/workspace",
		Stdin:      bytes.NewBufferString("stdin"),
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.PromptCombined, request.Prompt.Source)
	require.Equal(t, "stdin\n\nprompt", request.Prompt.Text)
}

func TestAgyAdapterTranslatesHeadlessInvocation(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "prompt", "--model", "gemini-test", "--effort", "medium", "--mode", "plan", "--output-format", "json", "--print-timeout", "30s"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.Equal(t, "gemini-test", request.Model)
	require.Equal(t, "medium", request.Effort)
	require.Equal(t, compatibility.PermissionMode("plan"), request.Permissions.Mode)
	require.Equal(t, compatibility.OutputJSON, request.Output.Mode)
	require.Equal(t, 30_000_000_000, int(request.Limits.Timeout))
}

func TestAgyAdapterDefaultsPrintTimeoutAndNormalizesPrimitiveSchema(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--print", "prompt", "--json-schema", "string"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, request.Limits.Timeout)
	require.JSONEq(t, `{"type":"string"}`, string(request.Output.Schema))
}

func TestAgyAdapterPreservesStreamJSONInput(t *testing.T) {
	stdin := bytes.NewBufferString("{\"event\":\"user\"}\n")
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--input-format", "stream-json", "--output-format", "stream-json"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterAcceptsEnvironmentAuthorization(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt"},
		WorkingDir: "/workspace",
		Env:        []string{"COPILOT_ALLOW_ALL=true"},
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
}

func TestCopilotAdapterEnablesACPStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n")
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--stdio", "--acp"},
		WorkingDir: "/workspace",
		Stdin:      stdin,
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCopilotACP, request.Protocol)
	require.Equal(t, compatibility.PromptStreamJSON, request.Prompt.Source)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterEnablesSDKStdioProtocol(t *testing.T) {
	stdin := bytes.NewBufferString("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"connect\"}\n")
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--headless", "--stdio", "--no-auto-update"}, WorkingDir: "/workspace", Stdin: stdin})
	require.NoError(t, err)
	require.Equal(t, compatibility.ProtocolCopilotSDK, request.Protocol)
	require.Same(t, stdin, request.Prompt.Stdin)
}

func TestCopilotAdapterAcceptsPipedPrompt(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--allow-all-tools"}, WorkingDir: "/workspace", Stdin: bytes.NewBufferString("prompt")})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, compatibility.PromptStdin, request.Prompt.Source)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestCopilotAdapterAcceptsSpaceSeparatedResumeValue(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--resume", "session-name"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.SessionExplicit, request.Session.Mode)
	require.Equal(t, "session-name", request.Session.ID)
}

func TestCopilotAdapterRequiresHeadlessAuthorization(t *testing.T) {
	_, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires --allow-all-tools")

	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--prompt", "prompt", "--allow-all", "--model", "copilot-test", "--output-format", "json", "--continue"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.True(t, request.Permissions.Bypass)
	require.Equal(t, "copilot-test", request.Model)
	require.Equal(t, compatibility.OutputJSONLines, request.Output.Mode)
	require.Equal(t, compatibility.SessionLatest, request.Session.Mode)
}

func TestClaudeAdapterValidatesPartialStreamOutput(t *testing.T) {
	_, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--print", "--include-partial-messages", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 1, "requires print mode and stream-json output")
}

func TestCodexAdapterPreservesIsolationFlags(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"exec", "--enable", "feature", "--disable", "other", "--remote", "localhost:1",
			"--remote-auth-token-env", "TOKEN", "--strict-config", "--oss", "--local-provider", "provider", "--profile", "profile",
			"--approve-for-me", "--dangerously-bypass-hook-trust", "--search", "--thread-source", "cli", "--ignore-user-config",
			"--ignore-rules", "--color", "auto", "--codex-run-as-apply-patch", "prompt",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.True(t, request.IgnoreUserConfig)
	require.True(t, request.IgnoreRules)
	require.False(t, request.InstructionIsolation)
	require.Equal(t, "cli", request.ThreadSource)
}

func skySightMemgenOverrides() []string {
	return []string{
		`model_reasoning_effort="medium"`,
		`developer_instructions="memgen instructions"`,
		`model_provider="openai-memgen"`,
		`model_providers.openai-memgen.name="OpenAI"`,
		`model_providers.openai-memgen.requires_openai_auth=true`,
		`model_providers.openai-memgen.supports_websockets=true`,
		`model_providers.openai-memgen.http_headers={ "X-OpenAI-Memgen-Request" = "true" }`,
		`features.memories=false`,
		`features.apps=false`,
		`features.plugins=false`,
		`features.multi_agent=false`,
		`features.tool_search=false`,
		`features.tool_suggest=false`,
		`web_search="disabled"`,
		`mcp_servers={}`,
		`plugins={}`,
		`apps._default.enabled=false`,
		`analytics.enabled=false`,
		`otel.exporter="none"`,
		`otel.trace_exporter="none"`,
		`otel.metrics_exporter="none"`,
		`project_doc_max_bytes=0`,
		`skills.bundled.enabled=false`,
		`skills.config=[]`,
	}
}

func TestCodexAdapterTranslatesSkySightMemgenProfile(t *testing.T) {
	args := []string{
		"exec", "--json", "--skip-git-repo-check", "--ephemeral", "--thread-source", "computer_history",
		"--ignore-user-config", "--ignore-rules", "--sandbox", "read-only", "--model", "gpt-5.6-sol",
	}
	for _, override := range skySightMemgenOverrides() {
		args = append(args, "--config", override)
	}
	args = append(args, "-")

	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       args,
		WorkingDir: "/workspace",
		Stdin:      bytes.NewBufferString("consolidate memories"),
	})
	require.NoError(t, err)
	require.Equal(t, "consolidate memories", request.Prompt.Text)
	require.Equal(t, compatibility.PromptStdin, request.Prompt.Source)
	require.Equal(t, "gpt-5.6-sol", request.Model)
	require.Equal(t, "codex", request.ModelProvider)
	require.Equal(t, "codex-memgen", request.ProviderOperation)
	require.Equal(t, "medium", request.AnalysisEffort)
	require.Equal(t, "memgen instructions", request.SystemPrompt)
	require.True(t, request.InstructionIsolation)
	require.True(t, request.IgnoreUserConfig)
	require.True(t, request.IgnoreRules)
	require.Equal(t, "computer_history", request.ThreadSource)
	require.False(t, request.Session.Persistent)
	require.Equal(t, compatibility.PermissionMode("never"), request.Permissions.Mode)
	require.Equal(t, compatibility.SandboxMode("read-only"), request.Permissions.Sandbox)
	require.Equal(t, compatibility.OutputJSONLines, request.Output.Mode)
}

func TestCodexAdapterRejectsPartialSkySightContracts(t *testing.T) {
	overrides := skySightMemgenOverrides()
	for omitted := range overrides {
		key, _, _ := strings.Cut(overrides[omitted], "=")
		t.Run("only "+key, func(t *testing.T) {
			_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
				Args:       []string{"exec", "--config", overrides[omitted], "prompt"},
				WorkingDir: "/workspace",
			})
			requireExitError(t, err, 2, "complete atomic contract")
		})
		t.Run("without "+key, func(t *testing.T) {
			args := []string{"exec"}
			for index, override := range overrides {
				if index != omitted {
					args = append(args, "--config", override)
				}
			}
			args = append(args, "prompt")
			_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: args, WorkingDir: "/workspace"})
			requireExitError(t, err, 2, "complete atomic contract")
		})
	}
}

func TestCodexAdapterRejectsDuplicateConfigOverrides(t *testing.T) {
	for _, override := range skySightMemgenOverrides() {
		key, _, _ := strings.Cut(override, "=")
		invalid := key + `="invalid"`
		for _, values := range [][]string{{invalid, override}, {override, invalid}} {
			t.Run(key+values[0], func(t *testing.T) {
				args := []string{"exec"}
				for _, value := range values {
					args = append(args, "--config", value)
				}
				args = append(args, "prompt")
				_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: args, WorkingDir: "/workspace"})
				requireExitError(t, err, 2, "duplicate --config override "+key)
			})
		}
	}
}

func TestCodexAdapterRejectsUnsupportedConfigOverrides(t *testing.T) {
	for _, test := range []struct {
		override string
		message  string
	}{
		{override: `model_reasoning_effort="extreme"`, message: "unsupported model_reasoning_effort"},
		{override: `features.memories=true`, message: "unsupported --config features.memories"},
	} {
		_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
			Args:       []string{"exec", "--config", test.override, "prompt"},
			WorkingDir: "/workspace",
		})
		requireExitError(t, err, 2, test.message)
	}
}

func TestCodexAdapterIgnoresUnsupportedAdditionalDirectory(t *testing.T) {
	request, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--add-dir", "/other", "prompt"},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, "prompt", request.Prompt.Text)
	require.Equal(t, "/workspace", request.WorkingDir)
	require.Empty(t, request.AdditionalDirectories)
	require.False(t, request.Permissions.Bypass)
}

func TestCodexAdapterRejectsInteractiveApprovalForHeadlessReadOnly(t *testing.T) {
	_, err := (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"exec", "--sandbox", "read-only", "--ask-for-approval", "on-request", "prompt"},
		WorkingDir: "/workspace",
	})
	requireExitError(t, err, 2, "cannot be enforced by Crux")
}

func TestClaudeAdapterAcceptsResearchedNoopFlags(t *testing.T) {
	request, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"--print", "--agents", `{"reviewer":{}}`, "--autocompact", "auto", "--max-thinking-tokens", "100",
			"--from-pr=12", "--name", "session", "--worktree=branch", "--tmux", "--file", "file.txt", "--betas", "beta",
			"--tools", "default", "--permission-prompt-tool", "prompt", "--allow-dangerously-skip-permissions",
			"--exclude-dynamic-system-prompt-sections", "--forward-subagent-text", "--prompt-suggestions",
			"--settings", "settings.json", "--setting-sources", "user", "--bare", "--safe-mode", "--disable-slash-commands",
			"--plugin-dir", "plugin", "--plugin-url", "https://example.invalid/plugin", "--mcp-config", "mcp.json", "--strict-mcp-config",
			"--ide", "--chrome", "--no-chrome", "--cloud=task", "--environment", "env", "--teleport=session",
			"--remote-control=name", "--remote-control-session-name-prefix", "prefix", "--debug=api", "--debug-file", "debug.log",
			"--brief", "--ax-screen-reader", "--thinking", "adaptive", "--thinking-display", "summary",
			"--append-subagent-system-prompt", "extra", "--plan-mode-instructions", "plan", "--resume-session-at", "turn",
			"--resume-drops-turn", "--reply-on-resume", "--rewind-files", "--watch-artifact", "artifact",
			"--watch-artifact-no-autoreact", "--prefill", "text", "--prefill-b64", "dGV4dA==", "--deep-link-origin", "origin",
			"--deep-link-repo", "repo", "--deep-link-last-fetch", "now", "--deep-link-cwd-b64", "L3RtcA==",
			"--plugin-dir-no-mcp", "plugin", "--advisor", "advisor", "--enable-auto-mode", "--messaging-socket-path", "socket",
			"--channels", "channel", "--dangerously-load-development-channels", "--no-home-settings", "--remote=host", "--pool=pool",
			"--correlation-id", "id", "--ref", "ref", "--on-branch", "branch", "--rc", "--init", "--init-only", "--maintenance",
			"--debug-to-stderr", "--session-mirror", "mirror", "--enable-auth-status", "--task-budget", "10", "--workload", "work",
			"--managed-settings", "managed.json", "--sdk-url", "https://example.invalid/sdk", "--handle-uri", "claude://test",
			"--agent-id", "id", "--agent-name", "name", "--team-name", "team", "--agent-color", "blue", "--plan-mode-required",
			"--parent-session-id", "parent", "--teammate-mode", "mode", "--agent-type", "type", "prompt",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestNoopFlagsAcceptAnyExplicitValue(t *testing.T) {
	_, err := (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--autocompact", "not-a-real-mode", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (claudeAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--setting-sources", "user,invalid", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--context", "invalid", "--allow-all-tools", "-p", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"--dynamic-retrieval", "skills=maybe", "--allow-all-tools", "-p", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
	_, err = (codexAdapter{}).Translate(t.Context(), compatibility.Invocation{Args: []string{"exec", "--color", "not-a-real-color", "prompt"}, WorkingDir: "/workspace"})
	require.NoError(t, err)
}

func TestAgyAdapterDefaultsStreamTurnsToFiveMinutes(t *testing.T) {
	request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args:       []string{"--input-format", "stream-json", "--output-format", "stream-json"},
		WorkingDir: "/workspace",
		Stdin:      strings.NewReader(""),
	})
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, request.Limits.Timeout)
}

func TestAgyAdapterIgnoresUnsupportedModeEntrypoints(t *testing.T) {
	for _, flag := range []string{"--remote-control", "--bg-updater"} {
		request, err := (agyAdapter{}).Translate(t.Context(), compatibility.Invocation{
			Args:       []string{flag, "--print", "prompt"},
			WorkingDir: "/workspace",
		})
		require.NoError(t, err)
		require.Equal(t, compatibility.ExecutionHeadless, request.Style)
		require.Equal(t, "prompt", request.Prompt.Text)
	}
}

func TestCopilotAdapterAcceptsResearchedNoopFlags(t *testing.T) {
	request, err := (copilotAdapter{}).Translate(t.Context(), compatibility.Invocation{
		Args: []string{
			"--prompt", "prompt", "--allow-all-tools", "--connect=session", "--context", "long_context", "--name", "name",
			"--enable-memory", "--share=transcript", "--share-gist", "--max-ai-credits", "10", "--enable-reasoning-summaries",
			"--plugin-dir", "plugin", "--secret-env-vars", "SECRET", "--disallow-temp-dir", "--disable-builtin-mcps",
			"--enable-mcp-server", "server", "--allow-all-mcp-server-instructions", "--add-github-mcp-tool", "issues",
			"--add-github-mcp-toolset", "all", "--enable-all-github-mcp-tools", "--banner", "--no-banner", "--bash-env",
			"--no-bash-env", "--mouse", "--no-mouse", "--experimental", "--no-experimental", "--extension-sdk-path", "sdk",
			"--log-dir", "logs", "--log-level", "debug", "--config-dir", "config", "--sandbox", "--no-sandbox",
			"--worktree=branch", "--remote", "--no-remote", "--remote-export", "--no-remote-export", "--with-token",
			"--binary-version", "--prefer-version", "1.0.80", "--print-debug-info", "--session-idle-timeout", "60",
			"--dynamic-retrieval", "skills=on", "--cloud", "--server", "--headless", "--ui-server", "--host", "127.0.0.1",
		},
		WorkingDir: "/workspace",
	})
	require.NoError(t, err)
	require.Equal(t, compatibility.ExecutionHeadless, request.Style)
	require.Equal(t, "prompt", request.Prompt.Text)
}

func TestAdaptersReturnTargetShapedHelp(t *testing.T) {
	tests := []struct {
		adapter compatibility.Adapter
		name    string
		stderr  bool
	}{
		{adapter: codexAdapter{}, name: "codex"},
		{adapter: claudeAdapter{}, name: "claude"},
		{adapter: agyAdapter{}, name: "agy", stderr: true},
		{adapter: copilotAdapter{}, name: "copilot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := test.adapter.Translate(context.Background(), compatibility.Invocation{Args: []string{"--help"}, WorkingDir: "/workspace"})
			exitError, ok := errors.AsType[*compatibility.ExitError](err)
			require.True(t, ok)
			require.Zero(t, exitError.Code)
			if test.stderr {
				require.NotEmpty(t, exitError.Stderr)
				require.Empty(t, exitError.Stdout)
			} else {
				require.NotEmpty(t, exitError.Stdout)
				require.Empty(t, exitError.Stderr)
			}
		})
	}
}

func requireExitError(t *testing.T, err error, code int, message string) {
	t.Helper()
	exitError, ok := errors.AsType[*compatibility.ExitError](err)
	require.True(t, ok)
	require.Equal(t, code, exitError.Code)
	require.Contains(t, exitError.Stderr, message)
}
