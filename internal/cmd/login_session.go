package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callbackrelay"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/workspace"
	"github.com/muesli/cancelreader"
)

type oauthLoginConsole struct {
	input    *bufio.Reader
	output   io.Writer
	openURL  func(string) error
	copyCode func(string)
}

type oauthLoginSelection struct {
	target  providerauth.Target
	status  providerauth.Status
	surface providerregistry.Surface
}

// The remote owner need not be installed on the terminal's host. Canonical IDs
// come from the workspace surface; local aliases only select an identical owner.
func selectOAuthLogin(ws workspace.Workspace, snapshot providerauth.Snapshot, args []string) (oauthLoginSelection, error) {
	if err := snapshot.Validate(); err != nil {
		return oauthLoginSelection{}, err
	}
	requested := ""
	if len(args) > 0 {
		requested = args[0]
	}
	var aliasOwner *providerauth.Owner
	if requested != "" && ws.Config() != nil {
		if registration, ok := ws.Config().ProviderRegistrationForAccount(requested); ok {
			owner := providerauth.PublicOwner(registration.Owner())
			aliasOwner = &owner
		}
	}
	for _, surface := range ws.ProviderSurfaces() {
		if requested != "" && surface.ID != requested && (aliasOwner == nil || aliasOwner.ProviderID != surface.ID) {
			continue
		}
		for _, status := range snapshot.Providers {
			if status.Owner.ProviderID != surface.ID || !status.Owner.HasOAuth {
				continue
			}
			if surface.Owner == nil || providerauth.PublicOwner(*surface.Owner) != status.Owner {
				return oauthLoginSelection{}, providerauth.ErrOwner
			}
			if surface.ID != requested && requested != "" && (aliasOwner == nil || *aliasOwner != status.Owner) {
				return oauthLoginSelection{}, providerauth.ErrOwner
			}
			for _, auth := range surface.Authentication {
				if auth.Kind != "oauth2" || auth.Adapter != status.Owner.OAuthAdapter || auth.FlowID != status.Owner.OAuthFlowID {
					continue
				}
				if !surface.Available || !auth.Available {
					return oauthLoginSelection{}, fmt.Errorf("OAuth login for %s is unavailable: %s", surface.ID, auth.Diagnostic)
				}
				return oauthLoginSelection{target: providerauth.Target{WorkspaceID: snapshot.WorkspaceID, Owner: status.Owner, Generation: snapshot.Generation}, status: status, surface: surface}, nil
			}
		}
	}
	if requested != "" {
		return oauthLoginSelection{}, fmt.Errorf("OAuth provider %q is unavailable in this workspace", requested)
	}
	return oauthLoginSelection{}, errors.New("no OAuth provider is available in this workspace")
}

func oauthCredentialPresent(status providerauth.Status) bool {
	for _, credential := range status.Credentials {
		if credential.Kind == "oauth" && credential.State == "present" {
			return true
		}
	}
	return false
}

func oauthActionID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

func runWorkspaceLogin(ctx context.Context, ws workspace.Workspace, args []string, force bool, input io.Reader, output io.Writer, openURL func(string) error, copyCode func(string)) error {
	snapshot, err := ws.ProviderAuthentication(ctx)
	if err != nil {
		return err
	}
	selection, err := selectOAuthLogin(ws, snapshot, args)
	if err != nil {
		return err
	}
	name := selection.surface.Name
	if name == "" {
		name = selection.surface.ID
	}
	if !force && oauthCredentialPresent(selection.status) && selection.status.AccountState != "out-of-sync" {
		_, err = fmt.Fprintf(output, "You are already logged in to %s.\nUse --force to re-authenticate.\n", name)
		return err
	}
	// Preserve the existing Copilot import-first behavior, using the same owning
	// workspace transaction as the UI instead of reading this host's account file.
	if selection.target.Owner.Construction == providerregistry.ConstructionCopilot && !selection.target.Owner.HasManifest && !selection.target.Owner.HasPreset {
		found, importErr := ws.ImportCopilot(ctx, *selection.surface.Owner)
		if importErr != nil {
			return importErr
		}
		if found {
			current, statusErr := ws.ProviderAuthentication(ctx)
			if statusErr != nil {
				return statusErr
			}
			selected, statusErr := selectOAuthLogin(ws, current, []string{selection.surface.ID})
			if statusErr != nil {
				return statusErr
			}
			if selected.target.Owner != selection.target.Owner || !oauthCredentialPresent(selected.status) || selected.status.AccountState == "out-of-sync" {
				return errors.New("imported OAuth credential could not be confirmed for the original owner")
			}
			_, err = fmt.Fprintf(output, "Authenticated with %s using the existing credential.\n", name)
			return err
		}
	}
	console, closeInput, err := newAuthenticationConsole(ctx, input, output, openURL, copyCode)
	if err != nil {
		return err
	}
	defer closeInput()
	return console.login(ctx, ws, selection, name)
}

func newAuthenticationConsole(ctx context.Context, input io.Reader, output io.Writer, openURL func(string) error, copyCode func(string)) (*oauthLoginConsole, func(), error) {
	reader, err := cancelreader.NewReader(input)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare authentication input: %w", err)
	}
	stopped := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { reader.Cancel(); close(stopped) })
	closeInput := func() {
		if !stop() {
			<-stopped
		}
		_ = reader.Close()
	}
	console := &oauthLoginConsole{input: bufio.NewReaderSize(reader, providerauth.OAuthLoginInputLimit+2), output: output, openURL: openURL, copyCode: copyCode}
	return console, closeInput, nil
}

func (c *oauthLoginConsole) readLine(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	data, err := c.input.ReadSlice('\n')
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if errors.Is(err, bufio.ErrBufferFull) {
		return "", errors.New("OAuth input exceeds the allowed size")
	}
	if err != nil && !(errors.Is(err, io.EOF) && len(data) > 0) {
		return "", err
	}
	line := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if len(line) > providerauth.OAuthLoginInputLimit {
		return "", errors.New("OAuth input exceeds the allowed size")
	}
	return line, nil
}

func (c *oauthLoginConsole) retry(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	fmt.Fprintf(c.output, "Login request failed: %v\nEnter r to retry the same request, or c to cancel: ", err)
	answer, inputErr := c.readLine(ctx)
	if inputErr != nil {
		return errors.Join(err, inputErr)
	}
	if strings.EqualFold(strings.TrimSpace(answer), "r") {
		return nil
	}
	return err
}

func (c *oauthLoginConsole) interaction(ctx context.Context, ref providerauth.OAuthLoginRef, call func() (providerauth.OAuthLoginState, error)) (providerauth.OAuthLoginState, error) {
	for {
		state, err := call()
		if err == nil {
			err = state.Validate()
			if err == nil && state.Login != ref {
				err = errors.New("OAuth reply belongs to another login")
			}
		}
		if err == nil {
			return state, nil
		}
		if err = c.retry(ctx, err); err != nil {
			return providerauth.OAuthLoginState{}, err
		}
	}
}

func (c *oauthLoginConsole) showURL(url string) {
	fmt.Fprintf(c.output, "Open this URL to authorize the login:\n%s\n", url)
	if c.openURL != nil {
		if err := c.openURL(url); err != nil {
			fmt.Fprintln(c.output, "Could not open the browser. Open the URL above manually.")
		}
	}
}

func (c *oauthLoginConsole) login(ctx context.Context, ws workspace.Workspace, selected oauthLoginSelection, name string) (resultErr error) {
	ref := providerauth.OAuthLoginRef{LoginID: oauthActionID(), OperationID: oauthActionID(), Target: selected.target}
	// A lost Begin reply may still have admitted the original session. Cancel
	// retrieves that exact reference; the service retains admitted commit receipts.
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		state, err := ws.CancelProviderOAuthLogin(cleanup, ref)
		if resultErr != nil && err != nil && state.Phase != providerauth.OAuthLoginCanceled && state.Phase != providerauth.OAuthLoginComplete && !errors.Is(err, providerauth.ErrOAuthLoginUnavailable) {
			resultErr = errors.Join(resultErr, fmt.Errorf("login cancellation could not be confirmed: %w", err))
		}
	}()
	state, err := c.interaction(ctx, ref, func() (providerauth.OAuthLoginState, error) { return ws.BeginProviderOAuthLogin(ctx, ref) })
	if err != nil {
		return err
	}
	var relay *callbackrelay.Relay
	defer func() {
		if relay != nil {
			_ = relay.Close()
		}
	}()
	shown := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch state.Phase {
		case providerauth.OAuthLoginWaitingLoopback:
			if relay != nil {
				return errors.New("OAuth owner requested a second callback binding")
			}
			callback := state.Callback
			relay, err = callbackrelay.Start(ctx, oauth.CallbackRequirement{Mode: callback.Mode, Port: callback.Port, Path: callback.Path})
			if err != nil {
				return err
			}
			request := providerauth.OAuthLoginBindRequest{Login: ref, BindingID: oauthActionID(), Port: relay.Port()}
			state, err = c.interaction(ctx, ref, func() (providerauth.OAuthLoginState, error) { return ws.BindProviderOAuthLogin(ctx, request) })
		case providerauth.OAuthLoginWaitingBrowser, providerauth.OAuthLoginWaitingCode:
			if !shown {
				c.showURL(state.AuthorizationURL)
				shown = true
			}
			var code string
			if state.Phase == providerauth.OAuthLoginWaitingBrowser {
				if relay == nil {
					return errors.New("OAuth browser login has no retained callback listener")
				}
				code, err = relay.Wait(ctx)
			} else {
				fmt.Fprint(c.output, "Paste the authorization code or full callback URL and press enter:\n> ")
				code, err = c.readLine(ctx)
			}
			if err != nil {
				return err
			}
			request := providerauth.OAuthLoginCodeRequest{Login: ref, SubmissionID: oauthActionID(), Input: code}
			if err = request.Validate(); err != nil {
				return err
			}
			state, err = c.interaction(ctx, ref, func() (providerauth.OAuthLoginState, error) { return ws.SubmitProviderOAuthLoginCode(ctx, request) })
		case providerauth.OAuthLoginWaitingDevice:
			if !shown {
				fmt.Fprintf(c.output, "Authorization code: %s\n", state.UserCode)
				if c.copyCode != nil {
					c.copyCode(state.UserCode)
				}
				c.showURL(state.AuthorizationURL)
				fmt.Fprintln(c.output, "Waiting for authorization...")
				shown = true
			}
			after := state.Sequence
			state, err = c.interaction(ctx, ref, func() (providerauth.OAuthLoginState, error) { return ws.WaitProviderOAuthLogin(ctx, ref, after) })
		case providerauth.OAuthLoginPreparing, providerauth.OAuthLoginAuthorizing, providerauth.OAuthLoginCommitting:
			after := state.Sequence
			state, err = c.interaction(ctx, ref, func() (providerauth.OAuthLoginState, error) { return ws.WaitProviderOAuthLogin(ctx, ref, after) })
		case providerauth.OAuthLoginAuthorized, providerauth.OAuthLoginComplete:
			outcome, err := c.complete(ctx, ws, ref)
			if err != nil {
				return err
			}
			display := ""
			for _, account := range outcome.Change.Current.Accounts {
				if account.Active {
					display = account.DisplayName
				}
			}
			if display != "" && display != "default" {
				_, err = fmt.Fprintf(c.output, "Authenticated with %s as %s.\n", name, display)
			} else {
				_, err = fmt.Fprintf(c.output, "Authenticated with %s.\n", name)
			}
			return err
		case providerauth.OAuthLoginCanceled, providerauth.OAuthLoginExpired, providerauth.OAuthLoginFailed:
			return fmt.Errorf("OAuth login %s", state.Phase)
		default:
			return errors.New("OAuth login returned an unsupported phase")
		}
		if err != nil {
			return err
		}
	}
}

func (c *oauthLoginConsole) complete(ctx context.Context, ws workspace.Workspace, ref providerauth.OAuthLoginRef) (providerauth.MutationOutcome, error) {
	return c.mutateAuthentication(ctx, ws, ref.OperationID, ref.Target, "Login", func() (providerauth.MutationOutcome, error) { return ws.CompleteProviderOAuthLogin(ctx, ref) }, func(outcome providerauth.MutationOutcome) error { return outcome.ValidateOAuthLogin(ref) })
}

func (c *oauthLoginConsole) mutateAuthentication(ctx context.Context, ws workspace.Workspace, operationID string, target providerauth.Target, label string, perform func() (providerauth.MutationOutcome, error), validate func(providerauth.MutationOutcome) error) (providerauth.MutationOutcome, error) {

	var recovery *workspace.ProviderAuthenticationRecoveryRequest
	var sequence uint64
	for {
		var outcome providerauth.MutationOutcome
		var err error
		recoverer, canRecover := ws.(workspace.ProviderAuthenticationRecoverer)
		canRecover = canRecover && recoverer.CanRecoverProviderAuthentication()
		if recovery != nil {
			outcome, err = recoverer.RecoverProviderAuthentication(ctx, *recovery)
		} else {
			outcome, err = perform()
		}
		valid := validate(outcome)
		if err == nil {
			err = valid
		}
		if err == nil && (outcome.Change == nil || outcome.Superseded || !outcome.Progress.ConfigSaved || !outcome.Progress.RuntimePublished) {
			err = errors.New("authentication change has no current completed receipt")
		}
		if err == nil {
			return outcome, nil
		}
		fmt.Fprintf(c.output, "%s completion failed: %v\n", label, err)
		if valid == nil && (outcome.Progress.AccountsSaved || outcome.Progress.ConfigSaved || outcome.Progress.RuntimePublished) {
			fmt.Fprintf(c.output, "Retained save progress: accounts=%t, configuration=%t, local runtime=%t. Remote acknowledgement is not confirmed.\n", outcome.Progress.AccountsSaved, outcome.Progress.ConfigSaved, outcome.Progress.RuntimePublished)
		}
		if ctx.Err() != nil {
			return outcome, errors.Join(err, ctx.Err())
		}
		offerRecovery := canRecover && valid == nil && outcome.Change != nil && !outcome.Superseded && outcome.Progress.ConfigSaved && outcome.Progress.RuntimePublished
		if offerRecovery {
			fmt.Fprint(c.output, "Enter r to retry the same request, p to publish the saved change, or c to stop: ")
		} else {
			fmt.Fprint(c.output, "Enter r to retry the same request, or c to stop: ")
		}
		answer, inputErr := c.readLine(ctx)
		if inputErr != nil {
			return outcome, errors.Join(err, inputErr)
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "r":
		case "p":
			if !offerRecovery {
				return outcome, err
			}
			sequence++
			recovery = &workspace.ProviderAuthenticationRecoveryRequest{OperationID: operationID, Target: target, RecoveryID: oauthActionID(), RecoverySequence: sequence}
		default:
			return outcome, err
		}
	}
}
