package model

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/example-git/crux/foundation/bubbletea"
	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authenticationSDKUIWorkspace struct {
	*workspace.ClientWorkspace
	publications int
}

func (w *authenticationSDKUIWorkspace) AgentIsReady() bool                               { return false }
func (w *authenticationSDKUIWorkspace) PermissionSkipRequests() bool                     { return false }
func (w *authenticationSDKUIWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo { return nil }
func (w *authenticationSDKUIWorkspace) UpdateAgentModel(context.Context, config.AgentModelState) error {
	w.publications++
	return nil
}

// This uses the actual UI actions, Workspace adapter, SDK, server route and
// authentication transaction. It observes durable credential changes and exact
// selected-model preservation; provider inference is covered by workspace TLS
// acceptance tests, not asserted by this UI fixture.
func TestAuthenticationUIThroughWorkspaceSDKSwitchAndLogout(t *testing.T) {
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated", "CRUX_DISABLE_AUTO_MEMORY": "true"}
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_DATA"], 0700))
	first := accounts.Entry{ID: "first", DisplayName: "First", AccessToken: "synthetic-first", RefreshToken: "synthetic-refresh-first", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	second := accounts.Entry{ID: "second", DisplayName: "Second", AccessToken: "synthetic-second", RefreshToken: "synthetic-refresh-second", ExpiresAt: first.ExpiresAt}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), accounts.ProviderCodex, second))
	models := map[string]any{"large": map[string]any{"provider": "codex", "model": "retained-main", "max_tokens": 123}, "small": map[string]any{"provider": "codex", "model": "retained-main", "max_tokens": 45}}
	document := map[string]any{"providers": map[string]any{"codex": map[string]any{"api_key": first.AccessToken, "oauth": first.Token(), "models": []map[string]any{{"id": "retained-main", "name": "Retained Main", "context_window": 8192, "default_max_tokens": 1024}}}}, "models": models}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json"), data, 0600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	selected := store.Config().Models
	a := app.NewForTest(t.Context())
	t.Cleanup(a.ShutdownForTest)
	hostServer := server.NewServer(store, "unix", "")
	host := &backend.Workspace{ID: "auth-ui", Path: t.TempDir(), App: a, Cfg: store}
	backend.InsertWorkspaceForTest(hostServer.Backend(), host)
	backend.SetWorkspaceShutdownFnForTest(host, func() {})
	remote := httptest.NewServer(hostServer.Handler())
	t.Cleanup(remote.Close)
	sdk, err := client.NewClient(t.TempDir(), "tcp", strings.TrimPrefix(remote.URL, "http://"))
	require.NoError(t, err)
	retained := workspace.NewClientWorkspace(sdk, proto.Workspace{ID: host.ID, Path: host.Path, Config: store.Config().RedactedForTransport(), ProviderSurfaces: config.ProviderSurfaces(store.Config())})
	t.Cleanup(retained.Shutdown)
	ws := &authenticationSDKUIWorkspace{ClientWorkspace: retained}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	ui.dialog = dialog.NewOverlay()
	for _, logout := range []bool{false, true} {
		messages := collectCommandMessages(ui.openAuthenticationAccounts(logout))
		require.Len(t, messages, 1)
		loaded := messages[0].(authenticationLoadedMsg)
		require.NoError(t, loaded.err)
		_, cmd := ui.Update(loaded)
		collectCommandMessages(cmd)
		id := dialog.AccountSwitcherID
		if logout {
			id = dialog.LogoutID
		}
		d := ui.dialog.Dialog(id).(interface {
			AuthenticationState() *dialog.AccountAuthentication
		}).AuthenticationState()
		var action dialog.ActionAuthenticationSelect
		for i := 0; i < len(loaded.rows); i++ {
			candidate, ok := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionAuthenticationSelect)
			require.True(t, ok)
			if candidate.Row.Target.Owner.ProviderID == "codex" && (logout || candidate.Row.AccountID == "second") {
				action = candidate
				break
			}
			d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
		}
		require.NotNil(t, action.Dialog, "the requested host row must be selectable")
		prepared := collectCommandMessages(ui.handleDialogAction(action))
		require.Len(t, prepared, 1)
		_, cmd = ui.Update(prepared[0])
		var completed authenticationCompletedMsg
		for _, msg := range collectCommandMessages(cmd) {
			if value, ok := msg.(authenticationCompletedMsg); ok {
				completed = value
			}
		}
		require.NotNil(t, completed.operation)
		require.NoError(t, completed.err)
		require.NotNil(t, completed.outcome.Change)
		require.Equal(t, action.Row.Target, completed.outcome.Previous)
		ui.dialog.CloseDialog(id)
		_, cmd = ui.Update(completed)
		messages = collectCommandMessages(cmd)
		provider, ok := store.Config().Providers.Get("codex")
		require.True(t, ok, "logout preserves configured membership")
		if logout {
			require.Empty(t, provider.APIKey)
			require.Nil(t, provider.OAuthToken)
			require.Contains(t, authenticationInfo(messages), "Logged out")
			saved, err := accounts.List(t.Context(), accounts.ProviderCodex)
			require.NoError(t, err)
			require.Empty(t, saved)
		} else {
			require.Equal(t, "synthetic-second", provider.APIKey)
			require.Contains(t, authenticationInfo(messages), "Switched account to Second")
			active, err := accounts.Active(t.Context(), accounts.ProviderCodex)
			require.NoError(t, err)
			require.Equal(t, "second", active.ID)
		}
		require.Equal(t, selected, store.Config().Models)
		require.Equal(t, selected, ws.Config().Models)
		require.Zero(t, ws.publications, "UI must not republish the acknowledged authentication runtime")
	}
}
