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
			"ClientRequest":      requestSchema,
			"ServerNotification": notificationSchema,
			"Thread":             codexThreadSchema(),
			"Turn":               codexTurnSchema(),
			"Item":               codexItemSchema(),
		},
	}
	files := map[string]any{
		"ClientRequest.json":                        requestSchema,
		"ServerNotification.json":                   notificationSchema,
		"codex_app_server_protocol.schemas.json":    aggregate,
		"codex_app_server_protocol.v2.schemas.json": aggregate,
		"v2/GetAccountResponse.json":                objectSchema(map[string]any{"account": nullableSchema(objectSchema(nil)), "requiresOpenaiAuth": booleanSchema()}, "requiresOpenaiAuth"),
		"v2/ConfigReadResponse.json":                objectSchema(map[string]any{"config": objectSchema(map[string]any{"model": stringSchema(), "model_provider": stringSchema()}), "origins": objectSchema(nil), "layers": arraySchema(objectSchema(nil))}, "config", "origins"),
		"v2/PluginListResponse.json":                objectSchema(map[string]any{"marketplaces": arraySchema(objectSchema(nil)), "marketplaceLoadErrors": arraySchema(objectSchema(nil)), "featuredPluginIds": arraySchema(stringSchema())}, "marketplaces"),
		"v2/ModelListResponse.json":                 pagedSchema(arraySchema(objectSchema(nil))),
		"v2/ThreadListResponse.json":                pagedSchema(arraySchema(codexThreadSchema())),
		"v2/ThreadReadResponse.json":                objectSchema(map[string]any{"thread": codexThreadSchema()}, "thread"),
		"v2/ThreadResumeResponse.json":              codexThreadStartResponseSchema(),
		"v2/ThreadForkResponse.json":                codexThreadStartResponseSchema(),
		"v2/ThreadStartResponse.json":               codexThreadStartResponseSchema(),
		"v2/TurnStartResponse.json":                 objectSchema(map[string]any{"turn": codexTurnSchema()}, "turn"),
		"v2/TurnCompletedNotification.json":         objectSchema(map[string]any{"threadId": stringSchema(), "turn": codexTurnSchema()}, "threadId", "turn"),
		"crux_compatibility.json":                   map[string]any{"contractVersion": "0.149.1", "scope": "implemented-subset", "methods": codexSupportedMethods()},
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
		"ClientRequest.ts": `export type ClientRequest =
  | { id: string | number; method: "initialize"; params?: unknown }
  | { id: string | number; method: "account/read"; params?: unknown }
  | { id: string | number; method: "config/read"; params?: { cwd?: string; includeLayers?: boolean } }
  | { id: string | number; method: "plugin/list"; params?: { cwds?: string[] } }
  | { id: string | number; method: "model/list"; params?: unknown }
  | { id: string | number; method: "thread/start"; params?: ThreadStartParams }
  | { id: string | number; method: "thread/list"; params?: unknown }
  | { id: string | number; method: "thread/read"; params: ThreadReadParams }
  | { id: string | number; method: "thread/resume"; params: ThreadResumeParams }
  | { id: string | number; method: "thread/fork"; params: ThreadForkParams }
  | { id: string | number; method: "turn/start"; params: TurnStartParams }
  | { id: string | number; method: "turn/interrupt"; params: TurnInterruptParams };

export type ApprovalPolicy = "on-request" | "never";
export type SandboxPolicy = "read-only" | "workspace-write" | "danger-full-access" | { type: "readOnly" | "workspaceWrite" | "dangerFullAccess" };
export type ThreadStartParams = { cwd?: string; model?: string; modelProvider?: string; approvalPolicy?: ApprovalPolicy; approvalsReviewer?: "user"; sandbox?: SandboxPolicy; ephemeral?: boolean };
export type ThreadReadParams = { threadId: string };
export type ThreadResumeParams = { threadId: string; cwd?: string; model?: string; modelProvider?: string };
export type ThreadForkParams = { threadId: string };
export type TurnStartParams = { threadId: string; input: Array<{ type: "text"; text: string }>; model?: string; approvalPolicy?: ApprovalPolicy; approvalsReviewer?: "user"; sandbox?: SandboxPolicy };
export type TurnInterruptParams = { threadId: string; turnId: string };
`,
		"ServerNotification.ts": `export type ServerNotification =
  | { method: "thread/started"; params: { thread: Thread } }
  | { method: "turn/started"; params: { threadId: string; turn: Turn } }
  | { method: "item/started"; params: { threadId: string; turnId: string; item: Item } }
  | { method: "item/agentMessage/delta"; params: { threadId: string; turnId: string; itemId: string; delta: string } }
  | { method: "item/completed"; params: { threadId: string; turnId: string; item: Item } }
  | { method: "turn/completed"; params: { threadId: string; turn: Turn } };

export type Thread = { id: string; sessionId: string; status: { type: string; activeFlags?: string[] }; cwd?: string; modelProvider?: string; turns: Turn[] };
export type ItemStatus = "inProgress" | "completed" | "failed" | "interrupted";
export type Turn = { id: string; status: ItemStatus; items: Item[]; error: { message: string } | null };
export type Item = { id: string; type: "agentMessage"; status: ItemStatus; text: string };
`,
		"index.ts": `export * from "./ClientRequest";
export * from "./ServerNotification";
`,
		"crux_compatibility.ts": `export const contractVersion = "0.149.1";
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
		"initialize":     openObjectSchema(nil),
		"account/read":   openObjectSchema(nil),
		"config/read":    openObjectSchema(map[string]any{"cwd": stringSchema(), "includeLayers": booleanSchema()}),
		"plugin/list":    openObjectSchema(map[string]any{"cwds": arraySchema(stringSchema())}),
		"model/list":     openObjectSchema(nil),
		"thread/start":   openObjectSchema(map[string]any{"cwd": stringSchema(), "model": stringSchema(), "modelProvider": stringSchema(), "approvalPolicy": enumSchema("on-request", "never"), "approvalsReviewer": enumSchema("user"), "sandbox": map[string]any{"anyOf": []any{enumSchema("read-only", "workspace-write", "danger-full-access"), objectSchema(map[string]any{"type": enumSchema("readOnly", "workspaceWrite", "dangerFullAccess")}, "type")}}, "ephemeral": booleanSchema()}),
		"thread/list":    openObjectSchema(nil),
		"thread/read":    openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"thread/resume":  openObjectSchema(map[string]any{"threadId": stringSchema(), "cwd": stringSchema(), "model": stringSchema(), "modelProvider": stringSchema()}, "threadId"),
		"thread/fork":    openObjectSchema(map[string]any{"threadId": stringSchema()}, "threadId"),
		"turn/start":     openObjectSchema(map[string]any{"threadId": stringSchema(), "input": arraySchema(openObjectSchema(map[string]any{"type": enumSchema("text"), "text": stringSchema()}, "type", "text")), "model": stringSchema(), "approvalPolicy": enumSchema("on-request", "never"), "approvalsReviewer": enumSchema("user"), "sandbox": map[string]any{"anyOf": []any{enumSchema("read-only", "workspace-write", "danger-full-access"), objectSchema(map[string]any{"type": enumSchema("readOnly", "workspaceWrite", "dangerFullAccess")}, "type")}}}, "threadId", "input"),
		"turn/interrupt": openObjectSchema(map[string]any{"threadId": stringSchema(), "turnId": stringSchema()}, "threadId", "turnId"),
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

func codexServerNotificationSchema() map[string]any {
	methods := []string{"thread/started", "turn/started", "item/started", "item/agentMessage/delta", "item/completed", "turn/completed"}
	oneOf := make([]any, 0, len(methods))
	for _, method := range methods {
		oneOf = append(oneOf, objectSchema(map[string]any{"method": enumSchema(method), "params": objectSchema(nil)}, "method", "params"))
	}
	return map[string]any{"$schema": "http://json-schema.org/draft-07/schema#", "oneOf": oneOf}
}

func codexThreadSchema() map[string]any {
	status := objectSchema(map[string]any{"type": stringSchema(), "activeFlags": arraySchema(stringSchema())}, "type")
	return openObjectSchema(map[string]any{"id": stringSchema(), "sessionId": stringSchema(), "status": status, "cwd": stringSchema(), "modelProvider": stringSchema(), "turns": arraySchema(codexTurnSchema())}, "id", "sessionId", "status")
}

func codexTurnSchema() map[string]any {
	return objectSchema(map[string]any{"id": stringSchema(), "status": enumSchema("inProgress", "completed", "failed", "interrupted"), "items": arraySchema(codexItemSchema()), "error": nullableSchema(objectSchema(map[string]any{"message": stringSchema()}, "message"))}, "id", "status", "items", "error")
}

func codexItemSchema() map[string]any {
	return objectSchema(map[string]any{"id": stringSchema(), "type": enumSchema("agentMessage"), "status": enumSchema("inProgress", "completed", "failed", "interrupted"), "text": stringSchema()}, "id", "type", "status", "text")
}

func codexThreadStartResponseSchema() map[string]any {
	return objectSchema(map[string]any{
		"thread": codexThreadSchema(), "model": stringSchema(), "modelProvider": stringSchema(),
		"approvalPolicy": stringSchema(), "approvalsReviewer": stringSchema(), "cwd": stringSchema(),
		"sandbox": objectSchema(map[string]any{"type": stringSchema()}, "type"),
	}, "thread", "model", "modelProvider", "approvalPolicy", "approvalsReviewer", "cwd", "sandbox")
}

func codexSupportedMethods() []string {
	return []string{"initialize", "initialized", "account/read", "config/read", "plugin/list", "model/list", "thread/start", "thread/list", "thread/read", "thread/resume", "thread/fork", "turn/start", "turn/interrupt"}
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
