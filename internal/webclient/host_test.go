package webclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/server"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestWebListenRejectsNonLoopback(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", "192.0.2.1:0", "localhost:0", "127.0.0.1", "127.0.0.1:invalid"} {
		_, err := Listen(address)
		require.Error(t, err, address)
	}
	listener, err := Listen("127.0.0.1:0")
	require.NoError(t, err)
	require.NoError(t, listener.Close())
}

func TestWebHostWorkspaceDirectoryCreation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	require.NoError(t, native.SetWorkspaceRoots([]string{root}))
	handler := Handler(native, "127.0.0.1:9090", Startup{Path: root})
	body, err := json.Marshal(proto.PeerWorkspaceCreateRequest{Root: root, RelativePath: "project", Mode: proto.PeerWorkspaceCreatePlain})
	require.NoError(t, err)
	for _, origin := range []string{"https://attacker.invalid", "http://127.0.0.1:9090"} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:9090/v1/browser/create", bytes.NewReader(body))
		request.Header.Set("Origin", origin)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if origin == "https://attacker.invalid" {
			require.Equal(t, http.StatusForbidden, response.Code)
			require.NoDirExists(t, filepath.Join(root, "project"))
			continue
		}
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		var result proto.PeerWorkspaceCreateResult
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(t, filepath.Join(root, "project"), result.Path)
		require.DirExists(t, result.Path)
	}
	require.Empty(t, native.Backend().ListWorkspaces())
}

func TestWebHostNativeSettingsAndAgent(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG", "CRUX_GLOBAL_DATA", "CRUX_CACHE_DIR", "AI_CLI_DIR"} {
		path := filepath.Join(root, key)
		t.Setenv(key, path)
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	t.Setenv("CRUX_PROVIDER_PLUGINS", "")
	t.Setenv("CRUX_PROVIDER_PLUGIN_COMPAT", "")
	observed := make(chan map[string]any, 32)
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if json.NewDecoder(r.Body).Decode(&body) != nil || r.URL.Path != "/v1/chat/completions" || r.Header.Get("Authorization") != "Bearer web-fixture-key" {
			http.Error(w, "unexpected provider request", http.StatusBadRequest)
			return
		}
		observed <- body
		if body["stream"] != true {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"Web acceptance"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2,"total_tokens":4}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Web client production path completed.\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		_, _ = io.WriteString(w, "data: {\"id\":\"fixture\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":2,\"total_tokens\":4}}\n\ndata: [DONE]\n\n")
	}))
	defer provider.Close()
	global := map[string]any{
		"options": map[string]any{"disable_default_providers": true, "auto_lsp": false, "notifications": "disabled"},
		"providers": map[string]any{"web-fixture": map[string]any{
			"type": "openai-compat", "api_key": "web-fixture-key", "base_url": provider.URL + "/v1",
			"models": []map[string]any{{"id": "model-a", "name": "Model A", "context_window": 32768, "default_max_tokens": 512}, {"id": "model-b", "name": "Model B", "context_window": 32768, "default_max_tokens": 512}},
		}},
		"models": map[string]any{"large": map[string]any{"provider": "web-fixture", "model": "model-a", "max_tokens": 512}, "small": map[string]any{"provider": "web-fixture", "model": "model-a", "max_tokens": 512}},
	}
	data, err := json.Marshal(global)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config.GlobalConfig(), data, 0o600))
	workdir := filepath.Join(root, "workspace")
	require.NoError(t, os.MkdirAll(workdir, 0o700))
	store, err := config.Load(config.GlobalWorkspaceDir(), "", false)
	require.NoError(t, err)
	native := server.NewServer(store, "tcp", "127.0.0.1:0")
	native.Backend().SetPersistent(true)
	hs := httptest.NewUnstartedServer(nil)
	hs.Config.Handler = Handler(native, hs.Listener.Addr().String(), Startup{Path: workdir})
	hs.Start()
	defer func() {
		for _, item := range native.Backend().ListWorkspaces() {
			ws, _ := native.Backend().GetWorkspace(item.ID)
			ws.Shutdown()
		}
		hs.Close()
		_ = native.Close()
	}()
	request := func(method, path string, body any, result any) int {
		t.Helper()
		var encoded []byte
		if body != nil {
			encoded, err = json.Marshal(body)
			require.NoError(t, err)
		}
		r, err := http.NewRequest(method, hs.URL+path, bytes.NewReader(encoded))
		require.NoError(t, err)
		r.Header.Set("Origin", hs.URL)
		r.Header.Set("Content-Type", "application/json")
		response, err := hs.Client().Do(r)
		require.NoError(t, err)
		defer response.Body.Close()
		payload, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		if response.StatusCode >= 400 && result != nil {
			t.Fatalf("%s %s: %s", method, path, payload)
		}
		if result != nil && len(payload) > 0 {
			require.NoError(t, json.Unmarshal(payload, result), string(payload))
		}
		return response.StatusCode
	}
	var bootstrap struct {
		Path, SettingsPath string
	}
	var rawBootstrap map[string]any
	require.Equal(t, http.StatusOK, request("GET", "/ui/bootstrap", nil, &rawBootstrap))
	bootstrap.Path, _ = rawBootstrap["path"].(string)
	bootstrap.SettingsPath, _ = rawBootstrap["settings_path"].(string)
	require.Equal(t, workdir, bootstrap.Path)
	require.Equal(t, config.GlobalConfigData(), bootstrap.SettingsPath)

	for _, mutation := range []func(*http.Request){func(r *http.Request) { r.Host = "attacker.invalid" }, func(r *http.Request) { r.Header.Set("Origin", "https://attacker.invalid") }, func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }} {
		r, _ := http.NewRequest("GET", hs.URL+"/ui/bootstrap", nil)
		mutation(r)
		response, err := hs.Client().Do(r)
		require.NoError(t, err)
		require.Equal(t, http.StatusForbidden, response.StatusCode)
		response.Body.Close()
	}

	clientID := uuid.NewString()
	var ws proto.Workspace
	require.Equal(t, http.StatusOK, request("POST", "/v1/workspaces", map[string]any{"path": workdir, "client_id": clientID, "authority_mode": "server"}, &ws))
	require.Equal(t, "model-a", ws.Config.Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "server", ws.Authority.Mode)
	base := "/v1/workspaces/" + ws.ID
	require.Equal(t, http.StatusOK, request("POST", base+"/config/set", proto.ConfigSetRequest{Scope: config.ScopeWorkspace, Key: "options.disabled_tools", Value: []string{"download"}}, nil))
	var configured map[string]any
	require.Equal(t, http.StatusOK, request("GET", base+"/config", nil, &configured))
	require.Equal(t, []any{"download"}, configured["options"].(map[string]any)["disabled_tools"])
	require.Equal(t, http.StatusOK, request("POST", base+"/config/remove", proto.ConfigRemoveRequest{Scope: config.ScopeWorkspace, Key: "options.disabled_tools"}, nil))
	configured = nil
	require.Equal(t, http.StatusOK, request("GET", base+"/config", nil, &configured))
	require.Empty(t, configured["options"].(map[string]any)["disabled_tools"])
	channelURL := "ws" + strings.TrimPrefix(hs.URL, "http") + base + "/channel?client_id=" + clientID
	dialer := websocket.Dialer{Subprotocols: []string{proto.WorkspaceChannelProtocol}}
	channel, _, err := dialer.Dial(channelURL, http.Header{"Origin": {hs.URL}})
	require.NoError(t, err)
	defer channel.Close()
	readEvent := func(kind string, accept func(json.RawMessage) bool) json.RawMessage {
		t.Helper()
		require.NoError(t, channel.SetReadDeadline(time.Now().Add(30*time.Second)))
		for {
			var frame proto.WorkspaceChannelFrame
			require.NoError(t, channel.ReadJSON(&frame))
			var envelope pubsub.Payload
			require.NoError(t, json.Unmarshal(frame.Event, &envelope))
			var event struct {
				Payload json.RawMessage `json:"payload"`
			}
			require.NoError(t, json.Unmarshal(envelope.Payload, &event))
			if envelope.Type == kind && (accept == nil || accept(event.Payload)) {
				return event.Payload
			}
		}
	}
	var session proto.Session
	require.Equal(t, http.StatusOK, request("POST", base+"/sessions", map[string]string{"title": "Web acceptance"}, &session))
	readEvent("session", func(raw json.RawMessage) bool { return bytes.Contains(raw, []byte(session.ID)) })
	filesRoute := "/ui/workspaces/" + ws.ID + "/instructions"
	var instructionFiles []struct {
		Path string `json:"path"`
	}
	require.Equal(t, http.StatusOK, request("GET", filesRoute, nil, &instructionFiles))
	filePath := filepath.Join(workdir, "AGENTS.md")
	require.Contains(t, instructionFiles, struct {
		Path string `json:"path"`
	}{Path: filePath})
	fileRoute := filesRoute + "?path=" + url.QueryEscape(filePath)
	var document instructionDocument
	require.Equal(t, http.StatusOK, request("GET", fileRoute, nil, &document))
	require.False(t, document.Exists)
	require.Equal(t, http.StatusOK, request("PUT", fileRoute, map[string]string{"text": "Native file instructions", "revision": document.Revision}, &document))
	require.Equal(t, "Native file instructions", document.Text)
	require.Equal(t, http.StatusConflict, request("PUT", fileRoute, map[string]string{"text": "Stale file instructions", "revision": ""}, nil))
	require.Equal(t, http.StatusBadRequest, request("GET", filesRoute+"?path="+url.QueryEscape(filepath.Join(workdir, "unconfigured.txt")), nil, nil))

	const sessionPrompt = "Use only this session's literal {{instructions}}."
	instructionUpdate := map[string]any{"instruction_update": map[string]any{"mode": "replace", "text": sessionPrompt, "revision": 0}}
	require.Equal(t, http.StatusOK, request("PUT", base+"/sessions/"+session.ID, instructionUpdate, &session))
	require.Equal(t, sessionPrompt, session.Instructions.Text)
	require.EqualValues(t, 1, session.Instructions.Revision)
	require.Equal(t, http.StatusConflict, request("PUT", base+"/sessions/"+session.ID, instructionUpdate, nil))
	require.Equal(t, http.StatusBadRequest, request("PUT", base+"/sessions/"+session.ID, map[string]any{"instruction_update": map[string]any{"mode": "unknown", "revision": 1}}, nil))
	require.Equal(t, http.StatusOK, request("POST", base+"/agent/init", map[string]bool{"interactive": true}, &struct{}{}))
	var surfaces []proto.ProviderSurface
	request("GET", base+"/providers", nil, &surfaces)
	var selected proto.ProviderSurface
	for _, surface := range surfaces {
		if surface.ID == "web-fixture" {
			selected = surface
		}
	}
	require.NotNil(t, selected.Owner)
	var models config.AgentModelState
	request("POST", base+"/config/model", proto.ConfigModelRequest{Scope: config.ScopeGlobal, ModelType: config.SelectedModelTypeLarge, Model: config.SelectedModel{Provider: "web-fixture", Model: "model-b", MaxTokens: 512}, Owner: *selected.Owner}, &models)
	request("POST", base+"/agent/update", proto.AgentUpdateRequest{State: models}, &struct{}{})
	fresh, err := config.Init(workdir, "", false)
	require.NoError(t, err)
	require.Equal(t, "model-b", fresh.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "model-a", fresh.Config().Models[config.SelectedModelTypeSmall].Model)
	require.Equal(t, "disabled", fresh.Config().Options.Notifications)
	invalid := request("POST", base+"/config/model", proto.ConfigModelRequest{Scope: config.ScopeGlobal, ModelType: config.SelectedModelTypeLarge, Model: config.SelectedModel{Provider: "web-fixture", Model: "missing"}, Owner: *selected.Owner}, nil)
	require.GreaterOrEqual(t, invalid, 400)

	const runID = "web-client-acceptance"
	require.Equal(t, http.StatusAccepted, request("POST", base+"/agent", proto.AgentMessage{SessionID: session.ID, RunID: runID, Prompt: "web-client-acceptance", WaitForAdmission: true}, &struct{}{}))
	var complete proto.RunComplete
	raw := readEvent("run_complete", func(raw json.RawMessage) bool {
		_ = json.Unmarshal(raw, &complete)
		return complete.RunID == runID
	})
	require.Empty(t, complete.Error, string(raw))
	require.Equal(t, "Web client production path completed.", complete.Text)
	var messages []proto.Message
	request("GET", base+"/sessions/"+session.ID+"/messages", nil, &messages)
	require.GreaterOrEqual(t, len(messages), 2)
	var found bool
	for len(observed) > 0 {
		body := <-observed
		encoded, _ := json.Marshal(body["messages"])
		if body["model"] == "model-b" && bytes.Contains(encoded, []byte(runID)) {
			found = true
			systems := []string{}
			for _, item := range body["messages"].([]any) {
				message := item.(map[string]any)
				if message["role"] == "system" || message["role"] == "developer" {
					systems = append(systems, message["content"].(string))
				}
			}
			require.Equal(t, []string{sessionPrompt}, systems)
			require.NotContains(t, string(encoded), "Native file instructions")
		}
	}
	require.True(t, found, "the selected native model must receive the prompt")

	live, err := native.Backend().GetWorkspace(ws.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	permissionResult := make(chan bool, 1)
	go func() {
		granted, _ := live.Permissions.Request(ctx, permission.CreatePermissionRequest{SessionID: session.ID, ToolCallID: "web-tool", ToolName: "bash", Action: "execute", Params: map[string]string{"command": "pwd"}, RequireExplicitApproval: true})
		permissionResult <- granted
	}()
	var pendingPermission proto.PermissionRequest
	require.NoError(t, json.Unmarshal(readEvent("permission_request", nil), &pendingPermission))
	var pending struct {
		Permission *proto.PermissionRequest `json:"permission"`
		Question   *proto.QuestionRequest   `json:"question"`
	}
	request("GET", "/ui/workspaces/"+ws.ID+"/pending", nil, &pending)
	require.Equal(t, pendingPermission.ID, pending.Permission.ID)
	request("POST", base+"/permissions/grant", proto.PermissionGrant{Permission: pendingPermission, Action: proto.PermissionAllow}, &struct{}{})
	require.True(t, <-permissionResult)
	request("GET", "/ui/workspaces/"+ws.ID+"/pending", nil, &pending)
	require.Nil(t, pending.Permission)
	questionDone := make(chan error, 1)
	go func() {
		_, err := live.Questions.Ask(ctx, question.Request{ID: "web-question", SessionID: session.ID, Questions: []question.Question{{ID: "choice", Type: question.TypeFreeText, Text: "Which path?", Description: "Pick a workspace path"}}})
		questionDone <- err
	}()
	readEvent("question_batch_request", nil)
	request("GET", "/ui/workspaces/"+ws.ID+"/pending", nil, &pending)
	require.Equal(t, "web-question", pending.Question.ID)
	var stale proto.QuestionAnswerResponse
	request("POST", base+"/questions/answer", proto.QuestionAnswer{BatchRequestID: "old-question", Responses: []proto.QuestionResponse{{QuestionID: "choice", FillInText: "stale"}}}, &stale)
	require.False(t, stale.Resolved)
	require.NotNil(t, live.Questions.Pending())
	request("POST", base+"/questions/answer", proto.QuestionAnswer{BatchRequestID: "web-question", Responses: []proto.QuestionResponse{{QuestionID: "choice", FillInText: workdir}}}, &struct{}{})
	require.NoError(t, <-questionDone)
	request("GET", "/ui/workspaces/"+ws.ID+"/pending", nil, &pending)
	require.Nil(t, pending.Question)
	go func() {
		_, err := live.Questions.Ask(ctx, question.Request{ID: "native-client-question", Questions: []question.Question{{ID: "native-choice", Type: question.TypeFreeText, Text: "Which path?", Description: "Native client request"}}})
		questionDone <- err
	}()
	readEvent("question_batch_request", nil)
	var nativeAnswer proto.QuestionAnswerResponse
	request("POST", base+"/questions/answer", proto.QuestionAnswer{Responses: []proto.QuestionResponse{{QuestionID: "native-choice", FillInText: workdir}}}, &nativeAnswer)
	require.True(t, nativeAnswer.Resolved)
	require.NoError(t, <-questionDone)

	require.NoError(t, channel.Close())
	reconnected, _, err := dialer.Dial(channelURL, http.Header{"Origin": {hs.URL}})
	require.NoError(t, err)
	defer reconnected.Close()
	var restored proto.Session
	request("GET", fmt.Sprintf("%s/sessions/%s", base, session.ID), nil, &restored)
	require.Equal(t, session.ID, restored.ID)
	require.Equal(t, session.Instructions, restored.Instructions)
}
