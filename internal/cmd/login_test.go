package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

func TestLoginCmd_Aliases(t *testing.T) {
	t.Parallel()

	require.Equal(t, "auth", loginCmd.Aliases[0])
}

func TestLoginCmd_ForceFlag(t *testing.T) {
	t.Parallel()

	flag := loginCmd.Flags().Lookup("force")
	require.NotNil(t, flag)
	require.Equal(t, "f", flag.Shorthand)
}

type commandLoginStatusWorkspace struct {
	workspace.Workspace
	status      providerauth.Snapshot
	surface     providerregistry.Surface
	changeOwner bool
	imports     int
}

func (w *commandLoginStatusWorkspace) Config() *config.Config { return nil }
func (w *commandLoginStatusWorkspace) ProviderSurfaces() []providerregistry.Surface {
	return []providerregistry.Surface{w.surface.Clone()}
}
func (w *commandLoginStatusWorkspace) ProviderAuthentication(context.Context) (providerauth.Snapshot, error) {
	return w.status, nil
}
func (w *commandLoginStatusWorkspace) ImportCopilot(context.Context, providerregistry.RegistrationOwner) (bool, error) {
	w.imports++
	if w.changeOwner {
		owner := *w.surface.Owner
		owner.OAuthFlowID = "replacement"
		w.surface.Owner = &owner
		w.surface.Authentication[0].FlowID = owner.OAuthFlowID
		w.status.Providers[0].Owner = providerauth.PublicOwner(owner)
	}
	return true, nil
}
func newCommandLoginStatusWorkspace(t *testing.T) *commandLoginStatusWorkspace {
	t.Helper()
	var owner providerregistry.RegistrationOwner
	for _, registration := range providerregistry.Integrated() {
		if registration.ProviderID == "copilot" {
			owner = registration.Owner()
			break
		}
	}
	require.True(t, owner.HasOAuth)
	status := providerauth.Status{Owner: providerauth.PublicOwner(owner), Configured: true, AccountState: "none", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present", Refreshable: true}}}
	snapshot := providerauth.Snapshot{WorkspaceID: "fixture", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}, Providers: []providerauth.Status{status}}
	require.NoError(t, snapshot.Validate())
	surface := providerregistry.Surface{ID: owner.ProviderID, Name: "Copilot", Owner: &owner, Available: true, Authentication: []providerregistry.Authentication{{Kind: "oauth2", Adapter: owner.OAuthAdapter, FlowID: owner.OAuthFlowID, Available: true}}}
	return &commandLoginStatusWorkspace{status: snapshot, surface: surface}
}
func TestCLILoginRejectsOwnerReplacementAfterImport(t *testing.T) {
	ws := newCommandLoginStatusWorkspace(t)
	ws.changeOwner = true
	var output bytes.Buffer
	err := runWorkspaceLogin(t.Context(), ws, []string{"copilot"}, true, strings.NewReader(""), &output, nil, nil)
	require.ErrorContains(t, err, "original owner")
	require.Equal(t, 1, ws.imports)
	require.NotContains(t, output.String(), "Authenticated with")
}
func TestCLILoginExistingCredentialDoesNotBegin(t *testing.T) {
	ws := newCommandLoginStatusWorkspace(t)
	var output bytes.Buffer
	require.NoError(t, runWorkspaceLogin(t.Context(), ws, []string{"copilot"}, false, strings.NewReader(""), &output, nil, nil))
	require.Zero(t, ws.imports)
	require.Contains(t, output.String(), "already logged in")
	require.Contains(t, output.String(), "--force")
}
