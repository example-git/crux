package workspace_test

import (
	tea "charm.land/bubbletea/v2"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestClientAuthorityTransactionsThroughTLS(t *testing.T) {
	xdgIsolate(t)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("transaction-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "transaction-client", identity.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	var loseAck, reject atomic.Bool
	var puts atomic.Int32
	hs := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
			puts.Add(1)
			if reject.Load() {
				http.Error(w, "synthetic rejection", http.StatusBadRequest)
				return
			}
			if loseAck.Swap(false) {
				recorder := httptest.NewRecorder()
				s.Handler().ServeHTTP(recorder, r)
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
				http.Error(w, "synthetic response lost after commit", http.StatusBadGateway)
				return
			}
		}
		s.Handler().ServeHTTP(w, r)
	}))
	hs.TLS = tlsConfig
	hs.StartTLS()
	t.Cleanup(func() { hs.Close(); _ = s.Close() })

	// Only this client configuration is persisted by the UI transaction. The
	// server has already loaded its identity and compiles detached runtimes.
	clientConfig := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", clientConfig)
	t.Setenv("CRUX_GLOBAL_DATA", t.TempDir())
	configFile := filepath.Join(clientConfig, "crux.json")
	require.NoError(t, os.WriteFile(configFile, []byte(`{
	 "providers":{"transaction-provider":{"id":"transaction-provider","name":"Client","type":"openai-compat","api_key":"synthetic-first","base_url":"https://client.invalid/v1","models":[{"id":"one","name":"One","context_window":8192,"default_max_tokens":1024},{"id":"two","name":"Two","context_window":8192,"default_max_tokens":1024}]},"additional-client-provider":{"id":"additional-client-provider","name":"Additional","type":"openai-compat","api_key":"synthetic-unselected","base_url":"https://additional.invalid/v1","models":[{"id":"additional","name":"Additional","context_window":8192,"default_max_tokens":1024}]}},
	 "models":{"large":{"provider":"transaction-provider","model":"one"},"small":{"provider":"transaction-provider","model":"one"}}
	}`), 0o600))
	store, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Providers, 1)
	require.Len(t, proposal.Credentials, 1)
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(hs.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	c.SetLocalRuntimeStore(store)
	remoteDataRoot := t.TempDir()
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: remoteDataRoot, AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := workspace.NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	require.NotNil(t, created.Runtime)
	require.Equal(t, remoteDataRoot, created.RequestedDataDir)
	capabilities, err := c.NegotiateRemoteRuntime(t.Context())
	require.NoError(t, err)
	require.EqualValues(t, 10000, capabilities.DisconnectGraceMillis)
	encoded, err := json.Marshal(created)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-first", "retained private state must stay out of discovery")
	owner, ok := store.Config().ProviderOwner("transaction-provider")
	require.True(t, ok)

	eventCtx, cancelEvents := context.WithCancel(t.Context())
	defer cancelEvents()
	events, err := s.Backend().SubscribeEvents(eventCtx, created.ID)
	require.NoError(t, err)
	loseAck.Store(true)
	require.NoError(t, w.SetProviderAPIKey(config.ScopeGlobal, owner.ProviderID, config.ProviderAPIKeyCredential{Owner: owner, APIKey: "synthetic-second"}))
	awaitChange := time.After(5 * time.Second)
waitForChange:
	for {
		select {
		case event := <-events:
			if changed, ok := event.Payload.(pubsub.Event[proto.ConfigChanged]); ok {
				require.Equal(t, created.ID, changed.Payload.WorkspaceID)
				break waitForChange
			}
		case <-awaitChange:
			t.Fatal("committed runtime did not publish ConfigChanged")
		}
	}
	accepted, err := c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, accepted.Authority.Revision)
	require.EqualValues(t, 1, puts.Load(), "lost acknowledgement must not cause a duplicate update")
	local, err := os.ReadFile(config.GlobalConfigData())
	require.NoError(t, err)
	require.Contains(t, string(local), "synthetic-second")
	source, err := os.ReadFile(configFile)
	require.NoError(t, err)
	require.Contains(t, string(source), "synthetic-first")

	reject.Store(true)
	_, err = w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, config.SelectedModel{Provider: owner.ProviderID, Model: "two"}, owner)
	require.ErrorContains(t, err, "acknowledgement is pending")
	noChangeDeadline := time.After(100 * time.Millisecond)
assertNoChange:
	for {
		select {
		case event := <-events:
			_, changed := event.Payload.(pubsub.Event[proto.ConfigChanged])
			require.False(t, changed, "rejected runtime published ConfigChanged")
		case <-noChangeDeadline:
			break assertNoChange
		}
	}
	require.Equal(t, "one", w.Config().Models[config.SelectedModelTypeLarge].Model, "rejected state must not masquerade as accepted")
	accepted, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.EqualValues(t, 2, accepted.Authority.Revision)
	reject.Store(false)
	state, err := w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, config.SelectedModel{Provider: owner.ProviderID, Model: "two"}, owner)
	require.NoError(t, err)
	require.NoError(t, w.UpdateAgentModel(context.Background(), state))
	require.Equal(t, "two", w.Config().Models[config.SelectedModelTypeLarge].Model)
	accepted, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.EqualValues(t, 3, accepted.Authority.Revision)
	// Disabling a selection is a complete revision, not an invalid candidate
	// that silently leaves the previous credential active remotely.
	require.NoError(t, w.SetProviderDisabled(config.ScopeGlobal, owner, true))
	accepted, err = c.GetWorkspace(t.Context(), created.ID)
	require.NoError(t, err)
	require.EqualValues(t, 4, accepted.Authority.Revision)
	provider, _ := accepted.Config.Providers.Get(owner.ProviderID)
	require.True(t, provider.Disable)
	require.NoError(t, w.SetProviderDisabled(config.ScopeGlobal, owner, false))
	received, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	active, _ := received.Cfg.Config().Providers.Get(owner.ProviderID)
	require.False(t, active.Disable)
	require.Equal(t, "synthetic-second", active.APIKey)
	additionalOwner, ok := w.Config().ProviderOwner("additional-client-provider")
	require.True(t, ok)
	_, alreadyPresent := received.Cfg.Config().Providers.Get(additionalOwner.ProviderID)
	require.False(t, alreadyPresent)
	additionalModel, err := w.GetDefaultSmallModel(additionalOwner.ProviderID)
	require.NoError(t, err)
	require.Equal(t, "additional", additionalModel.Model)
	_, err = w.UpdatePreferredModel(config.ScopeGlobal, config.SelectedModelTypeLarge, additionalModel, additionalOwner)
	require.NoError(t, err)
	added, found := received.Cfg.Config().Providers.Get(additionalOwner.ProviderID)
	require.True(t, found)
	require.Equal(t, "synthetic-unselected", added.APIKey)

	// Core Codex is used only as a synthetic account-capable fixture. No real
	// OAuth or provider endpoint is called by these account transactions.
	t.Setenv("CRUX_PROVIDER_PROFILE", "integrated")
	oauthConfig := t.TempDir()
	oauthData := t.TempDir()
	t.Setenv("CRUX_GLOBAL_CONFIG", oauthConfig)
	t.Setenv("CRUX_GLOBAL_DATA", oauthData)
	firstAccount := accounts.Entry{ID: "first", AccessToken: "synthetic-first-account", RefreshToken: "synthetic-first-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"first-remote"}`)}
	secondAccount := accounts.Entry{ID: "second", AccessToken: "synthetic-second-account", RefreshToken: "synthetic-second-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"second-remote"}`)}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, secondAccount))
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, firstAccount))
	require.NoError(t, os.WriteFile(filepath.Join(oauthConfig, "crux.json"), []byte(`{"providers":{"codex":{"api_key":"synthetic-first-account","owner":{"type":"core","construction":"integrated-codex"},"models":[{"id":"fixture-model","name":"Fixture"}]}},"models":{"large":{"provider":"codex","model":"fixture-model"},"small":{"provider":"codex","model":"fixture-model"}}}`), 0o600))
	oauthStore, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	oauthProposal, err := oauthStore.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	oauthClient, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(hs.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	oauthClient.SetLocalRuntimeStore(oauthStore)
	oauthCreated, err := oauthClient.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), AuthorityMode: "client", Runtime: &oauthProposal})
	require.NoError(t, err)
	oauthWorkspace := workspace.NewClientWorkspace(oauthClient, *oauthCreated)
	t.Cleanup(oauthWorkspace.Shutdown)
	registration, ok := oauthStore.ProviderRegistration("codex")
	require.True(t, ok)
	require.NoError(t, oauthWorkspace.InitCoderAgent(t.Context()))
	com := &common.Common{Workspace: oauthWorkspace}
	beforeSwitch := puts.Load()
	switched, ok := dialog.SwitchAccountCmd(com, dialog.ActionSwitchAccount{Provider: registration.AccountNamespace, AccountID: secondAccount.ID, DisplayName: "Second"})().(dialog.AccountSwitchedMsg)
	require.True(t, ok)
	require.NoError(t, switched.Err)
	require.Equal(t, beforeSwitch+1, puts.Load(), "the UI must publish exactly once after both local account writes")
	oauthReceiver, err := s.Backend().GetWorkspace(oauthCreated.ID)
	require.NoError(t, err)
	acceptedAccount, ok := oauthReceiver.Cfg.EphemeralAccount(registration.Owner())
	require.True(t, ok)
	require.Equal(t, secondAccount, *acceptedAccount)
	priorAccounts, err := accounts.List(t.Context(), registration.AccountNamespace)
	require.NoError(t, err)
	require.Len(t, priorAccounts, 2)
	for _, entry := range priorAccounts {
		if entry.ID != firstAccount.ID {
			continue
		}
		require.Equal(t, firstAccount.AccessToken, entry.AccessToken)
		require.Equal(t, firstAccount.RefreshToken, entry.RefreshToken)
		require.Equal(t, firstAccount.ExpiresAt, entry.ExpiresAt)
		require.JSONEq(t, string(firstAccount.Raw), string(entry.Raw))
	}
	loggedOut, ok := dialog.LogoutCmd(com, dialog.ActionLogout{Owner: registration.Owner(), AccountNamespace: registration.AccountNamespace, Label: "Codex"})().(dialog.LogoutDoneMsg)
	require.True(t, ok)
	require.NoError(t, loggedOut.Err)
	_, hasAccount := oauthReceiver.Cfg.EphemeralAccount(registration.Owner())
	require.False(t, hasAccount)
	require.ErrorContains(t, oauthReceiver.Cfg.RuntimeSnapshot().ClientProviderUnavailable("codex"), "has no credential")
	selectedProvider, _ := oauthReceiver.Cfg.Config().Providers.Get("codex")
	require.Empty(t, selectedProvider.APIKey)
	require.Nil(t, selectedProvider.OAuthToken)
	localAccount, err := accounts.Active(t.Context(), registration.AccountNamespace)
	require.NoError(t, err)
	require.Nil(t, localAccount)

	// Persist a session before actual TLS stream loss. The server's resolved
	// certificate directory must not be treated as a fresh data root on recovery.
	savedSession, err := w.CreateSession(t.Context(), "Retained client history")
	require.NoError(t, err)
	// A public config refresh drops the private SDK fields. Recovery must still
	// use the authority controller, not the redacted cached Workspace.
	publicEvents := make(chan any, 1)
	publicEvents <- pubsub.Event[proto.ConfigChanged]{Payload: proto.ConfigChanged{WorkspaceID: created.ID}}
	close(publicEvents)
	w.ConsumeEventsForTest(publicEvents, nil)
	require.NoError(t, store.SetProviderAPIKey(config.ScopeGlobal, additionalOwner.ProviderID, config.ProviderAPIKeyCredential{Owner: additionalOwner, APIKey: "synthetic-recollected-after-loss"}))
	t.Cleanup(workspace.SetSSEBackoffForTest(5*time.Millisecond, 25*time.Millisecond))
	done := make(chan struct{})
	go func() { w.RunSubscriptionForTest(func(tea.Msg) {}); close(done) }()
	require.Eventually(t, func() bool { return received.ConnectedClients() == 1 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, c.DeleteWorkspace(t.Context(), created.ID))
	hs.CloseClientConnections()
	require.Eventually(t, func() bool { return w.WorkspaceIDForTest() != created.ID }, 10*time.Second, 20*time.Millisecond)
	recovered, err := c.GetWorkspace(t.Context(), w.WorkspaceIDForTest())
	require.NoError(t, err)
	require.Equal(t, created.DataDir, recovered.DataDir)
	require.Equal(t, remoteDataRoot, recovered.RequestedDataDir)
	recoveredReceiver, err := s.Backend().GetWorkspace(recovered.ID)
	require.NoError(t, err)
	recoveredProvider, ok := recoveredReceiver.Cfg.Config().Providers.Get(additionalOwner.ProviderID)
	require.True(t, ok)
	require.Equal(t, "synthetic-recollected-after-loss", recoveredProvider.APIKey)
	history, err := w.ListSessions(t.Context())
	require.NoError(t, err)
	foundSession := false
	for _, entry := range history {
		if entry.ID == savedSession.ID {
			foundSession = true
		}
	}
	require.True(t, foundSession, "recovery must restore the same history store")
	w.Shutdown()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("recovery subscription did not stop")
	}

}
