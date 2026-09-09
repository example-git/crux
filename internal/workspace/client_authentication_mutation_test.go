package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/agent"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

type clientAuthenticationFixture struct {
	w                                *ClientWorkspace
	connection                       connection.Connection
	s                                *server.Server
	store                            *config.ConfigStore
	owner                            providerregistry.RegistrationOwner
	first, second                    accounts.Entry
	root, path, accountsPath, marker string
	puts                             atomic.Int32
	removes                          atomic.Int32
	removeMode                       atomic.Int32
	requests                         atomic.Int32
	getMode, putMode                 atomic.Int32 // PUT:1 reject,2 lose committed response; GET:1 reject,2 wrong workspace.
	afterPut                         atomic.Pointer[func()]
	afterGet                         atomic.Pointer[func()]
	mu                               sync.Mutex
	credentials                      []string
}

func newClientAuthenticationFixture(t *testing.T, barrier bool) *clientAuthenticationFixture {
	t.Helper()
	f := &clientAuthenticationFixture{root: t.TempDir()}
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
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/auth/remove") {
			f.removes.Add(1)
			if f.removeMode.Load() == 1 {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				if recorder.Code != http.StatusOK {
					t.Errorf("committed removal failed: %d %s", recorder.Code, recorder.Body.String())
				}
				http.Error(w, "synthetic lost removal response", http.StatusBadGateway)
				return
			}
		}
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
	f.connection = connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity}
	c, err := client.NewAuthenticatedClient(t.TempDir(), f.connection)
	require.NoError(t, err)
	c.SetLocalRuntimeStore(f.store)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	f.w = NewClientWorkspace(c, *created)
	t.Cleanup(f.w.Shutdown)
	return f
}

func (f *clientAuthenticationFixture) target(t *testing.T) providerauth.Target {
	t.Helper()
	status, err := f.w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	return providerauth.Target{WorkspaceID: status.WorkspaceID, Generation: status.Generation, Owner: providerauth.PublicOwner(f.owner)}
}
func (f *clientAuthenticationFixture) observed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.credentials)
}
func clientAuthenticationFiles(t *testing.T, paths ...string) ([]os.FileInfo, [][]byte) {
	t.Helper()
	var infos []os.FileInfo
	var bodies [][]byte
	for _, path := range paths {
		info, err := os.Stat(path)
		require.NoError(t, err)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		infos = append(infos, info)
		bodies = append(bodies, data)
	}
	return infos, bodies
}
func requireClientAuthenticationFilesUnchanged(t *testing.T, paths []string, infos []os.FileInfo, bodies [][]byte) {
	t.Helper()
	afterInfos, afterBodies := clientAuthenticationFiles(t, paths...)
	require.Equal(t, bodies, afterBodies)
	for i := range infos {
		require.True(t, os.SameFile(infos[i], afterInfos[i]))
		require.Equal(t, infos[i].ModTime(), afterInfos[i].ModTime())
	}
}

func TestClientAuthenticationMutationTLSAcknowledgedSwitchLogout(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	require.NoError(t, f.w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
	require.NoError(t, err)
	coordinator := receiver.App.CurrentAgentCoordinator()
	old := coordinator.Model()
	call := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("authentication adapter")}}
	_, err = old.Model.Generate(t.Context(), call)
	require.NoError(t, err)
	models := f.store.RuntimeSnapshot().AgentModelState()
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("1", 32), Target: f.target(t), AccountID: f.second.ID}
	outcome, err := f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, outcome.ValidateSwitch(request))
	require.False(t, outcome.Superseded)
	require.True(t, outcome.Progress.AccountsSaved && outcome.Progress.ConfigSaved && outcome.Progress.RuntimePublished)
	require.Equal(t, models, f.store.RuntimeSnapshot().AgentModelState())
	require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
	require.Same(t, f.store.Config(), f.w.Config())
	fresh := coordinator.Model()
	_, err = fresh.Model.Generate(t.Context(), call)
	require.NoError(t, err)
	require.Equal(t, []string{"Bearer " + f.first.AccessToken, "Bearer " + f.second.AccessToken}, f.observed())
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	again, err := f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, outcome, again)
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	again.Change.Models.Large.Model.Model = "caller-mutated"
	again, err = f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, "main", again.Change.Models.Large.Model.Model)
	logout := providerauth.LogoutRequest{OperationID: strings.Repeat("2", 32), Target: f.target(t)}
	loggedOut, err := f.w.logoutClientAuthentication(t.Context(), logout)
	require.NoError(t, err)
	require.NoError(t, loggedOut.ValidateLogout(logout))
	require.True(t, f.w.authority.removed[f.owner])
	require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
	for _, model := range []agent.Model{old, fresh, coordinator.Model()} {
		_, err = model.Model.Generate(t.Context(), call)
		require.Error(t, err)
	}
	require.Len(t, f.observed(), 2, "logout must prevent fresh and retained model requests")
	historical, err := f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	require.True(t, historical.Superseded)
	require.EqualValues(t, 2, f.puts.Load())
	require.NoFileExists(t, f.marker)
}

func TestClientAuthenticationMutationTLSRejectedAndLostAcknowledgement(t *testing.T) {
	for _, mode := range []string{"rejected", "lost", "wrong-get-workspace", "generic-reconcile"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			before := f.w.Config()
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("3", 32), Target: f.target(t), AccountID: f.second.ID}
			if mode == "rejected" {
				f.putMode.Store(1)
			} else {
				f.putMode.Store(2)
				f.getMode.Store(1)
			}
			outcome, err := f.w.switchClientAuthentication(t.Context(), request)
			require.Error(t, err)
			require.True(t, outcome.Progress.AccountsSaved && outcome.Progress.ConfigSaved && outcome.Progress.RuntimePublished)
			require.Same(t, before, f.w.Config())
			require.EqualValues(t, 1, f.w.authority.accepted.Revision)
			paths := []string{f.path, f.accountsPath}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			if mode == "wrong-get-workspace" {
				f.getMode.Store(2)
				_, err = f.w.ProviderAuthentication(t.Context())
				require.ErrorContains(t, err, "changed identity")
				require.Same(t, before, f.w.Config())
				require.EqualValues(t, 1, f.w.authority.accepted.Revision)
			} else {
				f.getMode.Store(0)
			}
			if mode == "generic-reconcile" {
				_, err = f.w.ProviderAuthentication(t.Context())
				require.NoError(t, err)
				require.True(t, f.w.authority.authenticationReceipts[request.OperationID].acknowledged)
			}
			again, err := f.w.switchClientAuthentication(t.Context(), request)
			if mode == "rejected" || mode == "wrong-get-workspace" {
				require.Error(t, err)
				require.Same(t, before, f.w.Config())
				require.Equal(t, outcome.Progress, again.Progress)
				if mode == "rejected" {
					receiver, e := f.s.Backend().GetWorkspace(f.w.workspaceID())
					require.NoError(t, e)
					provider, ok := receiver.Cfg.Config().Providers.Get("copilot")
					require.True(t, ok)
					require.Equal(t, f.first.AccessToken, provider.APIKey)
					f.w.authority.mu.Lock()
					require.NoError(t, f.w.reconcileClientAuthority(t.Context(), f.w.authority))
					f.w.authority.mu.Unlock()
					require.Nil(t, f.w.authority.pending)
					_, e = f.w.switchClientAuthentication(t.Context(), request)
					require.Error(t, e, "clearing generic pending cannot prove authentication acknowledgement")
				}
			} else {
				require.NoError(t, err)
				require.Equal(t, outcome, again)
				require.Same(t, f.store.Config(), f.w.Config())
			}
			require.EqualValues(t, 1, f.puts.Load(), "retry must use GET only")
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
			conflict := request
			conflict.AccountID = f.first.ID
			_, err = f.w.switchClientAuthentication(t.Context(), conflict)
			require.ErrorIs(t, err, providerauth.ErrOperationConflict)
			require.NoFileExists(t, f.marker)
		})
	}
}

func TestClientAuthenticationMutationCollectionRejectsConcurrentChanges(t *testing.T) {
	for _, mode := range []string{"account", "config", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, true)
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("4", 32), Target: f.target(t), AccountID: f.second.ID}
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "block-key"), nil, 0o600))
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			type result struct {
				outcome providerauth.MutationOutcome
				err     error
			}
			done := make(chan result, 1)
			go func() { outcome, err := f.w.switchClientAuthentication(ctx, request); done <- result{outcome, err} }()
			entered := filepath.Join(f.root, "key-entered")
			ticker := time.NewTicker(time.Millisecond)
			defer ticker.Stop()
		waiting:
			for {
				select {
				case <-ticker.C:
					if _, err := os.Stat(entered); err == nil {
						break waiting
					}
				case value := <-done:
					t.Fatalf("collection did not enter key expression: %v", value.err)
				case <-ctx.Done():
					t.Fatal("collection did not enter key expression")
				}
			}
			switch mode {
			case "account":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), f.owner.AccountNamespace, f.first))
			case "config":
				data, err := os.ReadFile(f.path)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(f.path, append(data, '\n'), 0o600))
			case "cancel":
				cancel()
			}
			require.NoError(t, os.WriteFile(filepath.Join(f.root, "key-release"), nil, 0o600))
			select {
			case value := <-done:
				require.Error(t, value.err)
				require.True(t, value.outcome.Progress.RuntimePublished)
				if mode == "cancel" {
					require.ErrorIs(t, value.err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("collection remained blocked")
			}
			require.Zero(t, f.puts.Load())
			require.Nil(t, f.w.authority.pending)
			require.Nil(t, f.w.authority.authenticationReceipts[request.OperationID].proposal)
			paths := []string{f.path, f.accountsPath, entered}
			infos, bodies := clientAuthenticationFiles(t, paths...)
			_, err := f.w.switchClientAuthentication(t.Context(), request)
			require.Error(t, err)
			requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
			require.Zero(t, f.puts.Load())
		})
	}
}

func TestClientAuthenticationMutationHistoricalLocalReceiptIsNotRemoteSuccess(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("5", 32), Target: f.target(t), AccountID: f.second.ID}
	local, err := f.w.authority.providerAuth.SwitchForAccepted(t.Context(), request, f.w.authority.accepted, f.w.Config())
	require.NoError(t, err)
	require.NotNil(t, local.Outcome.Change)
	require.NoError(t, f.store.SetConfigField(config.ScopeGlobal, "options.tui.compact_mode", true))
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	outcome, err := f.w.switchClientAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "historical")
	require.True(t, outcome.Superseded)
	require.True(t, outcome.Progress.RuntimePublished)
	require.Zero(t, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
}

func TestClientAuthenticationMutationRejectedLogoutRetainsExplicitRecoveryIntent(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.LogoutRequest{OperationID: strings.Repeat("7", 32), Target: f.target(t)}
	before := f.w.Config()
	f.putMode.Store(1)
	outcome, err := f.w.logoutClientAuthentication(t.Context(), request)
	require.Error(t, err)
	require.True(t, outcome.Progress.AccountsSaved && outcome.Progress.ConfigSaved && outcome.Progress.RuntimePublished)
	receipt := f.w.authority.authenticationReceipts[request.OperationID]
	require.True(t, receipt.request.logout)
	require.Equal(t, f.owner, receipt.owner)
	require.NotNil(t, receipt.proposal)
	require.True(t, receipt.proposal.Credentials[0].Unavailable)
	require.False(t, f.w.authority.removed[f.owner], "accepted removal state changes only on acknowledgement")
	require.Same(t, before, f.w.Config())
	receiver, err := f.s.Backend().GetWorkspace(f.w.workspaceID())
	require.NoError(t, err)
	provider, ok := receiver.Cfg.Config().Providers.Get("copilot")
	require.True(t, ok)
	require.Equal(t, f.first.AccessToken, provider.APIKey)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	_, err = f.w.logoutClientAuthentication(t.Context(), request)
	require.ErrorContains(t, err, "not acknowledged")
	local, err := f.w.authority.providerAuth.Status(t.Context())
	require.NoError(t, err)
	fresh := request
	fresh.OperationID, fresh.Target.Generation = strings.Repeat("8", 32), local.Generation
	_, err = f.w.logoutClientAuthentication(t.Context(), fresh)
	require.ErrorContains(t, err, "explicit recovery")
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	// An ordinary retry carries no new publication authorization. The separate
	// explicit recovery action uses this exact unavailable proposal and owner
	// intent; a fresh Switch/Logout operation ID is not that recovery action.
}

func TestClientAuthenticationMutationRejectsChangedAuthorityAfterPut(t *testing.T) {
	for _, mode := range []string{"workspace", "principal", "lifetime"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("9", 32), Target: f.target(t), AccountID: f.second.ID}
			before := f.w.Config()
			change := func() {
				if mode == "lifetime" {
					f.w.subCancel()
					return
				}
				f.w.mu.Lock()
				defer f.w.mu.Unlock()
				if mode == "workspace" {
					f.w.ws.ID = "replacement-workspace"
				} else {
					copy := *f.w.ws.Authority
					copy.Principal = "replacement-principal"
					f.w.ws.Authority = &copy
				}
			}
			f.afterPut.Store(&change)
			outcome, err := f.w.switchClientAuthentication(t.Context(), request)
			require.Error(t, err)
			require.True(t, outcome.Progress.RuntimePublished)
			require.Same(t, before, f.w.Config())
			require.EqualValues(t, 1, f.w.authority.accepted.Revision)
			require.EqualValues(t, 1, f.puts.Load())
			if mode == "lifetime" {
				require.ErrorIs(t, err, context.Canceled)
			}
		})
	}
}

func TestClientAuthenticationMutationGenericReconcileRejectsChangedCache(t *testing.T) {
	for _, mode := range []string{"workspace", "principal", "authority"} {
		t.Run(mode, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.LogoutRequest{OperationID: strings.Repeat("c", 32), Target: f.target(t)}
			before := f.w.Config()
			f.putMode.Store(2)
			f.getMode.Store(1)
			_, err := f.w.logoutClientAuthentication(t.Context(), request)
			require.Error(t, err)
			receipt := f.w.authority.authenticationReceipts[request.OperationID]
			require.False(t, receipt.acknowledged)
			pending := f.w.authority.pending
			change := func() {
				f.w.mu.Lock()
				defer f.w.mu.Unlock()
				if mode == "workspace" {
					f.w.ws.ID = "different-workspace"
					return
				}
				copy := *f.w.ws.Authority
				if mode == "principal" {
					copy.Principal = "different-principal"
				} else {
					copy.Revision += 10
					copy.Digest = "different-digest"
				}
				f.w.ws.Authority = &copy
			}
			f.afterGet.Store(&change)
			f.getMode.Store(0)
			_, err = f.w.ProviderAuthentication(t.Context())
			require.Error(t, err)
			require.Same(t, before, f.w.Config())
			require.EqualValues(t, 1, f.w.authority.accepted.Revision)
			require.Same(t, pending, f.w.authority.pending)
			require.False(t, receipt.acknowledged)
			require.False(t, f.w.authority.removed[f.owner])
			require.EqualValues(t, 1, f.puts.Load())
		})
	}
}

func TestClientAuthenticationMutationEvictedTargetCannotRecollect(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("a", 32), Target: f.target(t), AccountID: f.second.ID}
	_, err := f.w.switchClientAuthentication(t.Context(), request)
	require.NoError(t, err)
	current := f.target(t)
	for i := 0; i < clientAuthenticationReceiptLimit; i++ {
		_, err = f.w.switchClientAuthentication(t.Context(), providerauth.SwitchRequest{OperationID: fmt.Sprintf("%032x", i+1000), Target: current, AccountID: "not-an-account"})
		require.ErrorIs(t, err, providerauth.ErrAccount)
	}
	require.NotContains(t, f.w.authority.authenticationReceipts, request.OperationID)
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	_, err = f.w.switchClientAuthentication(t.Context(), request)
	require.ErrorIs(t, err, providerauth.ErrStale)
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	_, err = f.w.switchClientAuthentication(t.Context(), providerauth.SwitchRequest{OperationID: strings.Repeat("b", 32), Target: current, AccountID: f.second.ID})
	require.NoError(t, err, "bounded retention must not veto a valid current target")
	require.EqualValues(t, 2, f.puts.Load())
}

func TestClientAuthenticationMutationCancellationAndRetainedPrivacy(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	request := providerauth.SwitchRequest{OperationID: strings.Repeat("6", 32), Target: f.target(t), AccountID: f.second.ID}
	paths := []string{f.path, f.accountsPath}
	infos, bodies := clientAuthenticationFiles(t, paths...)
	for _, mode := range []string{"request", "lifetime"} {
		f.w.authority.mu.Lock()
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { _, err := f.w.switchClientAuthentication(ctx, request); done <- err }()
		select {
		case err := <-done:
			t.Fatalf("mutation bypassed authority mutex: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if mode == "request" {
			cancel()
		} else {
			f.w.subCancel()
		}
		select {
		case err := <-done:
			require.ErrorIs(t, err, context.Canceled)
		case <-time.After(time.Second):
			t.Fatal("canceled mutation waited for authority mutex")
		}
		cancel()
		f.w.authority.mu.Unlock()
	}
	require.Zero(t, f.puts.Load())
	require.Empty(t, f.w.authority.authenticationReceipts)
	requireClientAuthenticationFilesUnchanged(t, paths, infos, bodies)
	receipt := clientAuthenticationReceipt{principal: "private-principal", proposal: &config.RemoteRuntimeProposal{Credentials: []config.RemoteCredentialBinding{{APIKey: "private-credential"}}}}
	_, err := json.Marshal(receipt)
	require.Error(t, err)
	for _, format := range []string{"%v", "%+v", "%#v"} {
		require.NotContains(t, fmt.Sprintf(format, receipt), "private-credential")
	}
	a := &clientAuthority{}
	for i := 0; i < clientAuthenticationReceiptLimit+1; i++ {
		a.retainClientAuthentication(&clientAuthenticationReceipt{request: clientAuthenticationRequest{operationID: fmt.Sprintf("%032x", i)}})
	}
	require.Len(t, a.authenticationReceipts, clientAuthenticationReceiptLimit)
	require.Len(t, a.authenticationReceiptIDs, clientAuthenticationReceiptLimit)
	require.NotContains(t, a.authenticationReceipts, strings.Repeat("0", 32))
}
