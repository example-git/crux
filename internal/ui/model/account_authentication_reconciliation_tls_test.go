package model

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"encoding/json"
	"fmt"
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type authenticationReconciliationTLSFixture struct {
	w                                *workspace.ClientWorkspace
	id                               string
	s                                *server.Server
	store                            *config.ConfigStore
	owner                            providerregistry.RegistrationOwner
	first, second                    accounts.Entry
	root, path, accountsPath, marker string
	puts                             atomic.Int32
	requests                         atomic.Int32
	getMode, putMode                 atomic.Int32 // PUT:1 reject,2 lose committed response; GET:1 reject,2 wrong workspace.
	afterPut                         atomic.Pointer[func()]
	afterGet                         atomic.Pointer[func()]
	mu                               sync.Mutex
	credentials                      []string
}

// Usage is an ancillary UI refresh; provider execution and authentication use
// the real workspace. Keep this fixture's quota fetch off external services.
type authenticationReconciliationTLSWorkspace struct{ *authenticationSDKUIWorkspace }

func (w *authenticationReconciliationTLSWorkspace) PrepareProviderUsage(providerregistry.RegistrationOwner) oauthusage.Request {
	return func(context.Context) (*oauthusage.Usage, error) { return nil, nil }
}

func newAuthenticationReconciliationTLSFixture(t *testing.T, barrier bool) *authenticationReconciliationTLSFixture {
	t.Helper()
	f := &authenticationReconciliationTLSFixture{root: t.TempDir()}
	values := map[string]string{"HOME": f.root, "USERPROFILE": f.root, "AI_CLI_DIR": filepath.Join(f.root, "server-auth"), "CRUX_GLOBAL_CONFIG": filepath.Join(f.root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(f.root, "data"), "CRUX_CACHE_DIR": filepath.Join(f.root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated", "CRUX_DISABLE_AUTO_MEMORY": "true", "AUTH_LITERAL": "must-not-expand"}
	for name, value := range values {
		t.Setenv(name, value)
	}
	for _, dir := range []string{"config", "data", "workspace"} {
		require.NoError(t, os.MkdirAll(filepath.Join(f.root, dir), 0o700))
	}
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("authentication-owner")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "authentication-owner", identity.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	f.s = server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, f.s.EnableNetworkAuth(t.Context()))
	handler := f.s.Handler()
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
			f.puts.Add(1)
			switch f.putMode.Load() {
			case 1:
				http.Error(w, "synthetic rejection", http.StatusBadRequest)
				return
			case 2:
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusOK {
					t.Errorf("committed PUT failed: %d %s", recorder.Code, recorder.Body.String())
				}
				if change := f.afterPut.Load(); change != nil {
					(*change)()
				}
				http.Error(w, "synthetic lost response", http.StatusBadGateway)
				return
			}
			if change := f.afterPut.Load(); change != nil {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				(*change)()
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
		}
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/workspaces/") {
			switch f.getMode.Load() {
			case 1:
				http.Error(w, "synthetic GET unavailable", http.StatusBadGateway)
				return
			case 2:
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				var response proto.Workspace
				if json.Unmarshal(recorder.Body.Bytes(), &response) != nil {
					t.Error("cannot decode workspace fixture")
					return
				}
				response.ID = "different-workspace"
				_ = json.NewEncoder(w).Encode(response)
				return
			}
			if change := f.afterGet.Load(); change != nil {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				(*change)()
				for key, values := range recorder.Header() {
					w.Header()[key] = values
				}
				w.WriteHeader(recorder.Code)
				_, _ = w.Write(recorder.Body.Bytes())
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	remote.TLS = tlsConfig
	remote.StartTLS()
	t.Cleanup(func() { remote.Close(); _ = f.s.Close() })
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.credentials = append(f.credentials, r.Header.Get("Authorization"))
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","object":"chat.completion","model":"main","choices":[{"message":{"role":"assistant","content":"accepted"},"finish_reason":"stop"}]}`))
	}))
	t.Cleanup(provider.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = provider.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	values["AI_CLI_DIR"] = filepath.Join(f.root, "client-accounts")
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	f.accountsPath = filepath.Join(values["AI_CLI_DIR"], "accounts.json")
	f.marker = filepath.Join(f.root, "token-must-not-execute")
	f.first = accounts.Entry{ID: "first", AccessToken: "synthetic-first", RefreshToken: "synthetic-refresh-first", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	f.second = accounts.Entry{ID: "second", AccessToken: "literal-$(touch '" + f.marker + "')-$AUTH_LITERAL", RefreshToken: "synthetic-refresh-second", ExpiresAt: f.first.ExpiresAt}
	registry, err := providerregistry.New(providerregistry.Integrated()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("copilot")
	require.True(t, ok)
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, f.first))
	require.NoError(t, accounts.SaveWithoutActivating(t.Context(), registration.AccountNamespace, f.second))
	providers := map[string]any{"copilot": map[string]any{"base_url": provider.URL + "/v1", "models": []map[string]any{{"id": "main", "context_window": 8192, "default_max_tokens": 128}, {"id": "small", "context_window": 8192, "default_max_tokens": 64}}, "extra_headers": map[string]string{"X-Keep": "retained"}}}
	smallProvider, smallModel := "copilot", "small"
	if barrier {
		key := fmt.Sprintf("$(if [ -f '%s' ]; then printf x >> '%s'; while [ ! -f '%s' ]; do :; done; fi; printf synthetic-other)", filepath.ToSlash(filepath.Join(f.root, "block-key")), filepath.ToSlash(filepath.Join(f.root, "key-entered")), filepath.ToSlash(filepath.Join(f.root, "key-release")))
		providers["other"] = map[string]any{"type": "openai-compat", "base_url": provider.URL + "/v1", "api_key": key, "models": []map[string]any{{"id": "other", "context_window": 8192}}}
		smallProvider, smallModel = "other", "other"
	}
	source, err := json.Marshal(map[string]any{"providers": providers, "models": map[string]any{"large": map[string]any{"provider": "copilot", "model": "main", "max_tokens": 91}, "small": map[string]any{"provider": smallProvider, "model": smallModel, "max_tokens": 37}}, "options": map[string]any{"notifications": "disabled", "disable_auto_summarize": true}, "tools": map[string]any{"codebase_search": map[string]any{"enabled": false}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.root, "crux.json"), source, 0o600))
	f.path = filepath.Join(f.root, "data", "crux.json")
	credentials, err := json.Marshal(map[string]any{"providers": map[string]any{"copilot": map[string]any{"api_key": f.first.AccessToken, "oauth": f.first.Token()}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(f.path, credentials, 0o600))
	f.store, err = config.LoadIsolated(f.root, filepath.Join(f.root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	f.owner, ok = f.store.RuntimeSnapshot().ProviderOwner("copilot")
	require.True(t, ok)
	proposal, err := f.store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	c.SetLocalRuntimeStore(f.store)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	f.w = workspace.NewClientWorkspace(c, *created)
	f.id = created.ID
	t.Cleanup(f.w.Shutdown)
	return f
}

func TestAuthenticationUIReconciliationThroughTLS(t *testing.T) {
	for _, effect := range []string{"switch", "saved-account", "partial-logout", "lost-apply"} {
		t.Run(effect, func(t *testing.T) {
			f := newAuthenticationReconciliationTLSFixture(t, false)
			require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
			receiver, err := f.s.Backend().GetWorkspace(f.id)
			require.NoError(t, err)
			retained := receiver.App.CurrentAgentCoordinator().Model()
			call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("reviewed authentication UI")}}
			_, err = retained.Model.Generate(t.Context(), call)
			require.NoError(t, err)
			ws := &authenticationReconciliationTLSWorkspace{authenticationSDKUIWorkspace: &authenticationSDKUIWorkspace{ClientWorkspace: f.w}}
			ui := newTestUI()
			ui.com.Workspace = ws
			ui.focus = uiFocusNone
			ui.agentBusyCache.set(false)
			ui.yoloCache.set(false)
			ui.lspCheckedAt = time.Now()
			ui.dialog = dialog.NewOverlay()
			logout := effect == "partial-logout"
			messages := collectCommandMessages(ui.openAuthenticationAccounts(logout))
			require.Len(t, messages, 1)
			loaded := messages[0].(authenticationLoadedMsg)
			require.NoError(t, loaded.err)
			_, cmd := ui.Update(loaded)
			collectCommandMessages(cmd)
			pickerID := dialog.AccountSwitcherID
			if logout {
				pickerID = dialog.LogoutID
			}
			picker := ui.dialog.Dialog(pickerID).(interface {
				AuthenticationState() *dialog.AccountAuthentication
			}).AuthenticationState()
			var action dialog.ActionAuthenticationSelect
			for range loaded.rows {
				candidate := picker.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionAuthenticationSelect)
				if candidate.Row.Target.Owner.ProviderID == "copilot" && (logout || candidate.Row.AccountID == f.second.ID) {
					action = candidate
					break
				}
				picker.HandleMsg(tea.KeyPressMsg{Code: tea.KeyDown})
			}
			require.NotNil(t, action.Dialog)
			if logout {
				f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
					return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {
						data, err := os.ReadFile(f.path)
						require.NoError(t, err)
						require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0600))
					}}, nil
				})
			} else {
				f.putMode.Store(1)
			}
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
			require.Error(t, completed.err)
			require.True(t, completed.outcome.Progress.RuntimePublished)
			if logout {
				require.Nil(t, completed.outcome.Change)
			}
			f.store.SetRuntimeGenerationPreparer(nil)
			f.putMode.Store(0)
			_, cmd = ui.Update(completed)
			collectCommandMessages(cmd)
			originalID, originalMessage := completed.operation.id, completed.operation.message
			if effect == "saved-account" {
				capture, err := f.store.CaptureAuthentication(t.Context())
				require.NoError(t, err)
				_, err = f.store.SwitchAuthenticationAccount(t.Context(), config.ScopeGlobal, capture, f.owner, f.first.ID)
				require.NoError(t, err)
			}
			if logout {
				selected := f.store.RuntimeSnapshot().AgentModelState().Large.Model
				selected.MaxTokens++
				_, err = f.store.OverrideModelsForOwners(config.AgentModelState{Large: &config.OwnedSelectedModel{Owner: f.owner, Model: selected}})
				require.NoError(t, err)
			}
			models := f.store.Config().Models
			configBefore, err := os.ReadFile(f.path)
			require.NoError(t, err)
			accountsBefore, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			reviewDialog := openAuthenticationReviewUI(t, ui, picker)
			// Run through dialog action -> Update preparation -> command -> Update
			// completion. Neither the UI nor this fixture calls the private ledger.
			run := func(key tea.KeyPressMsg) tea.Msg {
				messages := collectCommandMessages(ui.handleDialogAction(reviewDialog.HandleMsg(key)))
				require.Len(t, messages, 1)
				if _, ok := messages[0].(authenticationReconciliationPreparedMsg); ok {
					_, cmd := ui.Update(messages[0])
					messages = collectCommandMessages(cmd)
				}
				require.Len(t, messages, 1)
				return messages[0]
			}
			reviewed := run(tea.KeyPressMsg{Code: tea.KeyEnter}).(authenticationReviewCompletedMsg)
			if effect == "saved-account" {
				require.ErrorIs(t, reviewed.err, config.ErrAuthenticationReconciliationConflict)
				_, cmd = ui.Update(reviewed)
				collectCommandMessages(cmd)
				ui.handleDialogAction(reviewDialog.HandleMsg(tea.KeyPressMsg{Code: '2', Mod: tea.ModAlt}))
				for range len(loaded.rows) + 1 {
					if reviewDialog.Choice().AccountID == f.first.ID {
						break
					}
					ui.handleDialogAction(reviewDialog.HandleMsg(tea.KeyPressMsg{Code: tea.KeyTab}))
				}
				require.Equal(t, f.first.ID, reviewDialog.Choice().AccountID)
				reviewed = run(tea.KeyPressMsg{Code: tea.KeyEnter}).(authenticationReviewCompletedMsg)
			}
			require.NoError(t, reviewed.err)
			_, cmd = ui.Update(reviewed)
			collectCommandMessages(cmd)
			if effect == "lost-apply" {
				f.putMode.Store(2)
				loseGet := func() { f.getMode.Store(1) }
				f.afterPut.Store(&loseGet)
			}
			applied := run(tea.KeyPressMsg{Code: 'y', Mod: tea.ModCtrl}).(authenticationApplyCompletedMsg)
			if effect == "lost-apply" {
				require.Error(t, applied.err)
				require.False(t, applied.outcome.Adopted)
				_, cmd = ui.Update(applied)
				collectCommandMessages(cmd)
				ui.dialog.CloseDialog(reviewDialog.ID())
				f.afterPut.Store(nil)
				f.getMode.Store(0)
				reviewDialog = openAuthenticationReviewUI(t, ui, picker)
				originalApply := applied.request
				applied = run(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl}).(authenticationApplyCompletedMsg)
				require.Equal(t, originalApply, applied.request)
				require.EqualValues(t, 2, f.puts.Load(), "one original rejected PUT and one reviewed PUT; UI retry is GET-only")
			}
			require.NoError(t, applied.err)
			require.True(t, applied.outcome.RemoteAcknowledged && applied.outcome.Adopted)
			ui.dialog.CloseDialog(reviewDialog.ID())
			_, cmd = ui.Update(applied)
			completionMessages := collectCommandMessages(cmd)
			require.Contains(t, authenticationInfo(completionMessages), "original operation result remains unchanged")
			var refreshed *authenticationLoadedMsg
			for _, message := range completionMessages {
				if value, ok := message.(authenticationLoadedMsg); ok {
					refreshed = &value
					_, follow := ui.Update(value)
					collectCommandMessages(follow)
				}
			}
			require.NotNil(t, refreshed, "adoption refreshes only the initiating account picker")
			require.NoError(t, refreshed.err)
			if !logout {
				require.NotEmpty(t, refreshed.rows)
				require.NotEqual(t, action.Row.Target.Generation, refreshed.rows[0].Target.Generation)
				_, err = f.w.ProviderAccounts(t.Context(), refreshed.rows[0].Target)
				require.NoError(t, err, "the next account row is current and usable")
			} else {
				require.Empty(t, refreshed.rows)
			}
			require.Equal(t, originalID, completed.operation.id)
			require.Equal(t, originalMessage, completed.operation.message)
			require.False(t, completed.operation.blockNew)
			require.Zero(t, ws.publications)
			receiver, err = f.s.Backend().GetWorkspace(f.id)
			require.NoError(t, err)
			require.Equal(t, models, receiver.Cfg.Config().Models)
			current := receiver.App.CurrentAgentCoordinator().Model()
			_, err = current.Model.Generate(t.Context(), call)
			if logout {
				require.Error(t, err)
				_, err = retained.Model.Generate(t.Context(), call)
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			f.mu.Lock()
			observed := append([]string(nil), f.credentials...)
			f.mu.Unlock()
			want := []string{"Bearer " + f.first.AccessToken}
			if effect == "switch" || effect == "lost-apply" {
				want = append(want, "Bearer "+f.second.AccessToken)
			} else if effect == "saved-account" {
				want = append(want, "Bearer "+f.first.AccessToken)
			}
			require.Equal(t, want, observed)
			require.NoFileExists(t, f.marker)
			configAfter, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, configBefore, configAfter)
			accountsAfter, err := os.ReadFile(f.accountsPath)
			require.NoError(t, err)
			require.Equal(t, accountsBefore, accountsAfter)
		})
	}
}
