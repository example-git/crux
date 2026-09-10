package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type checkedKeySDKWorkspace struct {
	*workspace.ClientWorkspace
	updates []config.AgentModelState
}

func (w *checkedKeySDKWorkspace) AgentIsReady() bool                               { return false }
func (w *checkedKeySDKWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *checkedKeySDKWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *checkedKeySDKWorkspace) UpdateAgentModel(_ context.Context, state config.AgentModelState) error {
	w.updates = append(w.updates, state)
	return nil
}

// Real UI messages call the actual Workspace, SDK, routes, check/save service
// and preferred-model persistence. The agent-update observer records continuation;
// production model inference is tested separately in agent/workspace fixtures.
func TestCheckedKeyUIThroughWorkspaceSDK(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, root)
	}
	var probes, publications atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		require.Equal(t, "/configured/v1/models", r.URL.Path)
		require.Equal(t, "Bearer synthetic-$LITERAL-key", r.Header.Get("Authorization"))
		require.Equal(t, "selected", r.Header.Get("X-Owner"))
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer provider.Close()
	previous := http.DefaultClient
	http.DefaultClient = provider.Client()
	t.Cleanup(func() { http.DefaultClient = previous })
	dataDir, settings, project := filepath.Join(root, "data"), filepath.Join(root, "settings"), filepath.Join(root, "project")
	for _, dir := range []string{dataDir, settings, project} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	document := map[string]any{"providers": map[string]any{"checked": map[string]any{"name": "Checked Provider", "type": "openai-compat", "base_url": provider.URL + "/configured/v1", "api_key": "synthetic-old", "owner": map[string]any{"type": "custom", "construction": "openai-compat"}, "extra_headers": map[string]string{"X-Owner": "selected"}, "models": []map[string]any{{"id": "current", "name": "Current", "context_window": 8192, "default_max_tokens": 1024}, {"id": "next", "name": "Next", "context_window": 8192, "default_max_tokens": 1024}}}}, "models": map[string]any{"large": map[string]any{"provider": "checked", "model": "current"}, "small": map[string]any{"provider": "checked", "model": "current"}}}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	path := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace"), false, env.NewFromMap(map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": root, "CRUX_GLOBAL_CONFIG": settings, "CRUX_GLOBAL_DATA": dataDir, "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated"}))
	require.NoError(t, err)
	store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { publications.Add(1); require.Same(t, snapshot.Config(), store.Config()) }}, nil
	})
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	hostServer := serverForCheckedKey(t, store, a, project)
	rpc := httptest.NewServer(hostServer.Handler())
	t.Cleanup(rpc.Close)
	sdk, err := client.NewClient(root, "tcp", strings.TrimPrefix(rpc.URL, "http://"))
	require.NoError(t, err)
	retained := workspace.NewClientWorkspace(sdk, proto.Workspace{ID: "checked-ui", Path: project, Config: store.Config().RedactedForTransport(), ProviderSurfaces: config.ProviderSurfaces(store.Config())})
	t.Cleanup(retained.Shutdown)
	ws := &checkedKeySDKWorkspace{ClientWorkspace: retained}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.dialog = dialog.NewOverlay()
	ui.header = newHeader(ui.com)
	ui.themeKey = styles.ThemeKeyForProvider("checked")
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	configured, _ := store.Config().Providers.Get("checked")
	owner, ok := ws.Config().ProviderOwner("checked")
	require.True(t, ok)
	selection := dialog.ActionSelectModel{Provider: configured.ToProvider(), Model: config.SelectedModel{Provider: "checked", Model: "next"}, ModelType: config.SelectedModelTypeLarge, ProviderOwner: owner, ProviderOwnerSet: true, ReAuthenticate: true}
	messages := collectCommandMessages(ui.openAuthenticationDialog(selection))
	require.Len(t, messages, 1)
	_, cmd := ui.Update(messages[0])
	collectCommandMessages(cmd)
	d := ui.dialog.Dialog(dialog.APIKeyInputID).(*dialog.APIKeyInput)
	counter := filepath.Join(root, "evaluations")
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' 'synthetic-$LITERAL-key')", counter)
	d.HandleMsg(tea.PasteMsg{Content: source})
	ids := collectCommandMessages(ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
	require.Len(t, ids, 1)
	_, cmd = ui.Update(ids[0])
	messages = collectCommandMessages(cmd)
	require.Len(t, messages, 1)
	checked := messages[0].(apiKeyCheckMsg)
	require.NoError(t, checked.err)
	_, cmd = ui.Update(checked)
	collectCommandMessages(cmd)
	require.Equal(t, "current", store.Config().Models[config.SelectedModelTypeLarge].Model, "check must not select a model")
	d.HandleMsg(tea.PasteMsg{Content: "must-not-replace-source"})
	messages = collectCommandMessages(ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
	require.Len(t, messages, 1)
	saved := messages[0].(apiKeySaveMsg)
	require.NoError(t, saved.err)
	require.Equal(t, "current", store.Config().Models[config.SelectedModelTypeLarge].Model, "save itself preserves model selection")
	ui.handleDialogAction(dialog.ActionClose{})
	_, cmd = ui.Update(saved)
	messages = collectCommandMessages(cmd)
	var completed *modelSelectionCompletedMsg
	for _, msg := range messages {
		if result, ok := msg.(modelSelectionCompletedMsg); ok {
			copy := result
			completed = &copy
		}
	}
	require.NotNil(t, completed)
	require.NoError(t, completed.err)
	_, cmd = ui.Update(*completed)
	collectCommandMessages(cmd)
	require.Equal(t, "next", store.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Equal(t, "next", ws.Config().Models[config.SelectedModelTypeLarge].Model)
	require.Len(t, ws.updates, 1)
	require.Equal(t, "next", ws.updates[0].Large.Model.Model)
	require.Equal(t, owner, ws.updates[0].Large.Owner)
	literal, _ := store.Config().Providers.Get("checked")
	require.Equal(t, "synthetic-$LITERAL-key", literal.APIKey)
	require.Equal(t, source, literal.APIKeyTemplate)
	content, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(content))
	require.EqualValues(t, 1, probes.Load())
	require.EqualValues(t, 1, publications.Load())
	written, err := os.ReadFile(path)
	require.NoError(t, err)
	var persisted config.Config
	require.NoError(t, json.Unmarshal(written, &persisted))
	p, _ := persisted.Providers.Get("checked")
	require.Equal(t, source, p.APIKey)
	require.Equal(t, "next", persisted.Models[config.SelectedModelTypeLarge].Model)
}

func serverForCheckedKey(t *testing.T, store *config.ConfigStore, a *app.App, project string) *server.Server {
	t.Helper()
	s := server.NewServer(store, "unix", "")
	host := &backend.Workspace{ID: "checked-ui", Path: project, App: a, Cfg: store}
	backend.InsertWorkspaceForTest(s.Backend(), host)
	backend.SetWorkspaceShutdownFnForTest(host, func() {})
	return s
}
