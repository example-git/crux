package localaddon

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/server"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCodexConfigBatchWriteNativeStdioPersistence(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	global := filepath.Join(root, "global")
	require.NoError(t, os.MkdirAll(global, 0o700))
	document := []byte(`{"options":{"disable_default_providers":true},"providers":{"fixture":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-settings-key","models":[{"id":"model","default_max_tokens":100}]}},"models":{"large":{"provider":"fixture","model":"model"},"small":{"provider":"fixture","model":"model"}}}`)
	require.NoError(t, os.WriteFile(filepath.Join(global, "crux.json"), document, 0o600))
	base := env.NewFromMap(map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": global, "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileCoreOnly)})
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, base)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	nativeApp := app.NewForTest(ctx)
	defer nativeApp.ShutdownForTest()
	nativeServer := server.NewServer(store, "unix", "")
	nativeServer.Backend().SetIdleShutdownDelay(time.Hour)
	defer nativeServer.Backend().Shutdown()
	workspace := &backend.Workspace{ID: uuid.NewString(), Path: root, App: nativeApp, Cfg: store}
	backend.SetWorkspaceShutdownFnForTest(workspace, func() {})
	backend.InsertWorkspaceForTest(nativeServer.Backend(), workspace)
	socketDir, err := os.MkdirTemp("", "crx-settings-")
	require.NoError(t, err)
	defer func() { require.NoError(t, os.RemoveAll(socketDir)) }()
	socketPath := filepath.Join(socketDir, "native.sock")
	listener, err := (&net.ListenConfig{}).Listen(ctx, "unix", socketPath)
	require.NoError(t, err)
	httpServer := &http.Server{Handler: nativeServer.Handler()}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(listener) }()
	defer func() {
		require.NoError(t, httpServer.Close())
		require.ErrorIs(t, <-served, http.ErrServerClosed)
	}()
	previous := codexClientFactory
	codexClientFactory = func(path string) (*client.Client, error) { return client.NewClient(path, "unix", socketPath) }
	defer func() { codexClientFactory = previous }()
	inputReader, inputWriter := io.Pipe()
	outputReader, outputWriter := io.Pipe()
	defer inputWriter.Close()
	defer outputReader.Close()
	done := make(chan error, 1)
	request := compatibility.Request{Source: "codex", Protocol: compatibility.ProtocolCodexAppServer, Style: compatibility.ExecutionHeadless, WorkingDir: root, Prompt: compatibility.Prompt{Source: compatibility.PromptStreamJSON, Stdin: inputReader}, Session: compatibility.Session{Mode: compatibility.SessionNew, Persistent: true}}
	go func() {
		done <- runCodexAppServer(ctx, protocolInvocation("unused", root, inputReader, outputWriter), request)
		_ = outputWriter.Close()
	}()
	encoder, decoder := json.NewEncoder(inputWriter), json.NewDecoder(outputReader)
	call := func(method string, params any) map[string]any {
		t.Helper()
		require.NoError(t, encoder.Encode(map[string]any{"id": "settings-request", "method": method, "params": params}))
		var response map[string]any
		require.NoError(t, decoder.Decode(&response))
		require.Equal(t, "settings-request", response["id"])
		return response
	}
	require.NotContains(t, call("initialize", map[string]any{"clientInfo": map[string]any{"name": "desktop-settings-test", "version": "1"}}), "error")
	require.NoError(t, encoder.Encode(map[string]any{"method": "initialized"}))
	edits := []any{map[string]any{"keyPath": "desktop.selected-avatar-id", "value": "codex", "mergeStrategy": "replace"}, map[string]any{"keyPath": "desktop.workspace", "value": map[string]any{"collapsed": true, "width": 320}, "mergeStrategy": "replace"}}
	response := call("config/batchWrite", map[string]any{"edits": edits, "filePath": nil, "expectedVersion": nil, "reloadUserConfig": true})
	require.NotContains(t, response, "error")
	written := response["result"].(map[string]any)
	require.Equal(t, "ok", written["status"])
	require.Equal(t, filepath.Join(root, "data", "crux.json"), written["filePath"])
	read := call("config/read", map[string]any{"cwd": nil, "includeLayers": false})
	require.NotContains(t, read, "error")
	view := read["result"].(map[string]any)
	settings := view["config"].(map[string]any)["desktop"].(map[string]any)
	require.Equal(t, "codex", settings["selected-avatar-id"])
	require.Equal(t, map[string]any{"collapsed": true, "width": float64(320)}, settings["workspace"])
	require.Equal(t, written["version"], view["origins"].(map[string]any)["desktop.workspace"].(map[string]any)["version"])
	require.Equal(t, "FIXTURE-MODEL", view["config"].(map[string]any)["model"])
	require.Equal(t, "crux", view["config"].(map[string]any)["model_provider"])
	before, err := os.ReadFile(written["filePath"].(string))
	require.NoError(t, err)
	failed := call("config/batchWrite", map[string]any{"edits": edits, "expectedVersion": "sha256:stale"})["error"].(map[string]any)
	require.Equal(t, "configVersionConflict", failed["data"].(map[string]any)["config_write_error_code"])
	failed = call("config/batchWrite", map[string]any{"edits": edits, "filePath": filepath.Join(root, "outside.json")})["error"].(map[string]any)
	require.Equal(t, "configLayerReadonly", failed["data"].(map[string]any)["config_write_error_code"])
	failed = call("config/batchWrite", map[string]any{"edits": []any{edits[0], map[string]any{"keyPath": "model", "value": "other", "mergeStrategy": "replace"}}})["error"].(map[string]any)
	require.Equal(t, "configValidationError", failed["data"].(map[string]any)["config_write_error_code"])
	after, err := os.ReadFile(written["filePath"].(string))
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, "fixture", store.Config().Models[config.SelectedModelTypeLarge].Provider)
	require.Equal(t, "model", store.Config().Models[config.SelectedModelTypeLarge].Model)
	globalAfter, err := os.ReadFile(filepath.Join(global, "crux.json"))
	require.NoError(t, err)
	require.Equal(t, document, globalAfter)
	require.NoError(t, inputWriter.Close())
	require.NoError(t, <-done)
	restarted, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, base)
	require.NoError(t, err)
	restored, err := restarted.ReadConfigView(t.Context(), config.ScopeGlobal)
	require.NoError(t, err)
	require.Equal(t, settings, restored.Desktop)
}
