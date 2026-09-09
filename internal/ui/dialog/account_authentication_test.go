package dialog

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/common"
	"github.com/example-git/crux/internal/ui/styles"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authenticationRowsWorkspace struct {
	workspace.Workspace
	snapshot providerauth.Snapshot
	accounts providerauth.AccountsState
	surfaces []providerregistry.Surface
	targets  []providerauth.Target
	err      error
}

func (w *authenticationRowsWorkspace) ProviderAuthentication(context.Context) (providerauth.Snapshot, error) {
	return w.snapshot, nil
}
func (w *authenticationRowsWorkspace) ProviderAccounts(_ context.Context, target providerauth.Target) (providerauth.AccountsState, error) {
	w.targets = append(w.targets, target)
	return w.accounts, w.err
}
func (w *authenticationRowsWorkspace) ProviderSurfaces() []providerregistry.Surface {
	return w.surfaces
}
func authenticationRowsFixture() *authenticationRowsWorkspace {
	owner := providerregistry.RegistrationOwner{ProviderID: "host-only", Construction: providerregistry.ConstructionCopilot, HasOAuth: true, AccountNamespace: "must-not-route-locally", HasManifest: true, ManifestID: "host.plugin", ManifestVersion: "1"}
	public := providerauth.PublicOwner(owner)
	target := providerauth.Target{WorkspaceID: "host", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 3}, Owner: public}
	status := providerauth.Status{Owner: public, Configured: false, AccountState: "out-of-sync", ActiveAccountID: "opaque-account", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "absent"}, {Kind: "oauth", State: "absent"}}}
	return &authenticationRowsWorkspace{snapshot: providerauth.Snapshot{WorkspaceID: target.WorkspaceID, Generation: target.Generation, Providers: []providerauth.Status{status}}, accounts: providerauth.AccountsState{Target: target, Status: status, Accounts: []providerauth.AccountSummary{{ID: "opaque-account", DisplayName: "Host Account", Active: true, CredentialState: "refresh-only", Refreshable: true}}}, surfaces: []providerregistry.Surface{{ID: owner.ProviderID, Name: "Host Provider", Owner: &owner}}}
}
func TestAuthenticationPickerConstructionAndInputArePure(t *testing.T) {
	theme := styles.ThemeForProvider("")
	com := &common.Common{Workspace: &struct{ workspace.Workspace }{}, Styles: &theme}
	for _, d := range []*AccountAuthentication{NewAccountSwitcher(com).AuthenticationState(), NewLogout(com).AuthenticationState()} {
		require.True(t, d.loading)
		require.Empty(t, d.list.FilteredItems())
		require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
		d.Draw(uv.NewScreenBuffer(80, 24), uv.Rect(0, 0, 80, 24))
		fixture := authenticationRowsFixture()
		generation := d.BeginRead()
		rows, err := LoadAuthenticationRows(t.Context(), fixture, false)
		require.NoError(t, err)
		require.True(t, d.loading, "command execution cannot install rows")
		require.Empty(t, d.list.FilteredItems())
		require.False(t, d.CompleteRead(generation-1, rows, nil))
		require.True(t, d.CompleteRead(generation, rows, nil))
		action, ok := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionAuthenticationSelect)
		require.True(t, ok)
		require.Equal(t, fixture.accounts.Target, action.Row.Target)
		require.Equal(t, "opaque-account", action.Row.AccountID)
		d.SetOperation("pending", true, false)
		require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	}
}
func TestAuthenticationRowsUseHostPublicOwnerAndStatus(t *testing.T) {
	fixture := authenticationRowsFixture()
	rows, err := LoadAuthenticationRows(t.Context(), fixture, false)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, []providerauth.Target{fixture.accounts.Target}, fixture.targets)
	require.Contains(t, rows[0].Info, "Host Provider")
	require.Contains(t, rows[0].Info, "refresh-only · refreshable · active · out-of-sync")
	require.NotContains(t, rows[0].Info, "must-not-route-locally")
	require.False(t, fixture.snapshot.Providers[0].Configured, "unconfigured stored account stays selectable")
	replacement := *fixture.surfaces[0].Owner
	replacement.ManifestVersion = "2"
	fixture.surfaces[0].Owner = &replacement
	rows, err = LoadAuthenticationRows(t.Context(), fixture, false)
	require.NoError(t, err)
	require.Contains(t, rows[0].Info, "host-only")
	require.NotContains(t, rows[0].Info, "Host Provider", "same ID with another owner must not supply the name")
	fixture.err = errors.New("owner changed")
	rows, err = LoadAuthenticationRows(t.Context(), fixture, false)
	require.ErrorContains(t, err, "owner changed")
	require.Nil(t, rows)
	fixture.err = nil
	fixture.accounts.Target.Generation.Sequence++
	rows, err = LoadAuthenticationRows(t.Context(), fixture, false)
	require.ErrorContains(t, err, "target changed")
	require.Nil(t, rows)
}
func TestAuthenticationLogoutEligibilityAndReadErrors(t *testing.T) {
	fixture := authenticationRowsFixture()
	rows, err := LoadAuthenticationRows(t.Context(), fixture, true)
	require.NoError(t, err)
	require.Empty(t, rows, "stored accounts do not widen logout menu")
	status := &fixture.snapshot.Providers[0]
	status.Configured = true
	status.Disabled = true
	status.Credentials[1] = providerauth.CredentialStatus{Kind: "oauth", State: "refresh-only", Refreshable: true}
	rows, err = LoadAuthenticationRows(t.Context(), fixture, true)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Contains(t, rows[0].Info, "disabled")
	require.Empty(t, fixture.targets, "logout does not need account file enumeration")
	status.Owner.HasOAuth = false
	rows, err = LoadAuthenticationRows(t.Context(), fixture, true)
	require.NoError(t, err)
	require.Empty(t, rows)
	theme := styles.ThemeForProvider("")
	d := NewLogout(&common.Common{Styles: &theme}).AuthenticationState()
	gen := d.BeginRead()
	d.CompleteRead(gen, nil, errors.New("host read failed"))
	require.Contains(t, d.notice, "Could not load")
	require.NotContains(t, d.notice, "No configured")
	d.CompleteRead(gen, nil, nil)
	require.Contains(t, d.notice, "No configured")
}
func TestAuthenticationRowsThroughWorkspaceSDK(t *testing.T) {
	fixture := authenticationRowsFixture()
	var calls atomic.Int64
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && (strings.HasPrefix(r.URL.Path, "/v1/clients/") || r.URL.Path == "/v1/workspaces/host") {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workspaces/host/auth":
			require.Equal(t, http.MethodGet, r.Method)
			_ = json.NewEncoder(w).Encode(fixture.snapshot)
		case "/v1/workspaces/host/auth/accounts":
			require.Equal(t, http.MethodPost, r.Method)
			var target providerauth.Target
			require.NoError(t, json.NewDecoder(r.Body).Decode(&target))
			require.Equal(t, fixture.accounts.Target, target)
			_ = json.NewEncoder(w).Encode(fixture.accounts)
		default:
			t.Errorf("unexpected UI read path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	address, err := url.Parse(host.URL)
	require.NoError(t, err)
	sdk, err := client.NewClient(t.TempDir(), "tcp", address.Host)
	require.NoError(t, err)
	retained := workspace.NewClientWorkspace(sdk, proto.Workspace{ID: "host", Config: &config.Config{Options: &config.Options{}}, ProviderSurfaces: fixture.surfaces})
	defer retained.Shutdown()
	theme := styles.ThemeForProvider("")
	d := NewAccountSwitcher(&common.Common{Workspace: retained, Styles: &theme}).AuthenticationState()
	require.Zero(t, calls.Load())
	generation := d.BeginRead()
	rows, err := LoadAuthenticationRows(t.Context(), retained, false)
	require.NoError(t, err)
	require.EqualValues(t, 2, calls.Load())
	require.Empty(t, d.list.FilteredItems())
	require.True(t, d.CompleteRead(generation, rows, nil))
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(ActionAuthenticationSelect)
	require.Equal(t, fixture.accounts.Target, action.Row.Target)
	require.Equal(t, "opaque-account", action.Row.AccountID)
}

func TestAuthenticationRecoveryHintsRemainVisible(t *testing.T) {
	theme := styles.ThemeForProvider("")
	d := NewAccountSwitcher(&common.Common{Styles: &theme}).AuthenticationState()
	d.CompleteRead(d.Generation(), nil, nil)
	d.SetOperation("Authentication result could not be confirmed. "+strings.Repeat("Detailed local progress. ", 20), false, true)
	screen := uv.NewScreenBuffer(80, 24)
	d.Draw(screen, uv.Rect(0, 0, 80, 24))
	output := ansi.Strip(screen.String())
	require.Contains(t, output, "ctrl+t")
	require.Contains(t, output, "retry original")
	require.Contains(t, output, "ctrl+r")
	require.Contains(t, output, "reload")
	require.Contains(t, output, "result could not")
}
