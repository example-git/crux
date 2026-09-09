package model

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type authenticationUIWorkspace struct {
	*testWorkspace
	t             *testing.T
	allowIO       bool
	snapshot      providerauth.Snapshot
	accounts      providerauth.AccountsState
	surfaces      []providerregistry.Surface
	switches      []providerauth.SwitchRequest
	logouts       []providerauth.LogoutRequest
	mode          string
	reads, probes int
	readContext   context.Context
}

func (w *authenticationUIWorkspace) ProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	require.True(w.t, w.allowIO, "status read ran outside a command")
	w.reads++
	w.readContext = ctx
	return w.snapshot, ctx.Err()
}
func (w *authenticationUIWorkspace) ProviderAccounts(ctx context.Context, target providerauth.Target) (providerauth.AccountsState, error) {
	require.True(w.t, w.allowIO, "accounts read ran outside a command")
	require.Equal(w.t, w.accounts.Target, target)
	return w.accounts, ctx.Err()
}
func (w *authenticationUIWorkspace) ProviderSurfaces() []providerregistry.Surface { return w.surfaces }
func (w *authenticationUIWorkspace) SwitchProviderAccount(_ context.Context, request providerauth.SwitchRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "switch ran outside a command")
	w.switches = append(w.switches, request)
	return w.result(request.OperationID, request.Target, request.AccountID, false)
}
func (w *authenticationUIWorkspace) LogoutProvider(_ context.Context, request providerauth.LogoutRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "logout ran outside a command")
	w.logouts = append(w.logouts, request)
	return w.result(request.OperationID, request.Target, "", true)
}
func (w *authenticationUIWorkspace) AgentIsReady() bool {
	require.True(w.t, w.allowIO, "model probe ran outside a command")
	w.probes++
	return false
}
func (w *authenticationUIWorkspace) PermissionSkipRequests() bool {
	require.True(w.t, w.allowIO)
	return false
}
func (w *authenticationUIWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	require.True(w.t, w.allowIO)
	return nil
}
func (w *authenticationUIWorkspace) result(id string, target providerauth.Target, accountID string, logout bool) (providerauth.MutationOutcome, error) {
	outcome := providerauth.MutationOutcome{OperationID: id, Previous: target, Progress: providerauth.MutationProgress{AccountsSaved: true, ConfigSaved: true, RuntimePublished: true}}
	if w.mode == "lost" {
		return providerauth.MutationOutcome{}, errors.New("transport interrupted")
	}
	if w.mode == "partial" {
		return outcome, errors.New("remote acknowledgement unavailable")
	}
	current := providerauth.AccountsState{Target: target, Status: w.accounts.Status, Accounts: []providerauth.AccountSummary{}}
	current.Target.Generation.Sequence++
	current.Status.Credentials = []providerauth.CredentialStatus{{Kind: "api-key", State: "absent"}, {Kind: "oauth", State: "absent"}}
	current.Status.AccountState = "none"
	current.Status.ActiveAccountID = ""
	if !logout {
		current.Status.Configured = true
		current.Status.AccountState = "in-sync"
		current.Status.ActiveAccountID = accountID
		current.Status.Credentials = []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}
		current.Accounts = []providerauth.AccountSummary{{ID: accountID, DisplayName: "Second", Active: true, CredentialState: "present"}}
	}
	outcome.Change = &providerauth.Change{OperationID: id, Previous: target, Current: current, Models: providerauth.ModelState{Large: &providerauth.OwnedModelState{Model: config.SelectedModel{Provider: "irrelevant-historical", Model: "must-not-restore"}}}}
	if w.mode == "historical" {
		outcome.Superseded = true
	}
	if w.mode == "wrong-target" {
		outcome.Previous.Generation.Sequence++
	}
	if w.mode == "published-error" {
		return outcome, errors.New("remote acknowledgement unavailable")
	}
	return outcome, nil
}
func newAuthenticationUI(t *testing.T) (*UI, *authenticationUIWorkspace) {
	t.Helper()
	owner := providerregistry.RegistrationOwner{ProviderID: "host-only", Construction: providerregistry.ConstructionCopilot, HasOAuth: true, AccountNamespace: "not-on-client"}
	target := providerauth.Target{WorkspaceID: "host", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}, Owner: providerauth.PublicOwner(owner)}
	status := providerauth.Status{Owner: target.Owner, Configured: true, AccountState: "out-of-sync", ActiveAccountID: "first", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}}
	ws := &authenticationUIWorkspace{t: t, testWorkspace: &testWorkspace{cfg: &config.Config{Options: &config.Options{}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: "host-only", Model: "chosen-large"}, config.SelectedModelTypeSmall: {Provider: "host-only", Model: "chosen-small"}}}}, snapshot: providerauth.Snapshot{WorkspaceID: target.WorkspaceID, Generation: target.Generation, Providers: []providerauth.Status{status}}, accounts: providerauth.AccountsState{Target: target, Status: status, Accounts: []providerauth.AccountSummary{{ID: "second", DisplayName: "Second", CredentialState: "refresh-only", Refreshable: true}}}, surfaces: []providerregistry.Surface{{ID: owner.ProviderID, Name: "Host Provider", Owner: &owner}}}
	ui := newTestUI()
	ui.com.Workspace = ws
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	ui.dialog = dialog.NewOverlay()
	return ui, ws
}
func runAuthenticationCmd(ws *authenticationUIWorkspace, cmd tea.Cmd) []tea.Msg {
	ws.allowIO = true
	defer func() { ws.allowIO = false }()
	return collectCommandMessages(cmd)
}
func openLoadedAuthentication(t *testing.T, ui *UI, ws *authenticationUIWorkspace, logout bool) *dialog.AccountAuthentication {
	t.Helper()
	cmd := ui.openAuthenticationAccounts(logout)
	require.Zero(t, ws.reads)
	messages := runAuthenticationCmd(ws, cmd)
	require.Len(t, messages, 1)
	_, cmd = ui.Update(messages[0])
	runAuthenticationCmd(ws, cmd)
	id := dialog.AccountSwitcherID
	if logout {
		id = dialog.LogoutID
	}
	return ui.dialog.Dialog(id).(interface {
		AuthenticationState() *dialog.AccountAuthentication
	}).AuthenticationState()
}
func prepareAndDispatchAuthentication(t *testing.T, ui *UI, ws *authenticationUIWorkspace, d *dialog.AccountAuthentication) authenticationCompletedMsg {
	t.Helper()
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.IsType(t, dialog.ActionAuthenticationSelect{}, action)
	prepared := runAuthenticationCmd(ws, ui.handleDialogAction(action))
	require.Len(t, prepared, 1)
	require.Empty(t, ws.switches)
	require.Empty(t, ws.logouts)
	_, cmd := ui.Update(prepared[0])
	completed := runAuthenticationCmd(ws, cmd)
	for _, message := range completed {
		if result, ok := message.(authenticationCompletedMsg); ok {
			return result
		}
	}
	t.Fatal("no mutation completion")
	return authenticationCompletedMsg{}
}
func authenticationInfo(messages []tea.Msg) string {
	var text []string
	for _, message := range messages {
		if info, ok := message.(util.InfoMsg); ok {
			text = append(text, info.Msg)
		}
	}
	return strings.Join(text, "\n")
}
func TestAuthenticationUICompletionAfterClosurePreservesModels(t *testing.T) {
	for _, logout := range []bool{false, true} {
		t.Run(map[bool]string{false: "switch", true: "logout"}[logout], func(t *testing.T) {
			for _, closing := range []string{"closed", "replacement"} {
				t.Run(closing, func(t *testing.T) {
					ui, ws := newAuthenticationUI(t)
					d := openLoadedAuthentication(t, ui, ws, logout)
					completed := prepareAndDispatchAuthentication(t, ui, ws, d)
					require.Zero(t, ws.probes)
					require.Regexp(t, "^[0-9a-f]{32}$", completed.operation.id)
					ui.dialog.CloseDialog(d.ID())
					var replacement dialog.Dialog
					if closing == "replacement" {
						replacement = dialog.NewLogout(ui.com)
						ui.dialog.OpenDialog(replacement)
					}
					oldUsageGeneration := ui.usageFetchGen
					_, cmd := ui.Update(completed)
					require.Zero(t, ws.probes, "Update only schedules presentation")
					messages := runAuthenticationCmd(ws, cmd)
					expected := "Switched account to Second"
					if logout {
						expected = "Logged out of Host Provider"
					}
					require.Contains(t, authenticationInfo(messages), expected)
					require.Greater(t, ui.usageFetchGen, oldUsageGeneration)
					require.Equal(t, 1, ws.probes)
					require.Zero(t, ws.updateAgentCalls)
					require.Zero(t, ws.preferredModelCalls)
					require.Equal(t, "chosen-large", ws.cfg.Models[config.SelectedModelTypeLarge].Model)
					require.Equal(t, "chosen-small", ws.cfg.Models[config.SelectedModelTypeSmall].Model)
					if replacement != nil {
						require.Same(t, replacement, ui.dialog.Dialog(dialog.LogoutID))
					}
					_, cmd = ui.Update(completed)
					require.NotContains(t, authenticationInfo(runAuthenticationCmd(ws, cmd)), expected, "duplicate delivery must not repeat the result")
				})
			}
		})
	}
}
func TestAuthenticationUIExactRequestRetryAndPartialStatus(t *testing.T) {
	for _, logout := range []bool{false, true} {
		ui, ws := newAuthenticationUI(t)
		ws.mode = "partial"
		d := openLoadedAuthentication(t, ui, ws, logout)
		completed := prepareAndDispatchAuthentication(t, ui, ws, d)
		ui.dialog.CloseDialog(d.ID())
		_, cmd := ui.Update(completed)
		text := authenticationInfo(runAuthenticationCmd(ws, cmd))
		require.Contains(t, text, "result could not be confirmed")
		require.Contains(t, text, "accounts saved, configuration saved, local runtime published")
		require.NotContains(t, text, "reconnect")
		require.Zero(t, ws.probes)
		require.True(t, completed.operation.retry)
		runAuthenticationCmd(ws, ui.openAuthenticationAccounts(logout))
		reopened := ui.dialog.Dialog(d.ID()).(interface {
			AuthenticationState() *dialog.AccountAuthentication
		}).AuthenticationState()
		action := reopened.HandleMsg(tea.KeyPressMsg{Code: 't', Mod: tea.ModCtrl})
		require.IsType(t, dialog.ActionAuthenticationRetry{}, action)
		messages := runAuthenticationCmd(ws, ui.handleDialogAction(action))
		require.Len(t, messages, 1)
		if logout {
			require.Len(t, ws.logouts, 2)
			require.Equal(t, ws.logouts[0], ws.logouts[1])
		} else {
			require.Len(t, ws.switches, 2)
			require.Equal(t, ws.switches[0], ws.switches[1])
		}
		require.Zero(t, ws.updateAgentCalls)
	}
}
func TestAuthenticationUIRejectsUncertainHistoricalAndForeignCompletion(t *testing.T) {
	for _, mode := range []string{"lost", "historical", "wrong-target", "published-error", "workspace changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws := newAuthenticationUI(t)
			ws.mode = mode
			d := openLoadedAuthentication(t, ui, ws, false)
			completed := prepareAndDispatchAuthentication(t, ui, ws, d)
			if mode == "workspace changed" {
				_, other := newAuthenticationUI(t)
				ui.com.Workspace = other
			}
			_, cmd := ui.Update(completed)
			text := authenticationInfo(runAuthenticationCmd(ws, cmd))
			switch mode {
			case "historical":
				require.Contains(t, text, "later authentication state is active")
				require.False(t, completed.operation.retry)
			case "workspace changed":
				require.Contains(t, text, "Previous workspace")
			default:
				require.Contains(t, text, "could not be confirmed")
				require.True(t, completed.operation.retry)
			}
			if mode == "lost" {
				require.NotContains(t, text, "Confirmed local effects")
			}
			if mode == "published-error" {
				require.Contains(t, text, "local runtime published")
				require.NotContains(t, text, "Switched account")
			}
			require.Zero(t, ws.probes)
			require.Zero(t, ws.updateAgentCalls)
			require.Equal(t, "chosen-large", ws.cfg.Models[config.SelectedModelTypeLarge].Model)
		})
	}
}
func TestAuthenticationUIReadAndPreparationLifecycle(t *testing.T) {
	for _, mode := range []string{"closed", "replaced", "reloaded", "workspace changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws := newAuthenticationUI(t)
			cmd := ui.openAuthenticationAccounts(false)
			d := ui.dialog.Dialog(dialog.AccountSwitcherID).(*dialog.AccountSwitcher).AuthenticationState()
			read := ui.authenticationReads[d]
			messages := runAuthenticationCmd(ws, cmd)
			switch mode {
			case "closed":
				ui.handleDialogAction(dialog.ActionClose{})
			case "replaced":
				ui.openAuthenticationAccounts(false)
			case "reloaded":
				ui.loadAuthenticationAccounts(d)
			case "workspace changed":
				_, other := newAuthenticationUI(t)
				ui.com.Workspace = other
			}
			ui.Update(messages[0])
			require.ErrorIs(t, ws.readContext.Err(), context.Canceled)
			require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}), "stale rows must not install")
			require.NotSame(t, read, ui.authenticationReads[d])
		})
	}
	ui, ws := newAuthenticationUI(t)
	d := openLoadedAuthentication(t, ui, ws, false)
	prepared := runAuthenticationCmd(ws, ui.handleDialogAction(d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})))
	ui.handleDialogAction(dialog.ActionClose{})
	_, cmd := ui.Update(prepared[0])
	runAuthenticationCmd(ws, cmd)
	require.Empty(t, ws.switches, "closing before dispatch must not execute mutation")
}

type authenticationBlockingReadWorkspace struct {
	*authenticationUIWorkspace
	started chan context.Context
}

func (w *authenticationBlockingReadWorkspace) ProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	w.started <- ctx
	<-ctx.Done()
	return providerauth.Snapshot{}, ctx.Err()
}
func TestAuthenticationUICloseCancelsReadBeforeItReturns(t *testing.T) {
	ui, base := newAuthenticationUI(t)
	ws := &authenticationBlockingReadWorkspace{authenticationUIWorkspace: base, started: make(chan context.Context, 1)}
	ui.com.Workspace = ws
	cmd := ui.openAuthenticationAccounts(false)
	result := make(chan tea.Msg, 1)
	go func() { result <- cmd() }()
	var ctx context.Context
	select {
	case ctx = <-ws.started:
	case <-time.After(5 * time.Second):
		t.Fatal("status read did not start")
	}
	ui.handleDialogAction(dialog.ActionClose{})
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	select {
	case msg := <-result:
		ui.Update(msg)
	case <-time.After(5 * time.Second):
		t.Fatal("closed dialog did not cancel pending read")
	}
	require.Empty(t, ui.authenticationReads)
	require.Empty(t, base.switches)
}

func TestAuthenticationUILoadedRowsCannotChangeWorkspace(t *testing.T) {
	ui, ws := newAuthenticationUI(t)
	d := openLoadedAuthentication(t, ui, ws, false)
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}).(dialog.ActionAuthenticationSelect)
	_, other := newAuthenticationUI(t)
	// Deliberately identical public targets: instance ownership is an additional
	// UI boundary, not inferred from the display provider ID or generation.
	require.Equal(t, ws.accounts.Target, other.accounts.Target)
	ui.com.Workspace = other
	messages := runAuthenticationCmd(other, ui.handleDialogAction(action))
	require.Contains(t, authenticationInfo(messages), "workspace changed")
	require.Empty(t, ws.switches)
	require.Empty(t, other.switches)
	require.Empty(t, ui.authenticationOperations)
	ui.Update(nil)
	require.Nil(t, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
}

func TestAuthenticationUIReceiptRetryAndQuotaRejectOldCompletion(t *testing.T) {
	ui, ws := newAuthenticationUI(t)
	ws.mode = "partial"
	d := openLoadedAuthentication(t, ui, ws, false)
	first := prepareAndDispatchAuthentication(t, ui, ws, d)
	_, cmd := ui.Update(first)
	runAuthenticationCmd(ws, cmd)
	require.True(t, first.operation.retry)
	ws.mode = ""
	var second authenticationCompletedMsg
	for _, msg := range runAuthenticationCmd(ws, ui.handleDialogAction(dialog.ActionAuthenticationRetry{Dialog: d})) {
		if completed, ok := msg.(authenticationCompletedMsg); ok {
			second = completed
		}
	}
	require.Equal(t, first.operation, second.operation)
	require.Greater(t, second.attempt, first.attempt)
	_, cmd = ui.Update(first)
	require.Empty(t, authenticationInfo(runAuthenticationCmd(ws, cmd)), "old attempt cannot finish a retry")
	require.True(t, second.operation.pending)
	oldGeneration := ui.usageFetchGen
	oldQuota := &oauthusage.Usage{}
	ui.providerUsage = oldQuota
	ui.dialog.CloseDialog(d.ID())
	_, cmd = ui.Update(second)
	require.Nil(t, ui.providerUsage)
	require.Contains(t, authenticationInfo(runAuthenticationCmd(ws, cmd)), "Switched account to Second")
	_, cmd = ui.Update(usageUpdatedMsg{gen: oldGeneration, usage: oldQuota})
	runAuthenticationCmd(ws, cmd)
	require.Nil(t, ui.providerUsage, "quota from the previous account cannot overwrite the new generation")
	require.False(t, second.operation.retry)
	require.Equal(t, ws.switches[0], ws.switches[1])
}
