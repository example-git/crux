package dialog

import (
	"errors"
	"testing"

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
