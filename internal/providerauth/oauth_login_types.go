package providerauth

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
)

const OAuthLoginInputLimit = 16 << 10

type OAuthLoginRequest struct {
	LoginID     string `json:"login_id"`
	OperationID string `json:"operation_id"`
	Target      Target `json:"target"`
}

// The original begin identity remains the reference for every later action.
// Service admission consumes the initiating generation; subsequent actions
// retrieve that retained session instead of substituting a fresh target.
type OAuthLoginRef = OAuthLoginRequest

func (r OAuthLoginRequest) Validate() error {
	if !validOperationID(r.LoginID) || !validOperationID(r.OperationID) {
		return errors.New("invalid OAuth login identity")
	}
	if err := r.Target.Validate(); err != nil {
		return err
	}
	if !r.Target.Owner.HasOAuth {
		return ErrOwner
	}
	switch r.Target.Owner.OAuthAdapter {
	case providerregistry.LoginBrowser, providerregistry.LoginHostedPaste, providerregistry.LoginDeviceCode:
		return nil
	default:
		return errors.New("unsupported OAuth login adapter")
	}
}

type OAuthLoginBindRequest struct {
	Login     OAuthLoginRef `json:"login"`
	BindingID string        `json:"binding_id"`
	Port      uint16        `json:"port"`
}

func (r OAuthLoginBindRequest) Validate() error {
	if err := r.Login.Validate(); err != nil {
		return err
	}
	if !validOperationID(r.BindingID) || r.Port == 0 {
		return errors.New("invalid OAuth callback binding")
	}
	if r.Login.Target.Owner.OAuthAdapter != providerregistry.LoginBrowser {
		return errors.New("OAuth login does not use a loopback callback")
	}
	return nil
}

type OAuthLoginCodeRequest struct {
	Login        OAuthLoginRef `json:"login"`
	SubmissionID string        `json:"submission_id"`
	Input        string        `json:"input"`
}

func (r OAuthLoginCodeRequest) Validate() error {
	if err := r.Login.Validate(); err != nil {
		return err
	}
	if !validOperationID(r.SubmissionID) || len(r.Input) == 0 || len(r.Input) > OAuthLoginInputLimit || !utf8.ValidString(r.Input) || strings.ContainsRune(r.Input, 0) {
		return errors.New("invalid OAuth callback input")
	}
	if r.Login.Target.Owner.OAuthAdapter == providerregistry.LoginDeviceCode {
		return errors.New("OAuth device login does not accept callback input")
	}
	return nil
}

func (OAuthLoginCodeRequest) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth callback input]"))
}

type OAuthLoginPhase string

const (
	OAuthLoginPreparing       OAuthLoginPhase = "preparing"
	OAuthLoginWaitingLoopback OAuthLoginPhase = "waiting-for-loopback"
	OAuthLoginWaitingBrowser  OAuthLoginPhase = "waiting-for-browser"
	OAuthLoginWaitingCode     OAuthLoginPhase = "waiting-for-code"
	OAuthLoginWaitingDevice   OAuthLoginPhase = "waiting-for-device"
	OAuthLoginAuthorizing     OAuthLoginPhase = "authorizing"
	OAuthLoginAuthorized      OAuthLoginPhase = "authorized"
	OAuthLoginCommitting      OAuthLoginPhase = "committing"
	OAuthLoginComplete        OAuthLoginPhase = "complete"
	OAuthLoginCanceled        OAuthLoginPhase = "canceled"
	OAuthLoginExpired         OAuthLoginPhase = "expired"
	OAuthLoginFailed          OAuthLoginPhase = "failed"
)

// OAuthLoginCallback is only a loopback binding requirement, never an arbitrary
// destination supplied by the caller. Its exact value comes from the admitted
// owner's immutable callback declaration.
type OAuthLoginCallback struct {
	Mode string `json:"mode"`
	Port uint16 `json:"port"`
	Path string `json:"path"`
}

func (c OAuthLoginCallback) Validate() error {
	if c.Mode != "loopback-fixed" && c.Mode != "loopback-dynamic" {
		return errors.New("invalid OAuth callback mode")
	}
	return (oauth.CallbackRequirement{Mode: c.Mode, Port: c.Port, Path: c.Path}).Validate()
}

// Interaction URLs and device user codes are returned only through the private
// authenticated login routes. They are never included in ordinary Status or
// discovery responses; diagnostic formatting always omits them.
type OAuthLoginState struct {
	Login            OAuthLoginRef       `json:"login"`
	Recovery         *OAuthLoginRecovery `json:"recovery,omitempty"`
	Sequence         uint64              `json:"sequence"`
	Phase            OAuthLoginPhase     `json:"phase"`
	ExpiresAt        int64               `json:"expires_at,omitempty"` // Unix milliseconds; zero means no declared deadline.
	Callback         *OAuthLoginCallback `json:"callback,omitempty"`
	AuthorizationURL string              `json:"authorization_url,omitempty"`
	UserCode         string              `json:"user_code,omitempty"`
}

func (OAuthLoginState) Format(state fmt.State, _ rune) {
	_, _ = state.Write([]byte("[private OAuth login state]"))
}

func (s OAuthLoginState) Validate() error {
	if err := s.Login.Validate(); err != nil {
		return err
	}
	if s.Recovery != nil {
		if err := (OAuthLoginRecoveryRequest{Login: s.Login, OriginalOperationID: s.Recovery.OriginalOperationID}).Validate(); err != nil {
			return err
		}
		switch s.Phase {
		case OAuthLoginWaitingLoopback, OAuthLoginWaitingBrowser, OAuthLoginWaitingCode, OAuthLoginWaitingDevice:
			return errors.New("recorded OAuth recovery cannot request a new interaction")
		}
	}
	// Zero means the captured adapter declares no overall expiry. The session
	// remains bound to workspace lifetime and explicit cancellation.
	if s.Sequence == 0 || s.ExpiresAt < 0 {
		return errors.New("invalid OAuth login state sequence or expiry")
	}
	if s.Callback != nil {
		if err := s.Callback.Validate(); err != nil {
			return err
		}
		if s.Login.Target.Owner.OAuthAdapter != providerregistry.LoginBrowser {
			return errors.New("unexpected OAuth callback requirement")
		}
	}
	if s.AuthorizationURL != "" {
		if !validText(s.AuthorizationURL, 64<<10, true) {
			return errors.New("invalid OAuth interaction URL")
		}
		u, err := url.Parse(s.AuthorizationURL)
		if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return errors.New("invalid OAuth interaction URL")
		}
	}
	if !validText(s.UserCode, 4096, false) {
		return errors.New("invalid OAuth device user code")
	}
	switch s.Phase {
	case OAuthLoginWaitingLoopback:
		if s.Callback == nil || s.AuthorizationURL != "" || s.UserCode != "" {
			return errors.New("inconsistent OAuth callback binding state")
		}
	case OAuthLoginWaitingBrowser:
		if s.Callback == nil || s.AuthorizationURL == "" || s.UserCode != "" {
			return errors.New("inconsistent OAuth browser state")
		}
	case OAuthLoginWaitingCode:
		if s.Login.Target.Owner.OAuthAdapter != providerregistry.LoginHostedPaste || s.Callback != nil || s.AuthorizationURL == "" || s.UserCode != "" {
			return errors.New("inconsistent OAuth pasted-code state")
		}
	case OAuthLoginWaitingDevice:
		if s.Login.Target.Owner.OAuthAdapter != providerregistry.LoginDeviceCode || s.Callback != nil || s.AuthorizationURL == "" || s.UserCode == "" {
			return errors.New("inconsistent OAuth device state")
		}
	case OAuthLoginPreparing, OAuthLoginAuthorizing, OAuthLoginAuthorized, OAuthLoginCommitting, OAuthLoginComplete, OAuthLoginCanceled, OAuthLoginExpired, OAuthLoginFailed:
		if s.Callback != nil || s.AuthorizationURL != "" || s.UserCode != "" {
			return errors.New("OAuth non-interactive state contains interaction data")
		}
	default:
		return errors.New("invalid OAuth login phase")
	}
	return nil
}

func (o MutationOutcome) ValidateOAuthLogin(request OAuthLoginRef) error {
	if err := request.Validate(); err != nil {
		return err
	}
	return o.validateRequest(mutationRequest{operationID: request.OperationID, target: request.Target, loginID: request.LoginID})
}

func validateOAuthLoginEffect(outcome MutationOutcome) error {
	if outcome.Change == nil || outcome.Progress.AccountRefreshed || !outcome.Progress.ConfigSaved || !outcome.Progress.RuntimePublished {
		return errors.New("OAuth login has inconsistent local progress")
	}
	current := outcome.Change.Current
	if !current.Status.Configured {
		return errors.New("OAuth login did not configure its provider")
	}
	for _, credential := range current.Status.Credentials {
		if credential.Kind == "api-key" && credential.State != "configured" || credential.Kind == "oauth" && credential.State != "present" {
			return errors.New("OAuth login did not install its selected credential kind")
		}
	}
	if outcome.Progress.AccountsSaved {
		if current.Status.AccountState != "in-sync" || current.Status.ActiveAccountID == "" {
			return errors.New("OAuth login did not install its saved account")
		}
	} else if current.Status.AccountState != "none" || current.Status.ActiveAccountID != "" || len(current.Accounts) != 0 {
		return errors.New("OAuth login has inconsistent account persistence")
	}
	return nil
}

var (
	ErrOAuthLogin            = errors.New("provider OAuth login did not complete")
	ErrOAuthLoginUnavailable = errors.New("OAuth login session is unavailable; begin a new login")
)
