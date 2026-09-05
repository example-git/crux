package dialog

import (
	"errors"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type accountSwitchTestWorkspace struct {
	workspace.Workspace
	cfg         *config.Config
	nextCfg     *config.Config
	configCalls int
	credential  config.ProviderOAuthCredential
	providerID  string
	setErr      error
	removed     providerregistry.RegistrationOwner
	removeError error
}

func (w *accountSwitchTestWorkspace) Config() *config.Config {
	w.configCalls++
	if w.configCalls > 1 && w.nextCfg != nil {
		return w.nextCfg
	}
	return w.cfg
}

func (w *accountSwitchTestWorkspace) SetProviderAPIKey(_ config.Scope, providerID string, value any) error {
	if w.setErr != nil {
		return w.setErr
	}
	credential, ok := value.(config.ProviderOAuthCredential)
	if !ok {
		return errors.New("credential is not owner-bound")
	}
	w.providerID = providerID
	w.credential = credential
	return nil
}

func (w *accountSwitchTestWorkspace) RemoveProviderCredentials(_ config.Scope, owner providerregistry.RegistrationOwner) error {
	if w.removeError != nil {
		return w.removeError
	}
	w.removed = owner
	return nil
}

func copilotTestRegistration(t *testing.T) providerregistry.Registration {
	t.Helper()
	for _, registration := range providerregistry.Integrated() {
		if registration.ProviderID == "copilot" {
			return registration
		}
	}
	t.Fatal("integrated Copilot registration not found")
	return providerregistry.Registration{}
}

func copilotTestConfig(provider config.ProviderConfig) *config.Config {
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{"copilot": provider})}
	return config.NewTestStore(cfg).Config()
}

func TestLoginReturnsCompletedOAuthAsDialogAction(t *testing.T) {
	token := &oauth.Token{AccessToken: "access-token"}
	continuation := &ActionSelectModel{}
	login := &Login{
		state:        loginStateBrowser,
		continuation: continuation,
	}

	registration := providerregistry.Registration{ProviderID: "provider"}
	action := login.HandleMsg(loginTokenMsg{registration: registration, token: token})

	done, ok := action.(LoginDoneMsg)
	require.True(t, ok)
	require.Equal(t, registration, done.Registration)
	require.Same(t, token, done.Token)
	require.Same(t, continuation, done.Continuation)
	require.Equal(t, loginStateSaving, login.state)
}

func TestSwitchAccountCmdUsesExactAccountNamespace(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	ctx := t.Context()
	require.NoError(t, accounts.Save(ctx, accounts.ProviderCopilot, accounts.Entry{ID: "first", DisplayName: "First", AccessToken: "first-token"}))
	require.NoError(t, accounts.Save(ctx, accounts.ProviderCopilot, accounts.Entry{ID: "second", DisplayName: "Second", AccessToken: "second-token"}))
	require.NoError(t, accounts.SetActive(ctx, accounts.ProviderCopilot, "first"))
	ws := &accountSwitchTestWorkspace{cfg: copilotTestConfig(config.ProviderConfig{ID: "copilot"})}

	message, ok := SwitchAccountCmd(&common.Common{Workspace: ws}, ActionSwitchAccount{
		Provider: "github", AccountID: "second", DisplayName: "Second",
	})().(AccountSwitchedMsg)
	require.True(t, ok)
	require.NoError(t, message.Err)
	require.Equal(t, "copilot", message.CruxProviderID)
	require.Equal(t, "copilot", ws.providerID)
	require.Equal(t, "copilot", ws.credential.Owner.ProviderID)
	require.Equal(t, providerregistry.ConstructionCopilot, ws.credential.Owner.Construction)
	require.Equal(t, "second-token", ws.credential.Token.AccessToken)
	active, err := accounts.Active(ctx, accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Equal(t, "second", active.ID)
}

func TestSwitchAccountCmdRevalidatesOwnerBeforeRefresh(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	ctx := t.Context()
	reference := &config.ProviderPluginReference{ID: "replacement", Version: "2"}
	require.NoError(t, accounts.Save(ctx, accounts.ProviderCopilot, accounts.Entry{
		ID: "account", DisplayName: "Account", AccessToken: "expired-token", RefreshToken: "refresh-token",
		ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}))
	ws := &accountSwitchTestWorkspace{
		cfg:     copilotTestConfig(config.ProviderConfig{ID: "copilot"}),
		nextCfg: copilotTestConfig(config.ProviderConfig{ID: "copilot", Plugin: reference}),
	}

	message, ok := SwitchAccountCmd(&common.Common{Workspace: ws}, ActionSwitchAccount{
		Provider: "github", AccountID: "account", DisplayName: "Account",
	})().(AccountSwitchedMsg)
	require.True(t, ok)
	require.ErrorContains(t, message.Err, "owner changed")
	require.Empty(t, ws.providerID)
	entries, err := accounts.List(ctx, accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, "expired-token", entries[0].AccessToken)
}

func TestSwitchAccountCmdDoesNotActivateWhenCredentialCommitFails(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	ctx := t.Context()
	require.NoError(t, accounts.Save(ctx, accounts.ProviderCopilot, accounts.Entry{ID: "first", DisplayName: "First", AccessToken: "first-token"}))
	require.NoError(t, accounts.Save(ctx, accounts.ProviderCopilot, accounts.Entry{ID: "second", DisplayName: "Second", AccessToken: "second-token"}))
	require.NoError(t, accounts.SetActive(ctx, accounts.ProviderCopilot, "first"))
	ws := &accountSwitchTestWorkspace{
		cfg:    copilotTestConfig(config.ProviderConfig{ID: "copilot"}),
		setErr: errors.New("owner changed"),
	}

	message, ok := SwitchAccountCmd(&common.Common{Workspace: ws}, ActionSwitchAccount{
		Provider: "github", AccountID: "second", DisplayName: "Second",
	})().(AccountSwitchedMsg)
	require.True(t, ok)
	require.ErrorContains(t, message.Err, "owner changed")
	active, err := accounts.Active(ctx, accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Equal(t, "first", active.ID)
}

func TestSaveLoginCmdDoesNotSaveAccountWhenCredentialCommitFails(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := copilotTestRegistration(t)
	ws := &accountSwitchTestWorkspace{
		cfg:    copilotTestConfig(config.ProviderConfig{ID: "copilot"}),
		setErr: errors.New("owner changed"),
	}

	message, ok := SaveLoginCmd(&common.Common{Workspace: ws}, LoginDoneMsg{
		Registration: registration,
		Token:        &oauth.Token{AccessToken: "new-access", RefreshToken: "new-refresh"},
	})().(LogoutDoneMsg)
	require.True(t, ok)
	require.ErrorContains(t, message.Err, "owner changed")
	entries, err := accounts.List(t.Context(), accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestLogoutCmdDoesNotRemoveAccountWhenCredentialRemovalFails(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := copilotTestRegistration(t)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCopilot, accounts.Entry{ID: "account", AccessToken: "access"}))
	ws := &accountSwitchTestWorkspace{
		cfg:         copilotTestConfig(config.ProviderConfig{ID: "copilot"}),
		removeError: errors.New("owner changed"),
	}

	message, ok := LogoutCmd(&common.Common{Workspace: ws}, ActionLogout{
		Owner: registration.Owner(), AccountNamespace: registration.AccountNamespace, Label: registration.Name,
	})().(LogoutDoneMsg)
	require.True(t, ok)
	require.ErrorContains(t, message.Err, "owner changed")
	entries, err := accounts.List(t.Context(), accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestLogoutCmdUsesInitiatingOwnerAndNamespace(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	registration := copilotTestRegistration(t)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCopilot, accounts.Entry{ID: "account", AccessToken: "access"}))
	ws := &accountSwitchTestWorkspace{cfg: copilotTestConfig(config.ProviderConfig{ID: "copilot"})}

	message, ok := LogoutCmd(&common.Common{Workspace: ws}, ActionLogout{
		Owner: registration.Owner(), AccountNamespace: registration.AccountNamespace, Label: registration.Name,
	})().(LogoutDoneMsg)
	require.True(t, ok)
	require.NoError(t, message.Err)
	require.Equal(t, registration.Owner(), ws.removed)
	entries, err := accounts.List(t.Context(), accounts.ProviderCopilot)
	require.NoError(t, err)
	require.Empty(t, entries)
}
