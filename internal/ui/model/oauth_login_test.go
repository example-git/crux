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
	"github.com/example-git/crux/internal/question"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/require"
)

type oauthUIWorkspace struct {
	*checkedKeyUIWorkspace
	state           providerauth.OAuthLoginState
	begins          []providerauth.OAuthLoginRef
	submissions     []providerauth.OAuthLoginCodeRequest
	completes       []providerauth.OAuthLoginRef
	cancels         []providerauth.OAuthLoginRef
	loginRecoveries []workspace.ProviderAuthenticationRecoveryRequest
	failure         string
}

func (w *oauthUIWorkspace) BeginProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef) (providerauth.OAuthLoginState, error) {
	require.True(w.t, w.allowIO, "Begin outside Cmd")
	w.begins = append(w.begins, ref)
	if w.failure == "unavailable" {
		return providerauth.OAuthLoginState{}, providerauth.ErrOAuthLoginUnavailable
	}
	if w.state.Login.LoginID == "" {
		w.state = providerauth.OAuthLoginState{Login: ref, Sequence: 1, Phase: providerauth.OAuthLoginWaitingCode, AuthorizationURL: "https://synthetic.example/auth?state=retained"}
	}
	if w.failure == "begin-lost" {
		return providerauth.OAuthLoginState{}, errors.New("synthetic transport lost")
	}
	return w.state, ctx.Err()
}
func (w *oauthUIWorkspace) WaitProviderOAuthLogin(ctx context.Context, ref providerauth.OAuthLoginRef, after uint64) (providerauth.OAuthLoginState, error) {
	require.True(w.t, w.allowIO, "Wait outside Cmd")
	require.Equal(w.t, w.state.Login, ref)
	if w.state.Sequence <= after {
		return w.state, context.DeadlineExceeded
	}
	return w.state, ctx.Err()
}
func (w *oauthUIWorkspace) SubmitProviderOAuthLoginCode(ctx context.Context, request providerauth.OAuthLoginCodeRequest) (providerauth.OAuthLoginState, error) {
	require.True(w.t, w.allowIO, "Submit outside Cmd")
	w.submissions = append(w.submissions, request)
	w.state = providerauth.OAuthLoginState{Login: request.Login, Sequence: 2, Phase: providerauth.OAuthLoginAuthorized}
	if w.failure == "submit-lost" {
		return providerauth.OAuthLoginState{}, errors.New("synthetic transport lost")
	}
	return w.state, ctx.Err()
}
func (w *oauthUIWorkspace) CancelProviderOAuthLogin(_ context.Context, ref providerauth.OAuthLoginRef) (providerauth.OAuthLoginState, error) {
	require.True(w.t, w.allowIO, "Cancel outside Cmd")
	w.cancels = append(w.cancels, ref)
	w.state = providerauth.OAuthLoginState{Login: ref, Sequence: 3, Phase: providerauth.OAuthLoginCanceled}
	return w.state, providerauth.ErrOAuthLogin
}
func (w *oauthUIWorkspace) CompleteProviderOAuthLogin(_ context.Context, ref providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "Complete outside Cmd")
	w.completes = append(w.completes, ref)
	return w.loginOutcome(ref)
}
func (w *oauthUIWorkspace) loginOutcome(ref providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	out := providerauth.MutationOutcome{OperationID: ref.OperationID, LoginID: ref.LoginID, Previous: ref.Target, Progress: providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}}
	if w.failure == "complete-lost" {
		return providerauth.MutationOutcome{}, errors.New("synthetic transport lost")
	}
	if w.failure == "partial" {
		return out, providerauth.ErrMutation
	}
	current := ref.Target
	current.Generation.Sequence++
	status := providerauth.Status{Owner: current.Owner, Configured: true, AccountState: "none", Credentials: []providerauth.CredentialStatus{{Kind: "api-key", State: "configured"}, {Kind: "oauth", State: "present"}}}
	out.Change = &providerauth.Change{OperationID: ref.OperationID, Previous: ref.Target, Current: providerauth.AccountsState{Target: current, Status: status, Accounts: []providerauth.AccountSummary{}}}
	if w.failure == "wrong-receipt" {
		out.LoginID = strings.Repeat("f", 32)
	}
	if w.failure == "historical" {
		out.Superseded = true
	}
	return out, nil
}
func (w *oauthUIWorkspace) RecoverProviderAuthentication(_ context.Context, request workspace.ProviderAuthenticationRecoveryRequest) (providerauth.MutationOutcome, error) {
	require.True(w.t, w.allowIO, "Recover outside Cmd")
	w.loginRecoveries = append(w.loginRecoveries, request)
	return w.loginOutcome(w.completes[0])
}
func newOAuthUI(t *testing.T) (*UI, *oauthUIWorkspace, dialog.ActionSelectModel) {
	ui, key, selection := newCheckedKeyUI(t)
	owner := selection.ProviderOwner
	owner.HasOAuth = true
	owner.OAuthAdapter = providerregistry.LoginHostedPaste
	owner.OAuthFlowID = "synthetic-flow"
	cfg := &config.Config{Providers: key.cfg.Providers, Models: key.cfg.Models}
	require.NoError(t, cfg.BindProviderSurfaceOwners([]providerregistry.Surface{{ID: owner.ProviderID, Name: "Remote OAuth", Owner: &owner}}))
	key.cfg = cfg
	selection.ProviderOwner = owner
	key.snapshot.Providers[0].Owner = providerauth.PublicOwner(owner)
	ws := &oauthUIWorkspace{checkedKeyUIWorkspace: key}
	ui.com.Workspace = ws
	ui.oauthOpenURL = func(rawURL string) error {
		require.True(t, ws.allowIO, "browser outside Cmd")
		require.Contains(t, rawURL, "synthetic.example")
		return nil
	}
	return ui, ws, selection
}
func oauthUICommand(ws *oauthUIWorkspace, cmd tea.Cmd) []tea.Msg {
	messages := runCheckedKeyCmd(ws.checkedKeyUIWorkspace, cmd)
	var relevant []tea.Msg
	for _, msg := range messages {
		switch msg.(type) {
		case busyStateMsg, lspStatesMsg:
			// Cache refreshes execute under the same I/O assertion, but are
			// independent of this focused login state-machine driver.
		default:
			relevant = append(relevant, msg)
		}
	}
	return relevant
}
func oauthUIUpdate(ui *UI, ws *oauthUIWorkspace, msg tea.Msg) []tea.Msg {
	// Status-dismissal timers are unrelated to login state transitions.
	if _, ok := msg.(util.InfoMsg); ok {
		return nil
	}
	ui.agentBusyCache.set(false)
	ui.yoloCache.set(false)
	ui.lspCheckedAt = time.Now()
	_, cmd := ui.Update(msg)
	return oauthUICommand(ws, cmd)
}
func openOAuthUI(t *testing.T, ui *UI, ws *oauthUIWorkspace, selection dialog.ActionSelectModel) (*dialog.OAuthLogin, []tea.Msg) {
	t.Helper()
	owner := providerauth.PublicOwner(selection.ProviderOwner)
	messages := oauthUICommand(ws, ui.openOAuthAuthentication(&selection, &owner))
	require.Len(t, messages, 1)
	messages = oauthUIUpdate(ui, ws, messages[0])
	require.Len(t, messages, 1)
	messages = oauthUIUpdate(ui, ws, messages[0])
	require.Len(t, messages, 1)
	d := ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
	return d, messages
}
func settleOAuthUI(t *testing.T, ui *UI, ws *oauthUIWorkspace, messages []tea.Msg) {
	t.Helper()
	for steps := 0; len(messages) > 0; steps++ {
		require.Less(t, steps, 40, "UI commands must reach a wait or terminal result")
		msg := messages[0]
		messages = messages[1:]
		messages = append(messages, oauthUIUpdate(ui, ws, msg)...)
	}
}
func submitOAuthUI(t *testing.T, ui *UI, ws *oauthUIWorkspace, d *dialog.OAuthLogin, input string) []tea.Msg {
	t.Helper()
	d.HandleMsg(tea.PasteMsg{Content: input})
	action := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	require.IsType(t, dialog.ActionOAuthLoginSubmit{}, action)
	return oauthUICommand(ws, ui.handleDialogAction(action))
}
func TestOAuthUIHostedOriginalInputAndAcknowledgedContinuation(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	require.Empty(t, ws.completes)
	require.Empty(t, ws.cancels, "a request wait timeout does not cancel the owner login")
	messages = submitOAuthUI(t, ui, ws, d, "code=synthetic-private&state=retained")
	require.Zero(t, ws.preferredModelCalls)
	settleOAuthUI(t, ui, ws, messages)
	require.Len(t, ws.begins, 1)
	require.Len(t, ws.submissions, 1)
	require.Len(t, ws.completes, 1)
	require.Equal(t, ws.begins[0], ws.submissions[0].Login)
	require.Equal(t, ws.begins[0], ws.completes[0])
	require.Equal(t, "code=synthetic-private&state=retained", ws.submissions[0].Input)
	require.Equal(t, 1, ws.preferredModelCalls)
	require.Equal(t, 1, ws.updateAgentCalls)
	require.NotContains(t, fmt.Sprintf("%#v", ui.oauthLogins[ws]), "synthetic-private")
}
func TestOAuthUIExactRetriesDoNotMintNewLoginOrCode(t *testing.T) {
	for _, failure := range []string{"begin-lost", "submit-lost", "complete-lost", "partial"} {
		t.Run(failure, func(t *testing.T) {
			ui, ws, selection := newOAuthUI(t)
			if failure == "begin-lost" {
				ws.failure = failure
			}
			d, messages := openOAuthUI(t, ui, ws, selection)
			settleOAuthUI(t, ui, ws, messages)
			if failure == "begin-lost" {
				original := ws.begins[0]
				ws.failure = ""
				settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRetry{Dialog: d})))
				require.Equal(t, []providerauth.OAuthLoginRef{original, original}, ws.begins)
			} else {
				ws.failure = failure
			}
			messages = submitOAuthUI(t, ui, ws, d, "code=original&state=retained")
			settleOAuthUI(t, ui, ws, messages)
			if failure != "begin-lost" {
				require.Zero(t, ws.preferredModelCalls)
				ws.failure = ""
				settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRetry{Dialog: d})))
			}
			if failure == "submit-lost" {
				require.Equal(t, ws.submissions[0], ws.submissions[1])
			}
			if failure == "complete-lost" || failure == "partial" {
				require.Equal(t, ws.completes[0], ws.completes[1])
			}
			require.Equal(t, 1, ws.preferredModelCalls)
		})
	}
}
func TestOAuthUIUnacknowledgedClosedOrStaleCompletionNeverContinues(t *testing.T) {
	for _, mode := range []string{"wrong-receipt", "partial", "historical", "closed", "new-workspace", "new-selection"} {
		t.Run(mode, func(t *testing.T) {
			ui, ws, selection := newOAuthUI(t)
			d, messages := openOAuthUI(t, ui, ws, selection)
			settleOAuthUI(t, ui, ws, messages)
			messages = submitOAuthUI(t, ui, ws, d, "code=original&state=retained")
			require.Len(t, messages, 1)
			ws.failure = mode
			messages = oauthUIUpdate(ui, ws, messages[0])
			var completed oauthLoginResultMsg
			for _, msg := range messages {
				if result, ok := msg.(oauthLoginResultMsg); ok {
					completed = result
				}
			}
			require.Equal(t, "complete", completed.kind)
			switch mode {
			case "closed":
				oauthUICommand(ws, ui.handleDialogAction(dialog.ActionClose{}))
			case "new-workspace":
				other, otherWS, _ := newOAuthUI(t)
				otherWS.allowIO = true
				ui.com.Workspace = other.com.Workspace
			case "new-selection":
				ui.modelSelectionGen++
			}
			settleOAuthUI(t, ui, ws, []tea.Msg{completed})
			require.Zero(t, ws.preferredModelCalls)
			require.Empty(t, ws.cancels, "dispatched completion survives dialog closure")
		})
	}
}
func TestOAuthUICloseCancelsOriginalAndLegacyTokenCannotPersist(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionClose{})))
	require.False(t, ui.oauthDialogOpen(d))
	require.Equal(t, ws.begins, ws.cancels)
	require.Empty(t, ws.completes)
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.LoginDoneMsg{})))
	require.Empty(t, ws.saves)
	require.Empty(t, ws.completes)
}

func TestOAuthUIRawPasteArrivesBeforeInlineEditorAndNormalization(t *testing.T) {
	for _, source := range []string{"code=one\r\nstate=original\x00", strings.Repeat("x", 16*1024+1), string([]byte{'x', 0xff, '\t', 'y'})} {
		ui, ws, selection := newOAuthUI(t)
		d, messages := openOAuthUI(t, ui, ws, selection)
		settleOAuthUI(t, ui, ws, messages)
		ui.activeInline = dialog.NewQuestionForm(ui.com.Styles, question.Request{Questions: []question.Question{{ID: "unrelated", Type: question.TypeFreeText, Text: "An unrelated prompt"}}})
		ui.focus = uiFocusEditor
		oauthUIUpdate(ui, ws, tea.PasteMsg{Content: source})
		require.Equal(t, source, d.TakeSource(), "main UI must preserve original bytes before validation")
		require.Empty(t, ws.submissions)
		settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionClose{})))
	}
}

func TestOAuthUIInitialAdmissionFencesNewerSelection(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	owner := providerauth.PublicOwner(selection.ProviderOwner)
	status := oauthUICommand(ws, ui.openOAuthAuthentication(&selection, &owner))
	ids := oauthUIUpdate(ui, ws, status[0])
	require.Len(t, ids, 1)
	ui.modelSelectionGen++
	settleOAuthUI(t, ui, ws, oauthUIUpdate(ui, ws, ids[0]))
	require.Empty(t, ws.begins)
	op := ui.oauthLogins[ws]
	ref := op.ref
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRetry{Dialog: op.dialog})))
	require.Equal(t, []providerauth.OAuthLoginRef{ref}, ws.begins, "explicit retry retains original identity")
}

func TestOAuthUIUnavailableBeforeSaveAllowsExplicitReload(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	ws.failure = "unavailable"
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	require.True(t, ui.oauthLogins[ws].ended)
	_, ok := d.HandleMsg(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}).(dialog.ActionOAuthLoginReload)
	require.True(t, ok, "reload action must be reachable")
	ws.failure = ""
	reloaded := oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginReload{Dialog: d}))
	require.Nil(t, ui.oauthLogins[ws])
	require.NotEmpty(t, reloaded)
	require.Len(t, ws.begins, 1, "reload only requests fresh status until its reply is handled")
}

func TestOAuthUIDifferentProviderNeverResumesAnotherOwner(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	original := ui.oauthLogins[ws]
	other := providerauth.PublicOwner(selection.ProviderOwner)
	other.ProviderID = "different-provider"
	result := oauthUICommand(ws, ui.openOAuthAuthentication(nil, &other))
	require.Len(t, result, 1)
	require.Contains(t, result[0].(util.InfoMsg).Msg, "different-provider")
	require.Contains(t, result[0].(util.InfoMsg).Msg, original.ref.Target.Owner.ProviderID)
	require.Same(t, d, original.dialog)
	require.Len(t, ws.begins, 1)
	require.Empty(t, ws.submissions)
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionClose{})))
	require.True(t, original.ended)
	newStatus := oauthUICommand(ws, ui.openOAuthAuthentication(nil, &other))
	require.Len(t, newStatus, 1)
	require.IsType(t, oauthLoginStatusMsg{}, newStatus[0], "an ended login must not block a different explicit provider request")
	require.NotSame(t, d, ui.dialog.Dialog(dialog.LoginID))
	require.Equal(t, ws.begins[0], original.ref, "starting fresh status does not rebind the old reference")
}

func TestOAuthUIGenericReloadReturnsToProviderPicker(t *testing.T) {
	ui, ws, _ := newOAuthUI(t)
	status := oauthUICommand(ws, ui.openLoginDialog())
	settleOAuthUI(t, ui, ws, oauthUIUpdate(ui, ws, status[0]))
	d := ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
	choice := d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter})
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(choice)))
	require.True(t, ui.oauthLogins[ws].chooseProvider)
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionClose{})))
	ui.openLoginDialog()
	d = ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginReload{Dialog: d})))
	d = ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
	require.IsType(t, dialog.ActionOAuthLoginSelect{}, d.HandleMsg(tea.KeyPressMsg{Code: tea.KeyEnter}))
	require.Len(t, ws.begins, 1, "reload does not silently restart the previously chosen owner")
}

func TestOAuthUIRecoveryRetainsOriginalReceiptAndExactAttempt(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	ws.failure = "partial"
	settleOAuthUI(t, ui, ws, submitOAuthUI(t, ui, ws, d, "code=original&state=retained"))
	op := ui.oauthLogins[ws]
	require.True(t, op.completeSent)
	require.False(t, op.resolved)
	ids := oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRecover{Dialog: d}))
	require.Len(t, ids, 1)
	results := oauthUIUpdate(ui, ws, ids[0])
	require.Len(t, results, 1)
	original := results[0].(oauthLoginResultMsg)
	require.Equal(t, op.ref.OperationID, original.recovery.OperationID)
	require.Equal(t, op.ref.Target, original.recovery.Target)
	forged := original
	wrong := *forged.recovery
	wrong.RecoveryID = strings.Repeat("f", 32)
	forged.recovery = &wrong
	require.Empty(t, oauthUIUpdate(ui, ws, forged))
	require.True(t, op.busy, "wrong recovery identity cannot consume the actual in-flight reply")
	settleOAuthUI(t, ui, ws, oauthUIUpdate(ui, ws, original))
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRecover{Dialog: d, Retry: true})))
	require.Len(t, ws.loginRecoveries, 2)
	require.Equal(t, ws.loginRecoveries[0], ws.loginRecoveries[1])
	require.Zero(t, ws.preferredModelCalls)
	ws.failure = ""
	settleOAuthUI(t, ui, ws, oauthUICommand(ws, ui.handleDialogAction(dialog.ActionOAuthLoginRecover{Dialog: d})))
	require.Len(t, ws.loginRecoveries, 3)
	require.EqualValues(t, 2, ws.loginRecoveries[2].RecoverySequence)
	require.NotEqual(t, ws.loginRecoveries[0].RecoveryID, ws.loginRecoveries[2].RecoveryID)
	require.Len(t, ws.begins, 1)
	require.Len(t, ws.submissions, 1)
	require.Len(t, ws.completes, 1)
	require.Equal(t, 1, ws.preferredModelCalls)
}

func TestOAuthUIOversizedPasteIsRejectedWithoutTruncation(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	d, messages := openOAuthUI(t, ui, ws, selection)
	settleOAuthUI(t, ui, ws, messages)
	settleOAuthUI(t, ui, ws, submitOAuthUI(t, ui, ws, d, strings.Repeat("x", 16*1024+1)))
	require.Empty(t, ws.submissions)
	require.Empty(t, ws.completes)
	require.Contains(t, ui.oauthLogins[ws].message, "16 KiB")
	source := strings.Repeat("x", 16*1024)
	settleOAuthUI(t, ui, ws, submitOAuthUI(t, ui, ws, d, source))
	require.Len(t, ws.submissions, 1)
	require.Equal(t, source, ws.submissions[0].Input)
}

func TestOAuthUIMissingRequestedOwnerIsReloadableWithoutSubstitution(t *testing.T) {
	ui, ws, selection := newOAuthUI(t)
	owner := providerauth.PublicOwner(selection.ProviderOwner)
	status := oauthUICommand(ws, ui.openOAuthAuthentication(&selection, &owner))[0].(oauthLoginStatusMsg)
	status.snapshot.Providers = []providerauth.Status{}
	results := oauthUIUpdate(ui, ws, status)
	require.Len(t, results, 1)
	require.Contains(t, results[0].(util.InfoMsg).Msg, "original owner")
	d := ui.dialog.Dialog(dialog.LoginID).(*dialog.OAuthLogin)
	require.IsType(t, dialog.ActionOAuthLoginReload{}, d.HandleMsg(tea.KeyPressMsg{Code: 'n', Mod: tea.ModCtrl}))
	require.Empty(t, ws.begins)
}
