package localaddon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/example-git/crux/internal/compatibility"
)

func runCodexSchemaGenerator(request compatibility.Request) error {
	out := request.Metadata["schema-out"]
	if out == "" {
		return fmt.Errorf("Codex schema output directory is required")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create Codex schema output directory: %w", err)
	}
	switch request.Metadata["schema-command"] {
	case "generate-json-schema", "generate-internal-json-schema":
		return writeCodexJSONSchemas(out)
	case "generate-ts":
		return writeCodexTypeScriptSchemas(out)
	default:
		return fmt.Errorf("unsupported Codex schema command %q", request.Metadata["schema-command"])
	}
}

func writeCodexJSONSchemas(out string) error {
	requestSchema := codexClientRequestSchema()
	notificationSchema := codexServerNotificationSchema()
	aggregate := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#",
		"title":   "Crux Codex App Server Compatibility Protocol",
		"definitions": map[string]any{
			"GetAuthStatusParams":   codexGetAuthStatusParamsSchema(),
			"GetAuthStatusResponse": codexGetAuthStatusResponseSchema(),
			"ConfigReadResponse":    codexConfigReadResponseSchema(),
			"ClientRequest":         requestSchema,
			"ServerNotification":    notificationSchema,
			"Thread":                codexThreadSchema(),
			"Turn":                  codexTurnSchema(),
			"Item":                  codexItemSchema(),
		},
	}
	files := map[string]any{
		"ClientRequest.json":                           requestSchema,
		"ServerNotification.json":                      notificationSchema,
		"codex_app_server_protocol.schemas.json":       aggregate,
		"codex_app_server_protocol.v2.schemas.json":    aggregate,
		"GetAuthStatusParams.json":                     codexGetAuthStatusParamsSchema(),
		"GetAuthStatusResponse.json":                   codexGetAuthStatusResponseSchema(),
		"v2/GetAccountResponse.json":                   objectSchema(map[string]any{"account": objectSchema(map[string]any{"type": enumSchema("apiKey")}, "type"), "requiresOpenaiAuth": map[string]any{"const": false}}, "account", "requiresOpenaiAuth"),
		"v2/GatewayOAuthReadResponse.json":             objectSchema(map[string]any{"providerId": map[string]any{"const": "crux"}, "providerName": map[string]any{"const": "Crux"}, "required": map[string]any{"const": false}, "status": map[string]any{"type": "null"}, "error": map[string]any{"type": "null"}}, "providerId", "providerName", "required", "status", "error"),
		"v2/LoginAccountResponse.json":                 objectSchema(map[string]any{"type": enumSchema("apiKey")}, "type"),
		"v2/LogoutAccountResponse.json":                objectSchema(nil),
		"v2/ConfigReadResponse.json":                   codexConfigReadResponseSchema(),
		"v2/ConfigBatchWriteParams.json":               codexConfigBatchWriteParamsSchema(),
		"v2/ConfigWriteResponse.json":                  objectSchema(map[string]any{"status": enumSchema("ok"), "version": stringSchema(), "filePath": stringSchema(), "overriddenMetadata": map[string]any{"type": "null"}}, "status", "version", "filePath", "overriddenMetadata"),
		"v2/ConfigRequirementsReadResponse.json":       objectSchema(map[string]any{"requirements": nullableSchema(openObjectSchema(nil))}, "requirements"),
		"v2/SkillsListResponse.json":                   codexSkillsListResponseSchema(),
		"v2/FuzzyFileSearchResponse.json":              objectSchema(map[string]any{"files": arraySchema(codexFuzzyFileSchema())}, "files"),
		"v2/FuzzyFileSearchSessionStartResponse.json":  objectSchema(nil),
		"v2/FuzzyFileSearchSessionUpdateResponse.json": objectSchema(nil),
		"v2/FuzzyFileSearchSessionStopResponse.json":   objectSchema(nil),
		"v2/FuzzyFileSearchSessionUpdatedNotification.json": objectSchema(map[string]any{
			"sessionId": stringSchema(), "query": stringSchema(), "files": arraySchema(codexFuzzyFileSchema()),
		}, "sessionId", "query", "files"),
		"v2/FuzzyFileSearchSessionCompletedNotification.json": objectSchema(map[string]any{"sessionId": stringSchema()}, "sessionId"),
		"v2/ModelListResponse.json":                           pagedSchema(arraySchema(map[string]any{})),
		"v2/ThreadListResponse.json": objectSchema(map[string]any{
			"data": arraySchema(codexThreadSchema()), "nextCursor": nullableSchema(stringSchema()), "backwardsCursor": nullableSchema(stringSchema()),
		}, "data", "nextCursor", "backwardsCursor"),
		"v2/ThreadReadResponse.json":                     objectSchema(map[string]any{"thread": codexThreadSchema()}, "thread"),
		"v2/ThreadLoadedListResponse.json":               pagedSchema(arraySchema(stringSchema())),
		"v2/ThreadBackgroundTerminalsCleanResponse.json": objectSchema(nil),
		"v2/ThreadBackgroundTerminalsListResponse.json":  pagedSchema(arraySchema(codexBackgroundTerminalSchema())),
		"v2/ThreadBackgroundTerminalsTerminateResponse.json": objectSchema(map[string]any{
			"terminated": booleanSchema(),
		}, "terminated"),
		"v2/ThreadSetNameResponse.json":         objectSchema(nil),
		"v2/ThreadNameUpdatedNotification.json": objectSchema(map[string]any{"threadId": stringSchema(), "threadName": nullableSchema(stringSchema())}, "threadId"),
		"v2/ThreadArchiveResponse.json":         objectSchema(nil),
		"v2/ThreadUnarchiveResponse.json":       objectSchema(map[string]any{"thread": codexThreadSchema()}, "thread"),
		"v2/ThreadDeleteResponse.json":          objectSchema(nil),
		"v2/ThreadRollbackResponse.json":        objectSchema(map[string]any{"thread": codexThreadSchema()}, "thread"),
		"v2/ThreadCompactStartResponse.json":    objectSchema(nil),
		"v2/ThreadShellCommandResponse.json":    objectSchema(nil),
		"v2/DeprecationNoticeNotification.json": objectSchema(map[string]any{"summary": stringSchema(), "details": nullableSchema(stringSchema())}, "summary"),
		"v2/CommandExecutionOutputDeltaNotification.json": objectSchema(map[string]any{
			"threadId": stringSchema(), "turnId": stringSchema(), "itemId": stringSchema(), "delta": stringSchema(),
		}, "threadId", "turnId", "itemId", "delta"),
		"v2/ThreadUnsubscribeResponse.json": objectSchema(map[string]any{"status": enumSchema("notLoaded", "notSubscribed", "unsubscribed")}, "status"),
		"v2/ThreadTurnsListResponse.json": objectSchema(map[string]any{
			"data": arraySchema(codexTurnSchema()), "nextCursor": nullableSchema(stringSchema()), "backwardsCursor": nullableSchema(stringSchema()),
		}, "data", "nextCursor", "backwardsCursor"),
		"v2/ThreadResumeResponse.json":      codexThreadStartResponseSchema(),
		"v2/ThreadForkResponse.json":        codexThreadStartResponseSchema(),
		"v2/ThreadStartResponse.json":       codexThreadStartResponseSchema(),
		"v2/TurnStartResponse.json":         objectSchema(map[string]any{"turn": codexTurnSchema()}, "turn"),
		"v2/TurnCompletedNotification.json": objectSchema(map[string]any{"threadId": stringSchema(), "turn": codexTurnSchema()}, "threadId", "turn"),
		"crux_compatibility.json":           map[string]any{"contractVersion": "0.153.4", "scope": "implemented-subset", "methods": codexSupportedMethods()},
	}
	for name, schema := range files {
		if err := writeGeneratedJSON(filepath.Join(out, name), schema); err != nil {
			return err
		}
	}
	return nil
}

func writeCodexTypeScriptSchemas(out string) error {
	files := map[string]string{
		"GetAuthStatusParams.ts":         `export type GetAuthStatusParams = { includeToken?: boolean | null; refreshToken?: boolean | null };` + "\n",
		"GetAuthStatusResponse.ts":       `export type GetAuthStatusResponse = { authMethod: "apikey"; authToken: null; requiresOpenaiAuth: false };` + "\n",
		"v2/GetAccountResponse.ts":       `export type GetAccountResponse = { account: { type: "apiKey" }; requiresOpenaiAuth: false };` + "\n",
		"v2/GatewayOAuthReadResponse.ts": `export type GatewayOAuthReadResponse = { providerId: "crux"; providerName: "Crux"; required: false; status: null; error: null };` + "\n",
		"v2/LoginAccountResponse.ts":     `export type LoginAccountResponse = { type: "apiKey" };` + "\n",
		"v2/LogoutAccountResponse.ts":    `export type LogoutAccountResponse = Record<string, never>;` + "\n",
		"v2/ConfigBatchWriteParams.ts": `export type ConfigEdit = { keyPath: string; value: unknown; mergeStrategy: "replace" | "upsert" };
export type ConfigBatchWriteParams = { edits: ConfigEdit[]; filePath?: string | null; expectedVersion?: string | null; [key: string]: unknown };
`,
		"v2/ConfigWriteResponse.ts": `export type ConfigWriteResponse = { status: "ok"; version: string; filePath: string; overriddenMetadata: null };` + "\n",
		"v2/ConfigReadResponse.ts": `export type ConfigLayerSource = { type: "system"; file: string } | { type: "user"; file: string; profile: null } | { type: "project"; dotCodexFolder: string } | { type: "sessionFlags" };
export type ConfigReadResponse = { config: { model?: string; model_provider?: string; model_providers?: { crux: { name: string } }; memories?: { consolidation_model?: string }; desktop?: Record<string, unknown> }; origins: Record<string, { name: ConfigLayerSource; version: string }> };
`,
		"ClientRequest.ts": `import type { ConfigBatchWriteParams } from "./v2/ConfigBatchWriteParams";

export type ClientRequest =
  | { id: string | number; method: "initialize"; params: InitializeParams }
  | { id: string | number; method: "getAuthStatus"; params?: { includeToken?: boolean | null; refreshToken?: boolean | null } | null }
  | { id: string | number; method: "account/read"; params?: unknown }
  | { id: string | number; method: "account/gatewayOAuth/read"; params?: unknown }
  | { id: string | number; method: "account/login/start"; params?: unknown }
  | { id: string | number; method: "account/logout"; params?: unknown }
  | { id: string | number; method: "configRequirements/read"; params?: unknown }
  | { id: string | number; method: "config/read"; params?: { cwd?: string; [key: string]: unknown } }
  | { id: string | number; method: "config/batchWrite"; params: ConfigBatchWriteParams }
  | { id: string | number; method: "skills/list"; params?: SkillsListParams }
  | { id: string | number; method: "fuzzyFileSearch"; params: FuzzyFileSearchParams }
  | { id: string | number; method: "fuzzyFileSearch/sessionStart"; params: FuzzyFileSearchSessionStartParams }
  | { id: string | number; method: "fuzzyFileSearch/sessionUpdate"; params: FuzzyFileSearchSessionUpdateParams }
  | { id: string | number; method: "fuzzyFileSearch/sessionStop"; params: FuzzyFileSearchSessionStopParams }
  | { id: string | number; method: "model/list"; params?: unknown }
  | { id: string | number; method: "thread/start"; params?: ThreadStartParams }
  | { id: string | number; method: "thread/list"; params?: ThreadListParams }
  | { id: string | number; method: "thread/read"; params: ThreadReadParams }
  | { id: string | number; method: "thread/loaded/list"; params?: ThreadLoadedListParams }
  | { id: string | number; method: "thread/backgroundTerminals/clean"; params: ThreadBackgroundTerminalsCleanParams }
  | { id: string | number; method: "thread/backgroundTerminals/list"; params: ThreadBackgroundTerminalsListParams }
  | { id: string | number; method: "thread/backgroundTerminals/terminate"; params: ThreadBackgroundTerminalsTerminateParams }
  | { id: string | number; method: "thread/turns/list"; params: ThreadTurnsListParams }
  | { id: string | number; method: "thread/resume"; params: ThreadResumeParams }
  | { id: string | number; method: "thread/fork"; params: ThreadForkParams }
  | { id: string | number; method: "thread/name/set"; params: ThreadSetNameParams }
  | { id: string | number; method: "thread/archive"; params: ThreadArchiveParams }
  | { id: string | number; method: "thread/unarchive"; params: ThreadUnarchiveParams }
  | { id: string | number; method: "thread/delete"; params: ThreadDeleteParams }
  | { id: string | number; method: "thread/rollback"; params: ThreadRollbackParams }
  | { id: string | number; method: "thread/unsubscribe"; params: ThreadUnsubscribeParams }
  | { id: string | number; method: "thread/compact/start"; params: ThreadCompactStartParams }
  | { id: string | number; method: "thread/shellCommand"; params: ThreadShellCommandParams }
  | { id: string | number; method: "turn/start"; params: TurnStartParams }
  | { id: string | number; method: "turn/steer"; params: TurnSteerParams }
  | { id: string | number; method: "turn/interrupt"; params: TurnInterruptParams };

export type InitializeParams = { clientInfo: { name: string; version: string; title?: string | null }; capabilities?: InitializeCapabilities | null };
export type InitializeCapabilities = { experimentalApi?: boolean; requestAttestation?: boolean; mcpServerOpenaiFormElicitation?: boolean; optOutNotificationMethods?: string[] | null; extensions?: Record<string, unknown> };
export type ConfigRequirementsReadResponse = { requirements: Record<string, unknown> | null };
export type SkillsListParams = { cwds?: string[]; forceReload?: boolean };
export type SkillScope = "user" | "repo" | "system" | "admin";
export type SkillToolDependency = { type: string; value: string; description?: string; transport?: string; command?: string; url?: string };
export type SkillDependencies = { tools: SkillToolDependency[] };
export type SkillInterface = { displayName?: string; shortDescription?: string; iconSmall?: string; iconLarge?: string; iconSmallUrl: string | null; iconLargeUrl: string | null; brandColor?: string; defaultPrompt?: string };
export type SkillMetadata = { name: string; description: string; shortDescription?: string; interface?: SkillInterface; dependencies?: SkillDependencies; path: string; scope: SkillScope; enabled: boolean; pluginId: string | null };
export type SkillErrorInfo = { path: string; message: string };
export type SkillsListEntry = { cwd: string; skills: SkillMetadata[]; errors: SkillErrorInfo[] };
export type SkillsListResponse = { data: SkillsListEntry[] };
export type ApprovalPolicy = "on-request" | "never";
export type SandboxMode = "read-only" | "workspace-write" | "danger-full-access";
export type SandboxPolicy = { type: "readOnly"; networkAccess?: false } | { type: "workspaceWrite"; writableRoots?: []; networkAccess?: false; excludeTmpdirEnvVar?: false; excludeSlashTmp?: false } | { type: "dangerFullAccess" };
export type ReasoningEffort = "none" | "low" | "medium" | "high" | "xhigh" | "max";
export type FuzzyFile = { root: string; path: string; match_type: "file" | "directory"; file_name: string; score: number; indices?: number[] };
export type FuzzyFileSearchParams = { query: string; roots: string[]; cancellationToken?: string };
export type FuzzyFileSearchResponse = { files: FuzzyFile[] };
export type FuzzyFileSearchSessionStartParams = { sessionId: string; roots: string[] };
export type FuzzyFileSearchSessionUpdateParams = { sessionId: string; query: string };
export type FuzzyFileSearchSessionStopParams = { sessionId: string };
export type FuzzyFileSearchSessionStartResponse = Record<string, never>;
export type FuzzyFileSearchSessionUpdateResponse = Record<string, never>;
export type FuzzyFileSearchSessionStopResponse = Record<string, never>;
export type ThreadStartParams = { cwd?: string; model?: string; modelProvider?: string; approvalPolicy?: ApprovalPolicy; approvalsReviewer?: "user"; sandbox?: SandboxMode; ephemeral?: boolean; [key: string]: unknown };
export type ThreadListParams = { cursor?: string | null; limit?: number | null; sortKey?: "created_at" | "updated_at" | null; sortDirection?: "asc" | "desc" | null; searchTerm?: string | null; archived?: boolean; [key: string]: unknown };
export type ThreadReadParams = { threadId: string; includeTurns?: boolean };
export type ThreadLoadedListParams = { cursor?: string | null; limit?: number | null };
export type BackgroundTerminal = { itemId: string; processId: string; command: string; cwd: string; osPid: number | null; cpuPercent: number | null; rssKb: number | null };
export type ThreadBackgroundTerminalsCleanParams = { threadId: string };
export type ThreadBackgroundTerminalsCleanResponse = Record<string, never>;
export type ThreadBackgroundTerminalsListParams = { threadId: string; cursor?: string | null; limit?: number | null };
export type ThreadBackgroundTerminalsListResponse = { data: BackgroundTerminal[]; nextCursor: string | null };
export type ThreadBackgroundTerminalsTerminateParams = { threadId: string; processId: string };
export type ThreadBackgroundTerminalsTerminateResponse = { terminated: boolean };
export type SortDirection = "asc" | "desc";
export type TurnItemsView = "notLoaded" | "summary" | "full";
export type ThreadTurnsListParams = { threadId: string; cursor?: string | null; limit?: number | null; sortDirection?: SortDirection | null; itemsView?: TurnItemsView | null };
export type ThreadResumeParams = { threadId: string; cwd?: string; model?: string; modelProvider?: string; [key: string]: unknown };
export type ThreadForkParams = { threadId: string; [key: string]: unknown };
export type ThreadSetNameParams = { threadId: string; name: string };
export type ThreadSetNameResponse = Record<string, never>;
export type ThreadArchiveParams = { threadId: string };
export type ThreadArchiveResponse = Record<string, never>;
export type ThreadUnarchiveParams = { threadId: string };
export type ThreadUnarchiveResponse = { thread: Thread };
export type ThreadDeleteParams = { threadId: string };
export type ThreadDeleteResponse = Record<string, never>;
export type ThreadRollbackParams = { threadId: string; numTurns: number };
export type ThreadRollbackResponse = { thread: Thread };
export type ThreadUnsubscribeParams = { threadId: string };
export type ThreadUnsubscribeStatus = "notLoaded" | "notSubscribed" | "unsubscribed";
export type ThreadUnsubscribeResponse = { status: ThreadUnsubscribeStatus };
export type ThreadCompactStartParams = { threadId: string };
export type ThreadCompactStartResponse = Record<string, never>;
export type ThreadShellCommandParams = { threadId: string; command: string; timeoutMs?: number | null };
export type ThreadShellCommandResponse = Record<string, never>;
export type UserInput = { type: "text"; text: string; textElements?: unknown[] } | { type: "localImage"; path: string } | { type: "skill"; name: string; path: string };
export type TurnStartParams = { threadId: string; clientUserMessageId?: string | null; input: UserInput[]; model?: string; effort?: ReasoningEffort | null; approvalPolicy?: ApprovalPolicy; approvalsReviewer?: "user"; sandboxPolicy?: SandboxPolicy | null; [key: string]: unknown };
export type TurnSteerParams = { threadId: string; expectedTurnId: string; clientUserMessageId?: string | null; input: UserInput[]; responsesapiClientMetadata?: null; additionalContext?: null };
export type TurnInterruptParams = { threadId: string; turnId: string };
`,
		"ServerNotification.ts": `export type ServerNotification =
  | { method: "deprecationNotice"; params: { summary: string; details?: string | null } }
  | { method: "fuzzyFileSearch/sessionUpdated"; params: { sessionId: string; query: string; files: FuzzyFile[] } }
  | { method: "fuzzyFileSearch/sessionCompleted"; params: { sessionId: string } }
  | { method: "thread/started"; params: { thread: Thread } }
  | { method: "thread/status/changed"; params: { threadId: string; status: ThreadStatus } }
  | { method: "thread/name/updated"; params: { threadId: string; threadName?: string | null } }
  | { method: "thread/archived"; params: { threadId: string } }
  | { method: "thread/unarchived"; params: { threadId: string } }
  | { method: "thread/deleted"; params: { threadId: string } }
  | { method: "thread/closed"; params: { threadId: string } }
  | { method: "turn/started"; params: { threadId: string; turn: Turn } }
  | { method: "item/started"; params: { threadId: string; turnId: string; item: Item } }
  | { method: "item/agentMessage/delta"; params: { threadId: string; turnId: string; itemId: string; delta: string } }
  | { method: "item/commandExecution/outputDelta"; params: { threadId: string; turnId: string; itemId: string; delta: string } }
  | { method: "item/completed"; params: { threadId: string; turnId: string; item: Item } }
  | { method: "turn/completed"; params: { threadId: string; turn: Turn } };

export type ThreadStatus = { type: "notLoaded" | "idle" | "systemError" } | { type: "active"; activeFlags: Array<"waitingOnApproval" | "waitingOnUserInput"> };
export type Thread = { id: string; sessionId: string; forkedFromId: string | null; parentThreadId: string | null; name: string | null; status: ThreadStatus; cwd?: string; model: string | null; modelProvider: string; turns: Turn[] };
export type ItemStatus = "inProgress" | "completed" | "failed" | "interrupted";
export type Turn = { id: string; status: ItemStatus; items: Item[]; itemsView?: TurnItemsView; error: { message: string; codexErrorInfo?: unknown; additionalDetails?: string | null } | null; startedAt?: number; completedAt?: number | null; durationMs?: number | null };
export type UserInput = { type: "text"; text: string; textElements: unknown[] } | { type: "image"; url: string; detail?: string } | { type: "localImage"; path: string } | { type: "skill"; name: string; path: string };
export type Item =
  | { id: string; type: "userMessage"; status?: ItemStatus; clientId: string | null; content: UserInput[] }
  | { id: string; type: "reasoning"; status?: ItemStatus; summary: string[]; content: string[] }
  | { id: string; type: "agentMessage"; status?: ItemStatus; text: string }
  | { id: string; type: "commandExecution"; pluginId: string | null; scriptPath: string | null; command: string; cwd: string; processId: string | null; source: "agent" | "userShell" | "unifiedExecStartup" | "unifiedExecInteraction"; status: "inProgress" | "completed" | "failed" | "declined"; commandActions: Array<{ type: "read" | "listFiles" | "search" | "unknown" }>; aggregatedOutput: string | null; exitCode: number | null; durationMs: number | null }
  | { id: string; type: "contextCompaction" };
`,
		"index.ts": `export * from "./ClientRequest";
export * from "./ServerNotification";
`,
		"crux_compatibility.ts": `export const contractVersion = "0.153.4";
export const scope = "implemented-subset";
`,
	}
	for name, content := range files {
		path := filepath.Join(out, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write Codex TypeScript schema %s: %w", name, err)
		}
	}
	return nil
}

func codexClientRequestSchema() map[string]any {
	methods := map[string]map[string]any{
		"initialize":                    codexInitializeParamsSchema(),
		"account/read":                  codexAccountReadParamsSchema(),
		"account/gatewayOAuth/read":     {},
		"account/login/start":           {},
		"account/logout":                {},
		"getAuthStatus":                 nullableSchema(codexGetAuthStatusParamsSchema()),
		"configRequirements/read":       {},
		"config/read":                   openObjectSchema(map[string]any{"cwd": stringSchema()}),
		"config/batchWrite":             codexConfigBatchWriteParamsSchema(),
		"skills/list":                   openObjectSchema(map[string]any{"cwds": arraySchema(stringSchema()), "forceReload": booleanSchema()}),
		"fuzzyFileSearch":               openObjectSchema(map[string]any{"query": stringSchema(), "roots": arraySchema(stringSchema()), "cancellationToken": stringSchema()}, "query", "roots"),
		"fuzzyFileSearch/sessionStart":  openObjectSchema(map[string]any{"sessionId": stringSchema(), "roots": arraySchema(stringSchema())}, "sessionId", "roots"),
		"fuzzyFileSearch/sessionUpdate": openObjectSchema(map[string]any{"sessionId": stringSchema(), "query": stringSchema()}, "sessionId", "query"),
		"fuzzyFileSearch/sessionStop":   openObjectSchema(map[string]any{"sessionId": stringSchema()}, "sessionId"),
		"model/list":                    openObjectSchema(nil),
		"thread/start":                  openObjectSchema(map[string]any{"cwd": stringSchema(), "model": stringSchema(), "modelProvider": stringSchema(), "approvalPolicy": enumSchema("on-request", "never"), "approvalsReviewer": enumSchema("user"), "sandbox": map[string]any{"anyOf": []any{enumSchema("read-only", "workspace-write", "danger-full-access"), objectSchema(map[string]any{"type": enumSchema("readOnly", "workspaceWrite", "dangerFullAccess")}, "type")}}, "ephemeral": booleanSchema()}),
		"thread/list": openObjectSchema(map[string]any{
			"cursor": nullableSchema(stringSchema()), "limit": nullableSchema(map[string]any{"type": "integer", "minimum": 0}),
			"sortKey": nullableSchema(enumSchema("created_at", "updated_at")), "sortDirection": nullableSchema(enumSchema("asc", "desc")),
			"searchTerm": nullableSchema(stringSchema()), "archived": booleanSchema(),
		}),
		"thread/read":                      openObjectSchema(map[string]any{"threadId": stringSchema(), "includeTurns": booleanSchema()}, "threadId"),
		"thread/loaded/list":               openObjectSchema(map[string]any{"cursor": nullableSchema(stringSchema()), "limit": nullableSchema(map[string]any{"type": "integer", "minimum": 0})}),
		"thread/backgroundTerminals/clean": openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/backgroundTerminals/list": openObjectSchema(map[string]any{
			"threadId": stringSchema(), "cursor": nullableSchema(stringSchema()), "limit": nullableSchema(map[string]any{"type": "integer", "minimum": 0}),
		}, "threadId"),
		"thread/backgroundTerminals/terminate": openObjectSchema(map[string]any{"threadId": stringSchema(), "processId": stringSchema()}, "threadId", "processId"),
		"thread/turns/list": openObjectSchema(map[string]any{
			"threadId": stringSchema(), "cursor": nullableSchema(stringSchema()), "limit": nullableSchema(map[string]any{"type": "integer", "minimum": 0}),
			"sortDirection": nullableSchema(enumSchema("asc", "desc")), "itemsView": nullableSchema(enumSchema("notLoaded", "summary", "full")),
		}, "threadId"),
		"thread/resume":        openObjectSchema(map[string]any{"threadId": stringSchema(), "cwd": stringSchema(), "model": stringSchema(), "modelProvider": stringSchema()}, "threadId"),
		"thread/fork":          openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/name/set":      openObjectSchema(map[string]any{"threadId": stringSchema(), "name": stringSchema()}, "threadId", "name"),
		"thread/archive":       openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/unarchive":     openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/delete":        openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/rollback":      openObjectSchema(map[string]any{"threadId": stringSchema(), "numTurns": map[string]any{"type": "integer", "minimum": 0}}, "threadId", "numTurns"),
		"thread/unsubscribe":   openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/compact/start": openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/shellCommand":  openObjectSchema(map[string]any{"threadId": stringSchema(), "command": stringSchema(), "timeoutMs": nullableSchema(map[string]any{"type": "integer"})}, "threadId", "command"),
		"turn/start":           openObjectSchema(map[string]any{"threadId": stringSchema(), "clientUserMessageId": nullableSchema(stringSchema()), "input": arraySchema(codexUserInputSchema()), "model": stringSchema(), "effort": nullableSchema(enumSchema("none", "low", "medium", "high", "xhigh", "max")), "approvalPolicy": enumSchema("on-request", "never"), "approvalsReviewer": enumSchema("user"), "sandboxPolicy": nullableSchema(codexTurnSandboxPolicySchema())}, "threadId", "input"),
		"turn/steer":           openObjectSchema(map[string]any{"threadId": stringSchema(), "expectedTurnId": stringSchema(), "clientUserMessageId": nullableSchema(stringSchema()), "input": arraySchema(codexUserInputSchema()), "responsesapiClientMetadata": nullableSchema(objectSchema(nil)), "additionalContext": nullableSchema(objectSchema(nil))}, "threadId", "expectedTurnId", "input"),
		"turn/interrupt":       openObjectSchema(map[string]any{"threadId": stringSchema(), "turnId": stringSchema()}, "threadId", "turnId"),
	}
	methodNames := make([]string, 0, len(methods))
	for method := range methods {
		methodNames = append(methodNames, method)
	}
	sort.Strings(methodNames)
	oneOf := make([]any, 0, len(methods))
	for _, method := range methodNames {
		oneOf = append(oneOf, objectSchema(map[string]any{"id": map[string]any{"type": []string{"string", "number"}}, "method": enumSchema(method), "params": methods[method]}, "id", "method"))
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "oneOf": oneOf}
}

func codexUserInputSchema() map[string]any {
	return map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("text"), "text": stringSchema(), "textElements": arraySchema(map[string]any{})}, "type", "text"),
		objectSchema(map[string]any{"type": enumSchema("localImage"), "path": stringSchema()}, "type", "path"),
		objectSchema(map[string]any{"type": enumSchema("skill"), "name": stringSchema(), "path": stringSchema()}, "type", "name", "path"),
	}}
}

func codexInitializeParamsSchema() map[string]any {
	clientInfo := openObjectSchema(map[string]any{
		"name":    stringSchema(),
		"version": stringSchema(),
		"title":   nullableSchema(stringSchema()),
	}, "name", "version")
	capabilities := openObjectSchema(map[string]any{
		"experimentalApi":                booleanSchema(),
		"requestAttestation":             booleanSchema(),
		"mcpServerOpenaiFormElicitation": booleanSchema(),
		"optOutNotificationMethods":      nullableSchema(arraySchema(stringSchema())),
		"extensions":                     openObjectSchema(nil),
	})
	return openObjectSchema(map[string]any{
		"clientInfo":   clientInfo,
		"capabilities": nullableSchema(capabilities),
	}, "clientInfo")
}

func codexAccountReadParamsSchema() map[string]any {
	return map[string]any{}
}

func codexServerNotificationSchema() map[string]any {
	threadID := stringSchema()
	turnID := stringSchema()
	itemID := stringSchema()
	delta := stringSchema()
	methods := map[string]map[string]any{
		"deprecationNotice":                 objectSchema(map[string]any{"summary": stringSchema(), "details": nullableSchema(stringSchema())}, "summary"),
		"fuzzyFileSearch/sessionUpdated":    objectSchema(map[string]any{"sessionId": stringSchema(), "query": stringSchema(), "files": arraySchema(codexFuzzyFileSchema())}, "sessionId", "query", "files"),
		"fuzzyFileSearch/sessionCompleted":  objectSchema(map[string]any{"sessionId": stringSchema()}, "sessionId"),
		"thread/started":                    objectSchema(map[string]any{"thread": codexThreadSchema()}, "thread"),
		"thread/status/changed":             objectSchema(map[string]any{"threadId": threadID, "status": codexThreadStatusSchema()}, "threadId", "status"),
		"thread/name/updated":               objectSchema(map[string]any{"threadId": threadID, "threadName": nullableSchema(stringSchema())}, "threadId"),
		"thread/archived":                   objectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/unarchived":                 objectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/deleted":                    objectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/closed":                     objectSchema(map[string]any{"threadId": threadID}, "threadId"),
		"turn/started":                      objectSchema(map[string]any{"threadId": threadID, "turn": codexTurnSchema()}, "threadId", "turn"),
		"item/started":                      objectSchema(map[string]any{"threadId": threadID, "turnId": turnID, "item": codexItemSchema()}, "threadId", "turnId", "item"),
		"item/agentMessage/delta":           objectSchema(map[string]any{"threadId": threadID, "turnId": turnID, "itemId": itemID, "delta": delta}, "threadId", "turnId", "itemId", "delta"),
		"item/commandExecution/outputDelta": objectSchema(map[string]any{"threadId": threadID, "turnId": turnID, "itemId": itemID, "delta": delta}, "threadId", "turnId", "itemId", "delta"),
		"item/completed":                    objectSchema(map[string]any{"threadId": threadID, "turnId": turnID, "item": codexItemSchema()}, "threadId", "turnId", "item"),
		"turn/completed":                    objectSchema(map[string]any{"threadId": threadID, "turn": codexTurnSchema()}, "threadId", "turn"),
	}
	methodNames := make([]string, 0, len(methods))
	for method := range methods {
		methodNames = append(methodNames, method)
	}
	sort.Strings(methodNames)
	oneOf := make([]any, 0, len(methods))
	for _, method := range methodNames {
		oneOf = append(oneOf, objectSchema(map[string]any{"method": enumSchema(method), "params": methods[method]}, "method", "params"))
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "oneOf": oneOf}
}

func codexThreadStatusSchema() map[string]any {
	return map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("notLoaded", "idle", "systemError")}, "type"),
		objectSchema(map[string]any{"type": enumSchema("active"), "activeFlags": arraySchema(enumSchema("waitingOnApproval", "waitingOnUserInput"))}, "type", "activeFlags"),
	}}
}

func codexThreadSchema() map[string]any {
	return openObjectSchema(map[string]any{"id": stringSchema(), "sessionId": stringSchema(), "forkedFromId": nullableSchema(stringSchema()), "parentThreadId": nullableSchema(stringSchema()), "name": nullableSchema(stringSchema()), "status": codexThreadStatusSchema(), "cwd": stringSchema(), "model": nullableSchema(stringSchema()), "modelProvider": stringSchema(), "turns": arraySchema(codexTurnSchema())}, "id", "sessionId", "forkedFromId", "parentThreadId", "name", "status", "model", "modelProvider")
}

func codexTurnSchema() map[string]any {
	return objectSchema(map[string]any{
		"id": stringSchema(), "status": enumSchema("inProgress", "completed", "failed", "interrupted"),
		"items": arraySchema(codexTurnItemSchema()), "itemsView": enumSchema("notLoaded", "summary", "full"),
		"error": nullableSchema(objectSchema(map[string]any{
			"message": stringSchema(), "codexErrorInfo": map[string]any{}, "additionalDetails": nullableSchema(stringSchema()),
		}, "message")),
		"startedAt": map[string]any{"type": "integer"}, "completedAt": nullableSchema(map[string]any{"type": "integer"}), "durationMs": nullableSchema(map[string]any{"type": "integer"}),
	}, "id", "status", "items", "error")
}

func codexCommandExecutionItemSchema() map[string]any {
	commandAction := openObjectSchema(map[string]any{"type": enumSchema("read", "listFiles", "search", "unknown")}, "type")
	return objectSchema(map[string]any{
		"id": stringSchema(), "type": enumSchema("commandExecution"), "pluginId": nullableSchema(stringSchema()), "scriptPath": nullableSchema(stringSchema()),
		"command": stringSchema(), "cwd": stringSchema(), "processId": nullableSchema(stringSchema()),
		"source": enumSchema("agent", "userShell", "unifiedExecStartup", "unifiedExecInteraction"), "status": enumSchema("inProgress", "completed", "failed", "declined"),
		"commandActions": arraySchema(commandAction), "aggregatedOutput": nullableSchema(stringSchema()), "exitCode": nullableSchema(map[string]any{"type": "integer"}), "durationMs": nullableSchema(map[string]any{"type": "integer"}),
	}, "id", "type", "pluginId", "scriptPath", "command", "cwd", "processId", "source", "status", "commandActions", "aggregatedOutput", "exitCode", "durationMs")
}

func codexTurnItemSchema() map[string]any {
	userInput := map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("text"), "text": stringSchema(), "textElements": arraySchema(map[string]any{})}, "type", "text", "textElements"),
		objectSchema(map[string]any{"type": enumSchema("image"), "url": stringSchema(), "detail": stringSchema()}, "type", "url"),
		objectSchema(map[string]any{"type": enumSchema("localImage"), "path": stringSchema()}, "type", "path"),
		objectSchema(map[string]any{"type": enumSchema("skill"), "name": stringSchema(), "path": stringSchema()}, "type", "name", "path"),
	}}
	status := enumSchema("inProgress", "completed", "failed", "interrupted")
	return map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("userMessage"), "status": status, "clientId": nullableSchema(stringSchema()), "content": arraySchema(userInput)}, "id", "type", "clientId", "content"),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("reasoning"), "status": status, "summary": arraySchema(stringSchema()), "content": arraySchema(stringSchema())}, "id", "type", "summary", "content"),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("agentMessage"), "status": status, "text": stringSchema()}, "id", "type", "text"),
		codexCommandExecutionItemSchema(),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("contextCompaction")}, "id", "type"),
	}}
}

func codexItemSchema() map[string]any {
	userInput := map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("text"), "text": stringSchema(), "textElements": arraySchema(map[string]any{})}, "type", "text", "textElements"),
		objectSchema(map[string]any{"type": enumSchema("image"), "url": stringSchema(), "detail": stringSchema()}, "type", "url"),
		objectSchema(map[string]any{"type": enumSchema("localImage"), "path": stringSchema()}, "type", "path"),
		objectSchema(map[string]any{"type": enumSchema("skill"), "name": stringSchema(), "path": stringSchema()}, "type", "name", "path"),
	}}
	return map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("userMessage"), "status": enumSchema("inProgress", "completed", "failed", "interrupted"), "clientId": nullableSchema(stringSchema()), "content": arraySchema(userInput)}, "id", "type", "clientId", "content"),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("reasoning"), "status": enumSchema("inProgress", "completed", "failed", "interrupted"), "summary": arraySchema(stringSchema()), "content": arraySchema(stringSchema())}, "id", "type", "status", "summary", "content"),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("agentMessage"), "status": enumSchema("inProgress", "completed", "failed", "interrupted"), "text": stringSchema()}, "id", "type", "status", "text"),
		codexCommandExecutionItemSchema(),
		objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("contextCompaction")}, "id", "type"),
	}}
}

func codexFuzzyFileSchema() map[string]any {
	return objectSchema(map[string]any{
		"root": stringSchema(), "path": stringSchema(), "match_type": enumSchema("file", "directory"),
		"file_name": stringSchema(), "score": map[string]any{"type": "integer", "minimum": 0}, "indices": arraySchema(map[string]any{"type": "integer", "minimum": 0}),
	}, "root", "path", "match_type", "file_name", "score")
}

func codexSkillsListResponseSchema() map[string]any {
	toolDependency := objectSchema(map[string]any{
		"type": stringSchema(), "value": stringSchema(), "description": nullableSchema(stringSchema()),
		"transport": nullableSchema(stringSchema()), "command": nullableSchema(stringSchema()), "url": nullableSchema(stringSchema()),
	}, "type", "value")
	dependencies := objectSchema(map[string]any{"tools": arraySchema(toolDependency)}, "tools")
	interfaceSchema := objectSchema(map[string]any{
		"displayName": nullableSchema(stringSchema()), "shortDescription": nullableSchema(stringSchema()),
		"iconSmall": nullableSchema(stringSchema()), "iconLarge": nullableSchema(stringSchema()),
		"iconSmallUrl": nullableSchema(stringSchema()), "iconLargeUrl": nullableSchema(stringSchema()),
		"brandColor": nullableSchema(stringSchema()), "defaultPrompt": nullableSchema(stringSchema()),
	})
	metadata := objectSchema(map[string]any{
		"name": stringSchema(), "description": stringSchema(), "shortDescription": nullableSchema(stringSchema()),
		"interface": nullableSchema(interfaceSchema), "dependencies": nullableSchema(dependencies), "path": stringSchema(),
		"scope": enumSchema("user", "repo", "system", "admin"), "enabled": booleanSchema(), "pluginId": nullableSchema(stringSchema()),
	}, "name", "description", "path", "scope", "enabled")
	skillError := objectSchema(map[string]any{"path": stringSchema(), "message": stringSchema()}, "path", "message")
	entry := objectSchema(map[string]any{
		"cwd": stringSchema(), "skills": arraySchema(metadata), "errors": arraySchema(skillError),
	}, "cwd", "skills", "errors")
	return objectSchema(map[string]any{"data": arraySchema(entry)}, "data")
}

func codexBackgroundTerminalSchema() map[string]any {
	return objectSchema(map[string]any{
		"itemId": stringSchema(), "processId": stringSchema(), "command": stringSchema(), "cwd": stringSchema(),
		"osPid": nullableSchema(map[string]any{"type": "integer"}), "cpuPercent": nullableSchema(map[string]any{"type": "number"}), "rssKb": nullableSchema(map[string]any{"type": "integer"}),
	}, "itemId", "processId", "command", "cwd", "osPid", "cpuPercent", "rssKb")
}

func codexTurnSandboxPolicySchema() map[string]any {
	return map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("readOnly"), "networkAccess": map[string]any{"const": false}}, "type"),
		objectSchema(map[string]any{
			"type": enumSchema("workspaceWrite"), "writableRoots": map[string]any{"type": "array", "maxItems": 0},
			"networkAccess": map[string]any{"const": false}, "excludeTmpdirEnvVar": map[string]any{"const": false}, "excludeSlashTmp": map[string]any{"const": false},
		}, "type"),
		objectSchema(map[string]any{"type": enumSchema("dangerFullAccess")}, "type"),
	}}
}

func codexThreadStartResponseSchema() map[string]any {
	return objectSchema(map[string]any{
		"thread": codexThreadSchema(), "model": stringSchema(), "modelProvider": stringSchema(),
		"approvalPolicy": stringSchema(), "approvalsReviewer": stringSchema(), "cwd": stringSchema(),
		"sandbox": objectSchema(map[string]any{"type": stringSchema()}, "type"),
	}, "thread", "model", "modelProvider", "approvalPolicy", "approvalsReviewer", "cwd", "sandbox")
}

func codexGetAuthStatusResponseSchema() map[string]any {
	return objectSchema(map[string]any{"authMethod": enumSchema("apikey"), "authToken": map[string]any{"type": "null"}, "requiresOpenaiAuth": map[string]any{"const": false}}, "authMethod", "authToken", "requiresOpenaiAuth")
}

func codexConfigReadResponseSchema() map[string]any {
	name := map[string]any{"oneOf": []any{
		objectSchema(map[string]any{"type": enumSchema("system"), "file": stringSchema()}, "type", "file"),
		objectSchema(map[string]any{"type": enumSchema("user"), "file": stringSchema(), "profile": map[string]any{"type": "null"}}, "type", "file", "profile"),
		objectSchema(map[string]any{"type": enumSchema("project"), "dotCodexFolder": stringSchema()}, "type", "dotCodexFolder"),
		objectSchema(map[string]any{"type": enumSchema("sessionFlags")}, "type"),
	}}
	origin := objectSchema(map[string]any{"name": name, "version": stringSchema()}, "name", "version")
	return objectSchema(map[string]any{
		"config":  objectSchema(map[string]any{"model": stringSchema(), "model_provider": stringSchema(), "model_providers": objectSchema(map[string]any{"crux": objectSchema(map[string]any{"name": stringSchema()}, "name")}, "crux"), "memories": objectSchema(map[string]any{"consolidation_model": stringSchema()}), "desktop": openObjectSchema(nil)}),
		"origins": map[string]any{"type": "object", "patternProperties": map[string]any{"^(model|model_provider|memories\\.consolidation_model|desktop\\..+)$": origin}, "additionalProperties": false},
	}, "config", "origins")
}

func codexConfigBatchWriteParamsSchema() map[string]any {
	return openObjectSchema(map[string]any{
		"edits":    arraySchema(openObjectSchema(map[string]any{"keyPath": stringSchema(), "value": map[string]any{}, "mergeStrategy": enumSchema("replace", "upsert")}, "keyPath", "value", "mergeStrategy")),
		"filePath": nullableSchema(stringSchema()), "expectedVersion": nullableSchema(stringSchema()),
	}, "edits")
}

func codexGetAuthStatusParamsSchema() map[string]any {
	return openObjectSchema(map[string]any{"includeToken": nullableSchema(booleanSchema()), "refreshToken": nullableSchema(booleanSchema())})
}

func codexSupportedMethods() []string {
	return []string{"initialize", "initialized", "getAuthStatus", "account/read", "account/gatewayOAuth/read", "account/login/start", "account/logout", "configRequirements/read", "config/read", "config/batchWrite", "skills/list", "fuzzyFileSearch", "fuzzyFileSearch/sessionStart", "fuzzyFileSearch/sessionUpdate", "fuzzyFileSearch/sessionStop", "model/list", "thread/start", "thread/list", "thread/read", "thread/loaded/list", "thread/backgroundTerminals/clean", "thread/backgroundTerminals/list", "thread/backgroundTerminals/terminate", "thread/turns/list", "thread/resume", "thread/fork", "thread/name/set", "thread/archive", "thread/unarchive", "thread/delete", "thread/rollback", "thread/unsubscribe", "thread/compact/start", "thread/shellCommand", "turn/start", "turn/steer", "turn/interrupt"}
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	if properties == nil {
		properties = map[string]any{}
	}
	value := map[string]any{"type": "object", "properties": properties, "additionalProperties": false}
	if len(required) != 0 {
		value["required"] = required
	}
	return value
}

func openObjectSchema(properties map[string]any, required ...string) map[string]any {
	value := objectSchema(properties, required...)
	value["additionalProperties"] = true
	return value
}

func stringSchema() map[string]any {
	return map[string]any{"type": "string"}
}

func booleanSchema() map[string]any {
	return map[string]any{"type": "boolean"}
}

func enumSchema(values ...string) map[string]any {
	return map[string]any{"type": "string", "enum": values}
}

func arraySchema(items any) map[string]any {
	return map[string]any{"type": "array", "items": items}
}

func nullableSchema(value any) map[string]any {
	return map[string]any{"anyOf": []any{value, map[string]any{"type": "null"}}}
}

func pagedSchema(data any) map[string]any {
	return objectSchema(map[string]any{"data": data, "nextCursor": nullableSchema(stringSchema())}, "data", "nextCursor")
}

func writeGeneratedJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write Codex JSON schema %s: %w", filepath.Base(path), err)
	}
	return nil
}
