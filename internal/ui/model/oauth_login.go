package model

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callbackrelay"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/ui/dialog"
	"github.com/example-git/crux/internal/ui/util"
	"github.com/example-git/crux/internal/workspace"
	"github.com/pkg/browser"
)

type oauthLoginRead struct {
	workspace        workspace.Workspace
	dialog           *dialog.OAuthLogin
	selection        *dialog.ActionSelectModel
	expected         *providerauth.Owner
	generation       uint64
	snapshot         providerauth.Snapshot
	recordedOwner    providerauth.Owner
	recorded         []providerauth.OAuthLoginRecordedResult
	recordedSequence uint64
	recordedLoaded   bool
	recordedNotice   string
	abandonRequest   *providerauth.OAuthLoginAbandonRequest
	cancel           context.CancelFunc
}

type oauthLoginOperation struct {
	chooseProvider                                                           bool
	workspace                                                                workspace.Workspace
	dialog                                                                   *dialog.OAuthLogin
	selection                                                                *dialog.ActionSelectModel
	generation                                                               uint64
	ref                                                                      providerauth.OAuthLoginRef
	binding                                                                  providerauth.OAuthLoginBindRequest
	submission                                                               providerauth.OAuthLoginCodeRequest
	state                                                                    providerauth.OAuthLoginState
	outcome                                                                  providerauth.MutationOutcome
	progress                                                                 providerauth.MutationProgress
	attempt, relayAttempt                                                    uint64
	kind, message, openedURL                                                 string
	recordedOperationID                                                      string
	recordedWorkspaceID                                                      string
	busy, preparing, began, completeSent, resolved, continued, closed, ended bool
	cancel                                                                   context.CancelFunc
	relay                                                                    *callbackrelay.Relay
	relayBusy                                                                bool
	recoverer                                                                workspace.ProviderAuthenticationRecoverer
	recovery                                                                 *workspace.ProviderAuthenticationRecoveryRequest
	recoverySequence                                                         uint64
	review                                                                   *authenticationReconciliation
}

func (*oauthLoginOperation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth UI session]"))
}

func oauthLoginReviewBusy(op *oauthLoginOperation) bool {
	return op != nil && op.review != nil && (op.review.preparing != nil || op.review.pending != "")
}
func oauthLoginReviewResolved(op *oauthLoginOperation) bool {
	return op != nil && op.review != nil && op.review.resolved
}

type oauthLoginStatusMsg struct {
	read     *oauthLoginRead
	snapshot providerauth.Snapshot
	err      error
}
type oauthLoginIDsMsg struct {
	operation *oauthLoginOperation
	attempt   uint64
	ids       [4]string
	recovery  bool
	err       error
}
type oauthLoginResultMsg struct {
	operation *oauthLoginOperation
	attempt   uint64
	ref       providerauth.OAuthLoginRef
	kind      string
	after     uint64
	state     providerauth.OAuthLoginState
	outcome   providerauth.MutationOutcome
	recovery  *workspace.ProviderAuthenticationRecoveryRequest
	err       error
}

func (oauthLoginResultMsg) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth UI reply]"))
}

type oauthLoginRelayMsg struct {
	operation *oauthLoginOperation
	attempt   uint64
	ref       providerauth.OAuthLoginRef
	relay     *callbackrelay.Relay
	input     string
	started   bool
	err       error
}

func (oauthLoginRelayMsg) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth callback reply]"))
}

type oauthLoginOpenMsg struct {
	operation *oauthLoginOperation
	ref       providerauth.OAuthLoginRef
	url       string
	err       error
}

func (oauthLoginOpenMsg) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth browser reply]"))
}

func (m *UI) oauthDialogOpen(d *dialog.OAuthLogin) bool {
	return d != nil && m.dialog != nil && m.dialog.Dialog(dialog.LoginID) == d
}
func closeOAuthRelay(relay *callbackrelay.Relay) tea.Cmd {
	if relay == nil {
		return nil
	}
	return func() tea.Msg { _ = relay.Close(); return nil }
}
func (m *UI) pruneOAuthLogins() tea.Cmd {
	var cmds []tea.Cmd
	for d, read := range m.oauthLoginReads {
		if read.workspace != m.com.Workspace || !m.oauthDialogOpen(d) {
			read.cancel()
			delete(m.oauthLoginReads, d)
		}
	}
	for _, op := range m.oauthLogins {
		if op.closed || op.workspace == m.com.Workspace && m.oauthDialogOpen(op.dialog) {
			continue
		}
		op.closed = true
		cmds = append(cmds, closeOAuthRelay(op.relay))
		if op.completeSent || op.resolved || op.ended {
			continue
		}
		if op.cancel != nil {
			op.cancel()
		}
		if op.began {
			cmds = append(cmds, m.dispatchOAuthLogin(op, "cancel"))
		} else {
			op.ended = true
			op.message = "Login closed before a request was dispatched."
		}
	}
	return tea.Batch(cmds...)
}

func (m *UI) openOAuthAuthentication(selection *dialog.ActionSelectModel, expected *providerauth.Owner) tea.Cmd {
	previous := m.oauthLogins[m.com.Workspace]
	if oauthLoginReviewBusy(previous) {
		return util.ReportWarn("Saved authentication review is still pending; its original request and receipt remain retained.")
	}
	if previous != nil && (previous.resolved || oauthLoginReviewResolved(previous) || previous.ended && !previous.completeSent && expected != nil && previous.ref.Target.Owner != *expected) {
		if m.oauthDialogOpen(previous.dialog) {
			m.dialog.CloseDialog(dialog.LoginID)
		}
		delete(m.oauthLogins, previous.workspace)
		previous = nil
	}
	if previous != nil && !previous.resolved && expected != nil && previous.ref.Target.Owner != *expected {
		return util.ReportError(fmt.Errorf("Sign-in for %s is still retained. Finish or close that login before starting sign-in for %s", previous.ref.Target.Owner.ProviderID, expected.ProviderID))
	}
	if m.dialog.ContainsDialog(dialog.LoginID) {
		m.dialog.BringToFront(dialog.LoginID)
		return nil
	}
	name := "Provider"
	if expected != nil {
		name = expected.ProviderID
	}
	if selection != nil {
		copy := *selection
		copy.Model = copy.Model.Clone()
		selection = &copy
		if selection.Provider.Name != "" {
			name = selection.Provider.Name
		}
	}
	if previous != nil && !previous.resolved {
		name = previous.ref.Target.Owner.ProviderID
	}
	d := dialog.NewOAuthLogin(m.com, m.state == uiOnboarding, name)
	m.dialog.OpenDialogWithGrace(d)
	if previous != nil && !previous.resolved {
		previous.dialog = d
		m.showOAuthLogin(previous)
		return nil
	}
	if m.oauthLoginReads == nil {
		m.oauthLoginReads = make(map[*dialog.OAuthLogin]*oauthLoginRead)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	read := &oauthLoginRead{workspace: m.com.Workspace, dialog: d, selection: selection, generation: m.modelSelectionGen, cancel: cancel}
	if expected != nil {
		value := *expected
		read.expected = &value
	}
	m.oauthLoginReads[d] = read
	ws := read.workspace
	return func() tea.Msg {
		defer cancel()
		snapshot, err := ws.ProviderAuthentication(ctx)
		return oauthLoginStatusMsg{read, snapshot, err}
	}
}
func (m *UI) completeOAuthLoginStatus(msg oauthLoginStatusMsg) tea.Cmd {
	r := msg.read
	if r == nil || m.oauthLoginReads[r.dialog] != r || r.workspace != m.com.Workspace || !m.oauthDialogOpen(r.dialog) {
		return nil
	}
	err := msg.err
	if err == nil {
		err = msg.snapshot.Validate()
	}
	if err == nil && r.selection != nil {
		err = r.selection.ValidateProviderOwner(r.workspace.Config())
		if r.generation != m.modelSelectionGen {
			err = providerauth.ErrStale
		}
	}
	if err != nil {
		r.dialog.SetPresentation(dialog.OAuthLoginPresentation{Message: "Could not load workspace sign-in: " + safeOAuthLoginError(err), Reload: true})
		return util.ReportError(errors.New("Could not load workspace sign-in: " + safeOAuthLoginError(err)))
	}
	r.snapshot = msg.snapshot
	var choices []dialog.OAuthLoginProvider
	for _, status := range msg.snapshot.Providers {
		if !status.Owner.HasOAuth {
			continue
		}
		name := status.Owner.ProviderID
		for _, surface := range r.workspace.ProviderSurfaces() {
			if surface.ID == name && surface.Name != "" {
				name = surface.Name
				break
			}
		}
		choices = append(choices, dialog.OAuthLoginProvider{Owner: status.Owner, Name: name})
	}
	if r.expected != nil {
		for _, choice := range choices {
			if choice.Owner == *r.expected {
				return m.beginOAuthLogin(r.dialog, *r.expected)
			}
		}
		message := "The requested provider is no longer available under its original owner. Reload workspace sign-in status."
		r.dialog.SetPresentation(dialog.OAuthLoginPresentation{Message: message, Reload: true})
		return util.ReportError(errors.New(message))
	}
	r.dialog.SetProviders(choices)
	message := "Choose the workspace provider to sign in. Login saves the authorized account on its credential owner."
	if len(choices) == 0 {
		message = "This workspace has no OAuth provider available for sign-in."
	}
	r.dialog.SetPresentation(dialog.OAuthLoginPresentation{Message: message, Reload: true})
	return nil
}
func (m *UI) startOAuthLogin(d *dialog.OAuthLogin, owner providerauth.Owner, originalWorkspaceID, originalOperationID string) tea.Cmd {
	r := m.oauthLoginReads[d]
	if r == nil || r.workspace != m.com.Workspace || !m.oauthDialogOpen(d) {
		return nil
	}
	if previous := m.oauthLogins[r.workspace]; previous != nil && !previous.resolved {
		return util.ReportError(errors.New("The original login remains retained; retry it or explicitly reload after it ends"))
	}
	found := false
	for _, status := range r.snapshot.Providers {
		if status.Owner == owner && owner.HasOAuth {
			found = true
			break
		}
	}
	if !found || r.expected != nil && *r.expected != owner {
		return util.ReportError(providerauth.ErrOwner)
	}
	if r.selection != nil {
		if r.generation != m.modelSelectionGen || providerauth.PublicOwner(r.selection.ProviderOwner) != owner {
			return util.ReportError(providerauth.ErrStale)
		}
		if err := r.selection.ValidateProviderOwner(r.workspace.Config()); err != nil {
			return util.ReportError(err)
		}
	}
	op := &oauthLoginOperation{recordedWorkspaceID: originalWorkspaceID, recordedOperationID: originalOperationID, workspace: r.workspace, dialog: d, selection: r.selection, chooseProvider: r.expected == nil && r.selection == nil, generation: r.generation, ref: providerauth.OAuthLoginRef{Target: providerauth.Target{WorkspaceID: r.snapshot.WorkspaceID, Owner: owner, Generation: r.snapshot.Generation}}, preparing: true, attempt: 1, kind: "begin", message: "Preparing sign-in for " + owner.ProviderID + "…"}
	if originalOperationID != "" {
		op.kind = "recover-result"
		op.message = "Recovering observed result from operation " + originalOperationID + "; no new exchange will run…"
	}
	if recoverer, ok := op.workspace.(workspace.ProviderAuthenticationRecoverer); ok && recoverer.CanRecoverProviderAuthentication() {
		op.recoverer = recoverer
	}
	if m.oauthLogins == nil {
		m.oauthLogins = make(map[workspace.Workspace]*oauthLoginOperation)
	}
	m.oauthLogins[op.workspace] = op
	d.SetProviders(nil)
	m.showOAuthLogin(op)
	return prepareOAuthLoginIDs(op, false)
}
func prepareOAuthLoginIDs(op *oauthLoginOperation, recovery bool) tea.Cmd {
	attempt := op.attempt
	return func() tea.Msg {
		var ids [4]string
		var err error
		for i := range ids {
			var bytes [16]byte
			if _, err = rand.Read(bytes[:]); err != nil {
				break
			}
			ids[i] = hex.EncodeToString(bytes[:])
			if recovery {
				break
			}
		}
		return oauthLoginIDsMsg{op, attempt, ids, recovery, err}
	}
}
func (m *UI) completeOAuthLoginIDs(msg oauthLoginIDsMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.oauthLogins[op.workspace] != op || !op.preparing || op.attempt != msg.attempt {
		return nil
	}
	op.preparing = false
	if msg.err != nil {
		op.message = "Could not prepare login identifiers; no request was sent."
		m.showOAuthLogin(op)
		return util.ReportError(errors.New(op.message))
	}
	if msg.recovery {
		op.recoverySequence++
		op.recovery = &workspace.ProviderAuthenticationRecoveryRequest{OperationID: op.ref.OperationID, Target: op.ref.Target, RecoveryID: msg.ids[0], RecoverySequence: op.recoverySequence}
	} else {
		op.ref.LoginID, op.ref.OperationID = msg.ids[0], msg.ids[1]
		op.binding = providerauth.OAuthLoginBindRequest{Login: op.ref, BindingID: msg.ids[2]}
		op.submission = providerauth.OAuthLoginCodeRequest{Login: op.ref, SubmissionID: msg.ids[3]}
	}
	if !msg.recovery && op.closed || op.workspace != m.com.Workspace || !m.oauthDialogOpen(op.dialog) {
		m.showOAuthLogin(op)
		return nil
	}
	if msg.recovery {
		return m.dispatchOAuthLogin(op, "recovery")
	}
	if op.selection != nil && op.generation != m.modelSelectionGen {
		op.kind = "begin"
		if op.recordedOperationID != "" {
			op.kind = "recover-result"
		}
		op.message = "The model selection changed before login began. Retry explicitly to continue the original login."
		m.showOAuthLogin(op)
		return util.ReportWarn(op.message)
	}
	if op.recordedOperationID != "" {
		return m.dispatchOAuthLogin(op, "recover-result")
	}
	return m.dispatchOAuthLogin(op, "begin")
}

func (m *UI) dispatchOAuthLogin(op *oauthLoginOperation, kind string) tea.Cmd {
	if op.cancel != nil {
		op.cancel()
	}
	op.busy = true
	op.kind = kind
	op.attempt++
	if kind == "begin" || kind == "recover-result" {
		op.began = true
	}
	if kind == "complete" || kind == "recovery" {
		op.completeSent = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	op.cancel = cancel
	ws, ref, binding, submission, attempt, after, recoverer := op.workspace, op.ref, op.binding, op.submission, op.attempt, op.state.Sequence, op.recoverer
	originalWorkspaceID, originalOperationID := op.recordedWorkspaceID, op.recordedOperationID
	var recovery *workspace.ProviderAuthenticationRecoveryRequest
	if kind == "recovery" {
		value := *op.recovery
		recovery = &value
	}
	if kind == "complete" {
		op.message = "Saving the authorized account; waiting for the workspace acknowledgement…"
	}
	if kind == "recovery" {
		op.message = "Attempting explicit publication recovery of the original saved login…"
	}
	if kind == "cancel" {
		op.message = "Closing the original login…"
	}
	m.showOAuthLogin(op)
	return func() tea.Msg {
		defer cancel()
		result := oauthLoginResultMsg{operation: op, attempt: attempt, ref: ref, kind: kind, after: after, recovery: recovery}
		switch kind {
		case "begin":
			result.state, result.err = ws.BeginProviderOAuthLogin(ctx, ref)
		case "recover-result":
			capability, ok := ws.(workspace.ProviderOAuthRecovery)
			if !ok {
				result.err = errors.New("recorded OAuth recovery is unavailable")
			} else {
				result.state, result.err = capability.RecoverProviderOAuthLogin(ctx, providerauth.OAuthLoginRecoveryRequest{Login: ref, OriginalWorkspaceID: originalWorkspaceID, OriginalOperationID: originalOperationID})
			}
		case "bind":
			result.state, result.err = ws.BindProviderOAuthLogin(ctx, binding)
		case "code":
			result.state, result.err = ws.SubmitProviderOAuthLoginCode(ctx, submission)
		case "wait":
			result.state, result.err = ws.WaitProviderOAuthLogin(ctx, ref, after)
		case "cancel":
			result.state, result.err = ws.CancelProviderOAuthLogin(ctx, ref)
		case "complete":
			result.outcome, result.err = ws.CompleteProviderOAuthLogin(ctx, ref)
		case "recovery":
			result.outcome, result.err = recoverer.RecoverProviderAuthentication(ctx, *recovery)
		default:
			result.err = providerauth.ErrReceiptUnverified
		}
		return result
	}
}
func (m *UI) completeOAuthLoginResult(msg oauthLoginResultMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.oauthLogins[op.workspace] != op || !op.busy || op.attempt != msg.attempt || op.kind != msg.kind || op.ref != msg.ref {
		return nil
	}
	if msg.kind == "recovery" && (msg.recovery == nil || op.recovery == nil || *msg.recovery != *op.recovery) {
		return nil
	}
	op.busy = false
	op.cancel = nil
	if msg.kind == "complete" || msg.kind == "recovery" {
		return m.completeOAuthLoginReceipt(msg)
	}
	response := proto.ProviderOAuthLoginResponse{Login: op.ref}
	if msg.state.Login.LoginID != "" {
		response.State = &msg.state
	}
	if msg.err != nil {
		response.Error = proto.NewProviderAuthenticationError(msg.err)
	}
	var validation error
	switch msg.kind {
	case "begin":
		validation = response.ValidateBegin(op.ref)
	case "recover-result":
		response.RecoveryWorkspaceID, response.RecoveryOperationID = op.recordedWorkspaceID, op.recordedOperationID
		validation = response.ValidateRecover(providerauth.OAuthLoginRecoveryRequest{Login: op.ref, OriginalWorkspaceID: op.recordedWorkspaceID, OriginalOperationID: op.recordedOperationID})
	case "bind":
		response.BindingID, response.Port = op.binding.BindingID, op.binding.Port
		validation = response.ValidateBind(op.binding)
	case "code":
		response.SubmissionID = op.submission.SubmissionID
		validation = response.ValidateCode(op.submission)
	case "wait":
		validation = response.ValidateWait(op.ref, msg.after)
	case "cancel":
		validation = response.ValidateCancel(op.ref)
	}
	if response.State != nil && !response.State.MatchesOAuthLoginRecovery(op.recordedWorkspaceID, op.recordedOperationID) {
		validation = providerauth.ErrReceiptUnverified
	}
	err := msg.err
	if validation != nil {
		err = providerauth.ErrReceiptUnverified
	} else if response.State != nil {
		op.state = msg.state
	}
	if err != nil {
		op.message = "Sign-in did not complete: " + safeOAuthLoginError(err)
		if op.recordedOperationID != "" && errors.Is(err, providerauth.ErrOAuthLogin) {
			op.message = "Recorded-result recovery did not complete; no new exchange was run. Reload recorded results or explicitly review saved authentication."
		}
		if validation == nil && !op.completeSent && (response.State != nil && oauthLoginEnded(op.state.Phase) || errors.Is(err, providerauth.ErrOAuthLoginUnavailable) || errors.Is(err, providerauth.ErrStale) || errors.Is(err, providerauth.ErrOwner)) {
			op.ended = true
		}
		if validation == nil && op.closed && op.state.Phase == providerauth.OAuthLoginCanceled {
			op.message = "Sign-in canceled."
			m.showOAuthLogin(op)
			return tea.Batch(closeOAuthRelay(op.relay), util.ReportInfo(op.message))
		}
		m.showOAuthLogin(op)
		if op.ended {
			return tea.Batch(closeOAuthRelay(op.relay), util.ReportError(errors.New(op.message)))
		}
		return util.ReportError(errors.New(op.message))
	}
	return m.advanceOAuthLogin(op)
}
func oauthLoginEnded(phase providerauth.OAuthLoginPhase) bool {
	return phase == providerauth.OAuthLoginCanceled || phase == providerauth.OAuthLoginExpired || phase == providerauth.OAuthLoginFailed
}
func (m *UI) advanceOAuthLogin(op *oauthLoginOperation) tea.Cmd {
	if op.closed && !op.completeSent && op.state.Phase != providerauth.OAuthLoginComplete && op.state.Phase != providerauth.OAuthLoginCommitting {
		return m.dispatchOAuthLogin(op, "cancel")
	}
	var commands []tea.Cmd
	switch op.state.Phase {
	case providerauth.OAuthLoginWaitingLoopback:
		op.message = "Binding the callback listener on this computer…"
		if op.relay == nil {
			return m.startOAuthRelay(op)
		}
		return m.dispatchOAuthLogin(op, "bind")
	case providerauth.OAuthLoginWaitingBrowser:
		op.message = "Finish sign-in in your browser. The callback is received on this computer."
		if op.relay == nil {
			op.message = "The original callback listener is unavailable; close this login and start a new one."
			m.showOAuthLogin(op)
			return util.ReportError(errors.New(op.message))
		}
		if !op.relayBusy {
			commands = append(commands, m.waitOAuthRelay(op))
		}
	case providerauth.OAuthLoginWaitingCode:
		op.message = "Finish sign-in, then paste the authorization code or callback URL."
	case providerauth.OAuthLoginWaitingDevice:
		op.message = "Open the verification page and enter the displayed code. The workspace is waiting for authorization."
	case providerauth.OAuthLoginAuthorized, providerauth.OAuthLoginComplete, providerauth.OAuthLoginCommitting:
		return tea.Batch(closeOAuthRelay(op.relay), m.dispatchOAuthLogin(op, "complete"))
	default:
		op.message = "Waiting for the workspace to prepare sign-in…"
	}
	if op.state.AuthorizationURL != "" && op.openedURL != op.state.AuthorizationURL && !op.closed {
		commands = append(commands, m.openOAuthBrowser(op))
	}
	commands = append(commands, m.dispatchOAuthLogin(op, "wait"))
	return tea.Batch(commands...)
}
func (m *UI) startOAuthRelay(op *oauthLoginOperation) tea.Cmd {
	if op.relayBusy {
		return nil
	}
	op.kind = "relay"
	op.relayBusy = true
	op.relayAttempt++
	descriptor := oauth.CallbackRequirement{Mode: op.state.Callback.Mode, Port: op.state.Callback.Port, Path: op.state.Callback.Path}
	attempt, ref := op.relayAttempt, op.ref
	m.showOAuthLogin(op)
	return func() tea.Msg {
		relay, err := callbackrelay.Start(context.Background(), descriptor)
		return oauthLoginRelayMsg{operation: op, attempt: attempt, ref: ref, relay: relay, started: true, err: err}
	}
}
func (m *UI) waitOAuthRelay(op *oauthLoginOperation) tea.Cmd {
	op.relayBusy = true
	op.relayAttempt++
	attempt, ref, relay := op.relayAttempt, op.ref, op.relay
	return func() tea.Msg {
		input, err := relay.Wait(context.Background())
		return oauthLoginRelayMsg{operation: op, attempt: attempt, ref: ref, relay: relay, input: input, err: err}
	}
}
func (m *UI) completeOAuthRelay(msg oauthLoginRelayMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.oauthLogins[op.workspace] != op || msg.ref != op.ref || msg.attempt != op.relayAttempt || !op.relayBusy {
		return closeOAuthRelay(msg.relay)
	}
	op.relayBusy = false
	if op.closed || op.ended || op.completeSent {
		m.showOAuthLogin(op)
		return closeOAuthRelay(msg.relay)
	}
	if msg.err != nil {
		op.message = "The callback listener could not complete this login. Retry the original listener action or close the login."
		m.showOAuthLogin(op)
		return util.ReportError(errors.New(op.message))
	}
	if msg.started {
		op.relay = msg.relay
		op.binding.Port = msg.relay.Port()
		return m.dispatchOAuthLogin(op, "bind")
	}
	if msg.relay != op.relay {
		return closeOAuthRelay(msg.relay)
	}
	return m.submitOAuthInput(op, msg.input)
}
func (m *UI) submitOAuthLogin(d *dialog.OAuthLogin) tea.Cmd {
	op := m.oauthLogins[m.com.Workspace]
	if op == nil || op.dialog != d || !m.oauthDialogOpen(d) || op.closed || op.completeSent || op.state.Phase != providerauth.OAuthLoginWaitingCode || op.submission.Input != "" {
		return nil
	}
	return m.submitOAuthInput(op, d.TakeSource())
}
func (m *UI) submitOAuthInput(op *oauthLoginOperation, input string) tea.Cmd {
	if op.submission.Input != "" {
		return nil
	}
	request := op.submission
	request.Input = input
	if err := request.Validate(); err != nil {
		op.message = "The authorization response is empty, invalid, or larger than 16 KiB. Paste the complete original response within that limit."
		m.showOAuthLogin(op)
		return util.ReportError(errors.New(op.message))
	}
	op.submission = request
	op.message = "Submitting the original authorization response to its workspace owner…"
	return m.dispatchOAuthLogin(op, "code")
}
func (m *UI) openOAuthBrowser(op *oauthLoginOperation) tea.Cmd {
	if op.closed || op.ended || op.state.AuthorizationURL == "" {
		return nil
	}
	opener := m.oauthOpenURL
	if opener == nil {
		opener = browser.OpenURL
	}
	ref, rawURL := op.ref, op.state.AuthorizationURL
	op.openedURL = rawURL
	return func() tea.Msg { return oauthLoginOpenMsg{operation: op, ref: ref, url: rawURL, err: opener(rawURL)} }
}
func (m *UI) completeOAuthBrowser(msg oauthLoginOpenMsg) tea.Cmd {
	op := msg.operation
	if op == nil || m.oauthLogins[op.workspace] != op || op.ref != msg.ref || op.closed || op.state.AuthorizationURL != msg.url || msg.err == nil {
		return nil
	}
	op.message = "The browser could not open. Use the displayed authorization URL, or retry opening it."
	m.showOAuthLogin(op)
	return util.ReportWarn(op.message)
}
func (m *UI) retryOAuthLogin(d *dialog.OAuthLogin) tea.Cmd {
	op := m.oauthLogins[m.com.Workspace]
	if op == nil || op.dialog != d || !m.oauthDialogOpen(d) || op.busy || op.preparing || op.resolved || oauthLoginReviewBusy(op) || oauthLoginReviewResolved(op) {
		return nil
	}
	if op.completeSent {
		return m.dispatchOAuthLogin(op, "complete")
	}
	if op.closed {
		return m.dispatchOAuthLogin(op, "cancel")
	}
	if op.ref.LoginID == "" {
		op.preparing = true
		op.attempt++
		m.showOAuthLogin(op)
		return prepareOAuthLoginIDs(op, false)
	}
	if op.kind == "relay" {
		return m.startOAuthRelay(op)
	}
	return m.dispatchOAuthLogin(op, op.kind)
}
func (m *UI) reloadOAuthLogin(d *dialog.OAuthLogin) tea.Cmd {
	if !m.oauthDialogOpen(d) {
		return nil
	}
	var selection *dialog.ActionSelectModel
	var expected *providerauth.Owner
	if read := m.oauthLoginReads[d]; read != nil {
		selection, expected = read.selection, read.expected
	}
	if op := m.oauthLogins[m.com.Workspace]; op != nil {
		if oauthLoginReviewBusy(op) || !op.resolved && !op.ended && !oauthLoginReviewResolved(op) {
			return util.ReportError(errors.New("The original login is still active or awaiting acknowledgement; retry or close it before starting another"))
		}
		selection = op.selection
		if !op.chooseProvider {
			owner := op.ref.Target.Owner
			expected = &owner
		} else {
			expected = nil
		}
		delete(m.oauthLogins, op.workspace)
	}
	m.dialog.CloseDialog(dialog.LoginID)
	cleanup := m.pruneOAuthLogins()
	return tea.Batch(cleanup, m.openOAuthAuthentication(selection, expected))
}
func (m *UI) recoverOAuthLogin(action dialog.ActionOAuthLoginRecover) tea.Cmd {
	op := m.oauthLogins[m.com.Workspace]
	if op == nil || op.dialog != action.Dialog || !m.oauthDialogOpen(action.Dialog) || op.busy || op.preparing || op.resolved || !op.completeSent || op.recoverer == nil || oauthLoginReviewBusy(op) || oauthLoginReviewResolved(op) {
		return nil
	}
	if action.Retry {
		if op.recovery == nil {
			return nil
		}
		return m.dispatchOAuthLogin(op, "recovery")
	}
	op.preparing = true
	op.attempt++
	op.message = "Preparing an explicit recovery attempt for the original saved login…"
	m.showOAuthLogin(op)
	return prepareOAuthLoginIDs(op, true)
}
func (m *UI) completeOAuthLoginReceipt(msg oauthLoginResultMsg) tea.Cmd {
	op := msg.operation
	if (msg.recovery == nil) != (msg.kind != "recovery") || msg.recovery != nil && (op.recovery == nil || *op.recovery != *msg.recovery) {
		return nil
	}
	err := msg.err
	if validation := msg.outcome.ValidateOAuthLogin(op.ref); validation != nil {
		err = providerauth.ErrReceiptUnverified
	} else {
		op.outcome = msg.outcome
		op.progress.AccountRefreshed = op.progress.AccountRefreshed || msg.outcome.Progress.AccountRefreshed
		op.progress.AccountsSaved = op.progress.AccountsSaved || msg.outcome.Progress.AccountsSaved
		op.progress.ConfigSaved = op.progress.ConfigSaved || msg.outcome.Progress.ConfigSaved
		op.progress.RuntimePublished = op.progress.RuntimePublished || msg.outcome.Progress.RuntimePublished
	}
	if err == nil && !msg.outcome.Superseded && msg.outcome.Change == nil {
		err = providerauth.ErrReceiptUnverified
	}
	if err != nil {
		op.message = "Login has no current workspace acknowledgement: " + safeOAuthLoginError(err) + " " + apiKeyProgress(op.progress) + " Retry keeps the original login receipt."
		m.showOAuthLogin(op)
		return util.ReportError(errors.New(op.message))
	}
	op.resolved = true
	op.submission.Input = ""
	if msg.outcome.Superseded {
		op.message = "The original login is historical. It does not authorize a model change."
		m.showOAuthLogin(op)
		return util.ReportWarn(op.message)
	}
	op.message = "Signed in to " + op.ref.Target.Owner.ProviderID + "; the owning workspace acknowledged the saved account."
	m.showOAuthLogin(op)
	if op.selection == nil || op.continued || op.closed || op.workspace != m.com.Workspace || op.generation != m.modelSelectionGen {
		return util.ReportInfo(op.message)
	}
	if err := op.selection.ValidateProviderOwner(op.workspace.Config()); err != nil {
		return util.ReportError(errors.New(op.message + " The provider owner changed, so model selection was not continued."))
	}
	op.continued = true
	selection := *op.selection
	selection.ReAuthenticate = false
	return m.handleSelectModelAfterImport(selection, false)
}
func (m *UI) showOAuthLogin(op *oauthLoginOperation) {
	if !m.oauthDialogOpen(op.dialog) {
		return
	}
	p := dialog.OAuthLoginPresentation{Message: op.message, CompleteDispatched: op.completeSent}
	if op.recordedOperationID != "" {
		p.Message = "Recorded workspace: " + op.recordedWorkspaceID + ". Result: " + op.recordedOperationID + ". This session has a new operation identity; the original outcome remains historical.\n" + p.Message
	}
	if !op.closed && !op.ended && !op.completeSent {
		p.AuthorizationURL, p.UserCode = op.state.AuthorizationURL, op.state.UserCode
		p.Open = p.AuthorizationURL != ""
		p.Editable = op.state.Phase == providerauth.OAuthLoginWaitingCode && op.submission.Input == "" && (!op.busy || op.kind == "wait")
	}
	if !op.busy && !op.preparing && !op.relayBusy && !oauthLoginReviewBusy(op) {
		reconciled := oauthLoginReviewResolved(op)
		p.Retry = !op.resolved && !op.ended && !reconciled
		p.Reload = op.resolved || op.ended || reconciled
		p.Recover = op.completeSent && !op.resolved && op.recoverer != nil && !reconciled
		p.RetryRecovery = p.Recover && op.recovery != nil
		if capability, ok := op.workspace.(workspace.ProviderAuthenticationReconciler); ok {
			p.Review = op.completeSent && (!op.resolved || op.outcome.Superseded) && !reconciled && capability.CanReconcileProviderAuthentication()
		}
	}
	op.dialog.SetPresentation(p)
}
func safeOAuthLoginError(err error) string {
	switch {
	case errors.Is(err, providerauth.ErrOAuthLoginUnavailable):
		return "The retained login is unavailable; reload to begin a new explicit login."
	case errors.Is(err, providerauth.ErrOAuthLogin):
		return "The workspace ended this login without authorization."
	case errors.Is(err, providerauth.ErrOperationConflict):
		return "The reply conflicts with the original login request."
	default:
		return strings.ReplaceAll(safeAPIKeyError(err), "new check", "new login")
	}
}
