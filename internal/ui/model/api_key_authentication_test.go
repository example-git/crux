package model

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/ui/attachments"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type checkedKeyUIWorkspace struct {
	*testWorkspace
	t            *testing.T
	allowIO      bool
	snapshot     providerauth.Snapshot
	checks       []providerauth.APIKeyCheckRequest
	saves        []providerauth.APIKeySaveRequest
	recoveries   []workspace.ProviderAuthenticationRecoveryRequest
	mode         string
	checkContext context.Context
}

func (w *checkedKeyUIWorkspace) ProviderAuthentication(ctx context.Context) (providerauth.Snapshot, error) {
	require.True(w.t, w.allowIO, "status I/O outside Cmd")
	return w.snapshot, ctx.Err()
}
func (w *checkedKeyUIWorkspace) CheckProviderAPIKey(ctx context.Context, r providerauth.APIKeyCheckRequest) (providerauth.APIKeyCheckOutcome, error) {
	require.True(w.t, w.allowIO, "check I/O outside Cmd")
	w.checks = append(w.checks, r)
	w.checkContext = ctx
	out := providerauth.APIKeyCheckOutcome{CheckID: r.CheckID, Previous: r.Target, CredentialID: r.CredentialID, Probe: config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyHTTP200, HTTPStatus: 200, EnteredKeyInAuthorization: true}}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	if w.mode == "check-error" {
		return out, providerauth.ErrAPIKeyCheck
	}
	target := r.Target
	target.Generation.Sequence++
	out.CheckedTarget = &target
	if w.mode == "wrong-check" {
		out.CheckID = strings.Repeat("f", 32)
	}
	return out, nil
}
func (w *checkedKeyUIWorkspace) SaveCheckedProviderAPIKey(_ context.Context, r providerauth.APIKeySaveRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "save I/O outside Cmd")
	w.saves = append(w.saves, r)
	return w.result(r)
}
func (w *checkedKeyUIWorkspace) result(r providerauth.APIKeySaveRequest) (providerauth.MutationOutcome, error) {
	out := providerauth.MutationOutcome{OperationID: r.OperationID, CheckID: r.CheckID, Previous: r.Target, Progress: providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}}
	for _, check := range w.checks {
		if check.CheckID == r.CheckID {
			out.CredentialID = check.CredentialID
			break
		}
	}
	require.NotEmpty(w.t, out.CredentialID, "save must retain its exact original checked slot")
	if w.mode == "partial" {
		return out, errors.New("uncertain acknowledgement")
	}
	if w.mode == "lost" {
		return providerauth.MutationOutcome{}, errors.New("transport interrupted")
	}
	target := r.Target
	target.Generation.Sequence++
	status := providerauth.Status{Owner: target.Owner, Configured: true, AccountState: "none", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "absent"}}, CredentialSlots: []providerauth.CredentialSlot{{ID: out.CredentialID, Kind: "api-key", Configured: true}}}
	out.Change = &providerauth.Change{OperationID: r.OperationID, Previous: r.Target, Current: providerauth.AccountsState{Target: target, Status: status, Accounts: []providerauth.AccountSummary{}}}
	if w.mode == "historical" {
		out.Superseded = true
	}
	if w.mode == "wrong-save" {
		out.CheckID = strings.Repeat("f", 32)
	}
	return out, nil
}
func (w *checkedKeyUIWorkspace) CanRecoverProviderAuthentication() bool { return true }
func (w *checkedKeyUIWorkspace) RecoverProviderAuthentication(_ context.Context, r workspace.ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO)
	w.recoveries = append(w.recoveries, r)
	return w.result(w.saves[0])
}
func (w *checkedKeyUIWorkspace) AgentIsReady() bool { require.True(w.t, w.allowIO); return false }
func (w *checkedKeyUIWorkspace) PermissionSkipRequests() bool {
	require.True(w.t, w.allowIO)
	return false
}
func (w *checkedKeyUIWorkspace) LSPGetStates() map[string]workspace.LSPClientInfo {
	require.True(w.t, w.allowIO)
	return nil
}
func newCheckedKeyUI(t *testing.T) (*UI, *checkedKeyUIWorkspace, dialog.ActionSelectModel) {
	ui, base, selection, _ := modelSelectionTestUI(t, true)
	owner := providerauth.PublicOwner(selection.ProviderOwner)
	status := providerauth.Status{Owner: owner, Configured: true, AccountState: "none", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "absent"}}, CredentialSlots: []providerauth.CredentialSlot{{ID: "provider.api_key", Kind: "api-key", Configured: true}}}
	ws := &checkedKeyUIWorkspace{testWorkspace: base, t: t, snapshot: providerauth.Snapshot{WorkspaceID: "key-ui", Generation: providerauth.Generation{Epoch: strings.Repeat("a", 32), Sequence: 1}, Providers: []providerauth.Status{status}}}
	ui.com.Workspace = ws
	ui.keyMap = DefaultKeyMap()
	ui.attachments = attachments.New(
		attachments.NewRenderer(
			ui.com.Styles.Attachments.Normal,
			ui.com.Styles.Attachments.Deleting,
			ui.com.Styles.Attachments.Image,
			ui.com.Styles.Attachments.Text,
			ui.com.Styles.Attachments.Skill,
			ui.com.Styles.Attachments.Remove,
		),
		attachments.Keymap{
			DeleteMode: ui.keyMap.Editor.AttachmentDeleteMode,
			DeleteAll:  ui.keyMap.Editor.DeleteAllAttachments,
			Escape:     ui.keyMap.Editor.Escape,
		},
	)
	ui.focus = uiFocusNone
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	return ui, ws, selection
}
func runCheckedKeyCmd(w *checkedKeyUIWorkspace, cmd tea.Cmd) []tea.Msg {
	w.allowIO = true
	defer func() { w.allowIO = false }()
	return collectCommandMessages(cmd)
}
func updateCheckedKey(t *testing.T, ui *UI, ws *checkedKeyUIWorkspace, msg tea.Msg) []tea.Msg {
	t.Helper()
	_, cmd := ui.Update(msg)
	return runCheckedKeyCmd(ws, cmd)
}
func openCheckedKey(t *testing.T, ui *UI, ws *checkedKeyUIWorkspace, selection dialog.ActionSelectModel) *dialog.APIKeyInput {
	t.Helper()
	messages := runCheckedKeyCmd(ws, ui.openAuthenticationDialog(selection))
	require.Len(t, messages, 1)
	updateCheckedKey(t, ui, ws, messages[0])
	return ui.dialog.Dialog(dialog.APIKeyInputID).(*dialog.APIKeyInput)
}
func checkKeyInput(t *testing.T, ui *UI, ws *checkedKeyUIWorkspace, d *dialog.APIKeyInput, source string) apiKeyCheckMsg {
	t.Helper()
	require.Same(t, d, ui.dialog.Dialog(dialog.APIKeyInputID))
	updateCheckedKey(t, ui, ws, tea.PasteMsg{Content: source})
	ids := updateCheckedKey(t, ui, ws, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Len(t, ids, 1)
	require.IsType(t, apiKeyIDsMsg{}, ids[0])
	results := updateCheckedKey(t, ui, ws, ids[0])
	require.Len(t, results, 1)
	return results[0].(apiKeyCheckMsg)
}
func saveKeyInput(t *testing.T, ui *UI, ws *checkedKeyUIWorkspace, d *dialog.APIKeyInput) apiKeySaveMsg {
	t.Helper()
	require.Same(t, d, ui.dialog.Dialog(dialog.APIKeyInputID))
	messages := updateCheckedKey(t, ui, ws, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Len(t, messages, 1)
	return messages[0].(apiKeySaveMsg)
}
func TestCheckedKeyUIFreezesInputAndContinuesAfterClosedSave(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	d := openCheckedKey(t, ui, ws, selection)
	checked := checkKeyInput(t, ui, ws, d, "source-A")
	require.NoError(t, checked.err)
	updateCheckedKey(t, ui, ws, checked)
	d.HandleMsg(tea.PasteMsg{Content: "source-B"})
	require.NotContains(t, fmt.Sprintf("%#v", checked.operation), "source-A")
	saved := saveKeyInput(t, ui, ws, d)
	require.NoError(t, saved.err)
	ui.handleDialogAction(dialog.ActionClose{})
	require.False(t, ui.apiKeyDialogOpen(d))
	updateCheckedKey(t, ui, ws, saved)
	require.Equal(t, "source-A", ws.checks[0].Source)
	require.Equal(t, ws.checks[0].CheckID, ws.saves[0].CheckID)
	require.Equal(t, 1, ws.preferredModelCalls)
	require.Equal(t, 1, ws.updateAgentCalls)
	updateCheckedKey(t, ui, ws, saved)
	require.Equal(t, 1, ws.preferredModelCalls, "duplicate completion must not repeat continuation")
}
func TestCheckedKeyUINoContinuationFromUnacknowledgedOrStaleResults(t *testing.T) {
	for _, mode := range []string{"partial", "lost", "wrong-save", "historical", "new-selection", "new-workspace", "owner-changed"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws, selection := newCheckedKeyUI(t)
			d := openCheckedKey(t, ui, ws, selection)
			checked := checkKeyInput(t, ui, ws, d, "source-A")
			updateCheckedKey(t, ui, ws, checked)
			if mode != "new-selection" && mode != "new-workspace" && mode != "owner-changed" {
				ws.mode = mode
			}
			saved := saveKeyInput(t, ui, ws, d)
			switch mode {
			case "new-selection":
				ui.modelSelectionGen++
			case "new-workspace":
				_, other, _ := newCheckedKeyUI(t)
				ui.com.Workspace = other
				other.allowIO = true
			case "owner-changed":
				ws.cfg = &config.Config{}
			}
			updateCheckedKey(t, ui, ws, saved)
			require.Zero(t, ws.preferredModelCalls)
			require.Zero(t, ws.updateAgentCalls)
		})
	}
}
func TestCheckedKeyUIRetainsPartialOriginalAcrossReopenAndRecovery(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	d := openCheckedKey(t, ui, ws, selection)
	updateCheckedKey(t, ui, ws, checkKeyInput(t, ui, ws, d, "source-A"))
	ws.mode = "partial"
	saved := saveKeyInput(t, ui, ws, d)
	updateCheckedKey(t, ui, ws, saved)
	original := ws.saves[0]
	ui.handleDialogAction(dialog.ActionClose{})
	ui.modelSelectionGen++
	d = openCheckedKey(t, ui, ws, selection)
	reload := runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyReload{Dialog: d}))
	require.Contains(t, authenticationInfo(reload), "original save receipt")
	ws.mode = "lost"
	retry := runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyRetry{Dialog: d}))
	updateCheckedKey(t, ui, ws, retry[0])
	require.Equal(t, original, ws.saves[1])
	require.True(t, ui.apiKeyOperations[ws].progress.ConfigSaved)
	ids := runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyRecover{Dialog: d}))
	require.Len(t, ids, 1)
	ws.mode = "partial"
	results := updateCheckedKey(t, ui, ws, ids[0])
	updateCheckedKey(t, ui, ws, results[0])
	firstRecovery := ws.recoveries[0]
	ws.mode = ""
	results = runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyRecover{Dialog: d, Retry: true}))
	updateCheckedKey(t, ui, ws, results[0])
	require.Equal(t, firstRecovery, ws.recoveries[1])
	require.Equal(t, original.OperationID, firstRecovery.OperationID)
	require.Equal(t, original.Target, firstRecovery.Target)
	require.Len(t, ws.checks, 1)
	require.Len(t, ws.saves, 2)
	require.Zero(t, ws.preferredModelCalls, "original selection generation is stale")
}
func TestCheckedKeyUICancelAndWrongCheckCannotSave(t *testing.T) {
	for _, mode := range []string{"close-before-ID", "close-before-check", "wrong-check", "check-error"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws, selection := newCheckedKeyUI(t)
			openCheckedKey(t, ui, ws, selection)
			updateCheckedKey(t, ui, ws, tea.PasteMsg{Content: "source-A"})
			ids := updateCheckedKey(t, ui, ws, tea.KeyPressMsg{Code: tea.KeyEnter})
			require.Len(t, ids, 1)
			require.IsType(t, apiKeyIDsMsg{}, ids[0])
			if mode == "close-before-ID" {
				ui.handleDialogAction(dialog.ActionClose{})
				updateCheckedKey(t, ui, ws, ids[0])
				require.Empty(t, ws.checks)
				return
			}
			ws.mode = mode
			_, cmd := ui.Update(ids[0])
			if mode == "close-before-check" {
				ui.handleDialogAction(dialog.ActionClose{})
			}
			messages := runCheckedKeyCmd(ws, cmd)
			require.Len(t, messages, 1)
			updateCheckedKey(t, ui, ws, messages[0])
			require.Nil(t, ui.apiKeyOperations[ws].checked.CheckedTarget)
			require.Empty(t, ws.saves)
		})
	}
}

func (w *checkedKeyUIWorkspace) UpdatePreferredModel(scope config.Scope, typ config.SelectedModelType, model config.SelectedModel, owner providerregistry.RegistrationOwner) (config.AgentModelState, error) {
	require.True(w.t, w.allowIO, "model persistence outside Cmd")
	return w.testWorkspace.UpdatePreferredModel(scope, typ, model, owner)
}
func (w *checkedKeyUIWorkspace) UpdateAgentModel(ctx context.Context, state config.AgentModelState) error {
	require.True(w.t, w.allowIO, "agent update outside Cmd")
	return w.testWorkspace.UpdateAgentModel(ctx, state)
}
func TestCheckedKeyUIHostOAuthNeverBecomesKeyFallback(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	owner := selection.ProviderOwner
	owner.HasOAuth = true
	owner.OAuthAdapter = providerregistry.LoginBrowser
	cfg := &config.Config{Models: ws.cfg.Models, Providers: ws.cfg.Providers}
	require.NoError(t, cfg.BindProviderSurfaceOwners([]providerregistry.Surface{{ID: owner.ProviderID, Owner: &owner}}))
	ws.cfg = cfg
	selection.ProviderOwner = owner
	ws.snapshot.Providers[0].Owner = providerauth.PublicOwner(owner)
	ws.snapshot.Providers[0].CredentialSlots = nil // This fixture models an OAuth-only owner.
	messages := runCheckedKeyCmd(ws, ui.openAuthenticationDialog(selection))
	require.Len(t, messages, 1)
	_, next := ui.Update(messages[0])
	require.NotNil(t, next, "OAuth status runs through a separate command")
	require.IsType(t, &dialog.OAuthLogin{}, ui.dialog.Dialog(dialog.LoginID))
	require.False(t, ui.dialog.ContainsDialog(dialog.APIKeyInputID))
	require.Empty(t, ws.checks)
	require.Empty(t, ws.saves)
}
func TestCheckedKeyUIExactCheckRetryAndReplacementDialog(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	d := openCheckedKey(t, ui, ws, selection)
	ws.mode = "check-error"
	checked := checkKeyInput(t, ui, ws, d, "source-A")
	updateCheckedKey(t, ui, ws, checked)
	original := ws.checks[0]
	ui.modelSelectionGen++
	replacement := openCheckedKey(t, ui, ws, selection)
	require.NotSame(t, d, replacement)
	replacement.HandleMsg(tea.PasteMsg{Content: "source-B"})
	ws.mode = ""
	messages := runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyRetry{Dialog: replacement}))
	updateCheckedKey(t, ui, ws, messages[0])
	require.Equal(t, original, ws.checks[1])
	require.Equal(t, "source-A", ws.checks[1].Source)
	saved := saveKeyInput(t, ui, ws, replacement)
	updateCheckedKey(t, ui, ws, saved)
	require.Zero(t, ws.preferredModelCalls, "reopened dialog cannot transfer the old continuation to a new generation")
}

func TestCheckedKeyUIInitialAdmissionFencesNewerInvalidSelection(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	d := openCheckedKey(t, ui, ws, selection)
	updateCheckedKey(t, ui, ws, tea.PasteMsg{Content: "source-A"})
	ids := updateCheckedKey(t, ui, ws, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Len(t, ids, 1)
	require.IsType(t, apiKeyIDsMsg{}, ids[0])
	invalid := selection
	invalid.ProviderOwner.ProviderID = "wrong-owner"
	ui.handleSelectModel(invalid)
	require.True(t, ui.apiKeyDialogOpen(d), "invalid new selection leaves the old dialog open")
	updateCheckedKey(t, ui, ws, ids[0])
	require.Empty(t, ws.checks, "initial stale check must not execute source")
	messages := runCheckedKeyCmd(ws, ui.handleDialogAction(dialog.ActionAPIKeyRetry{Dialog: d}))
	require.Len(t, messages, 1)
	updateCheckedKey(t, ui, ws, messages[0])
	require.Len(t, ws.checks, 1, "explicit original retry is allowed")
	require.Equal(t, "source-A", ws.checks[0].Source)
}
func TestCheckedKeyUIResolvedReceiptDoesNotBlockNewOAuthRoute(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	owner := selection.ProviderOwner
	owner.HasOAuth = true
	owner.OAuthAdapter = providerregistry.LoginBrowser
	cfg := &config.Config{Providers: ws.cfg.Providers, Models: ws.cfg.Models}
	require.NoError(t, cfg.BindProviderSurfaceOwners([]providerregistry.Surface{{ID: owner.ProviderID, Owner: &owner}}))
	ws.cfg = cfg
	selection.ProviderOwner = owner
	ws.snapshot.Providers[0].Owner = providerauth.PublicOwner(owner)
	ws.snapshot.Providers[0].CredentialSlots = nil // This fixture models an OAuth-only owner.
	ui.apiKeyOperations = map[workspace.Workspace]*apiKeyOperation{ws: {resolved: true, message: "old key saved"}}
	messages := runCheckedKeyCmd(ws, ui.openAuthenticationDialog(selection))
	require.Len(t, messages, 1)
	ui.Update(messages[0])
	require.Nil(t, ui.apiKeyOperations[ws])
	require.IsType(t, &dialog.OAuthLogin{}, ui.dialog.Dialog(dialog.LoginID))
	require.False(t, ui.dialog.ContainsDialog(dialog.APIKeyInputID), "host OAuth family must not become editable key input")
}

func TestCheckedKeyUIRequiresDeclaredCredentialSlot(t *testing.T) {
	ui, ws, selection := newCheckedKeyUI(t)
	ws.snapshot.Providers[0].CredentialSlots = nil
	d := openCheckedKey(t, ui, ws, selection)
	updateCheckedKey(t, ui, ws, tea.PasteMsg{Content: "source-must-not-be-used"})
	messages := updateCheckedKey(t, ui, ws, tea.KeyPressMsg{Code: tea.KeyEnter})
	require.Empty(t, messages)
	require.Empty(t, ws.checks)
	require.Empty(t, ws.saves)
	require.Nil(t, ui.apiKeyOperations[ws])
	require.Empty(t, d.TakeSource(), "undeclared input must remain disabled")
}
