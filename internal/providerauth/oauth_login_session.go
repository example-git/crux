package providerauth

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerregistry"
)

type oauthLoginSession struct {
	mu               sync.Mutex
	ctx              context.Context
	cancel           context.CancelFunc
	state            OAuthLoginState
	changed          chan struct{}
	err              error
	before           config.AuthenticationCapture
	owner            providerregistry.RegistrationOwner
	admittedSequence uint64
	preparation      config.OAuthLoginPreparation
	code             config.OAuthCodeLogin
	authorized       config.AuthorizedOAuthPreparation
	binding          *OAuthLoginBindRequest
	submission       *OAuthLoginCodeRequest
	submissionDigest [sha256.Size]byte
	expiry           *time.Timer
}

func (*oauthLoginSession) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth login session]"))
}
func (*oauthLoginSession) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth login sessions are private")
}

func cloneOAuthLoginState(state OAuthLoginState) OAuthLoginState {
	if state.Callback != nil {
		copy := *state.Callback
		state.Callback = &copy
	}
	return state
}

func oauthLoginTerminal(phase OAuthLoginPhase) bool {
	return phase == OAuthLoginComplete || phase == OAuthLoginCanceled || phase == OAuthLoginExpired || phase == OAuthLoginFailed
}

func (l *oauthLoginSession) snapshot() (OAuthLoginState, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return cloneOAuthLoginState(l.state), l.err
}

// Caller holds l.mu. Every transition clears previous interaction data unless
// its exact new phase explicitly supplies it.
func (l *oauthLoginSession) publishLocked(phase OAuthLoginPhase, callback *OAuthLoginCallback, url, userCode string) {
	l.state.Phase, l.state.Callback = phase, callback
	l.state.AuthorizationURL, l.state.UserCode = url, userCode
	l.state.Sequence++
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *oauthLoginSession) publishInteractionLocked(phase OAuthLoginPhase, callback *OAuthLoginCallback, url, userCode string) error {
	candidate := l.state
	candidate.Phase, candidate.Callback = phase, callback
	candidate.AuthorizationURL, candidate.UserCode = url, userCode
	candidate.Sequence++
	if err := candidate.Validate(); err != nil {
		return err
	}
	l.publishLocked(phase, callback, url, userCode)
	return nil
}

func (l *oauthLoginSession) fail(err error) {
	l.mu.Lock()
	if oauthLoginTerminal(l.state.Phase) || l.state.Phase == OAuthLoginCommitting {
		l.mu.Unlock()
		return
	}
	phase := OAuthLoginFailed
	if errors.Is(err, context.Canceled) {
		phase = OAuthLoginCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		phase = OAuthLoginExpired
	}
	l.err = &mutationError{public: ErrOAuthLogin, cause: err}
	l.publishLocked(phase, nil, "", "")
	l.mu.Unlock()
	l.cancel()
	l.clearPrivate()
}

func (l *oauthLoginSession) clearPrivate() {
	l.mu.Lock()
	code := l.code
	prepared := l.preparation
	l.code = config.OAuthCodeLogin{}
	l.preparation = config.OAuthLoginPreparation{}
	l.authorized = config.AuthorizedOAuthPreparation{}
	l.before = config.AuthenticationCapture{}
	if l.expiry != nil {
		l.expiry.Stop()
		l.expiry = nil
	}
	l.mu.Unlock()
	code.Close()
	_ = prepared.DiscardUnstartedJournal(l.ctx)
}

// A deadline comes from the captured adapter, never a service-invented shorter
// timeout. Authorization completion stops it before account commit begins.
func (l *oauthLoginSession) setExpiryLocked(deadline time.Time) {
	if deadline.IsZero() {
		return
	}
	l.state.ExpiresAt = deadline.UnixMilli()
	l.expiry = time.AfterFunc(time.Until(deadline), func() {
		l.mu.Lock()
		phase := l.state.Phase
		if oauthLoginTerminal(phase) || phase == OAuthLoginAuthorized || phase == OAuthLoginCommitting {
			l.mu.Unlock()
			return
		}
		l.err = &mutationError{public: ErrOAuthLogin, cause: context.DeadlineExceeded}
		l.publishLocked(OAuthLoginExpired, nil, "", "")
		l.mu.Unlock()
		l.cancel()
		l.clearPrivate()
	})
}

func (l *oauthLoginSession) authorize(value config.AuthorizedOAuthPreparation) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if oauthLoginTerminal(l.state.Phase) || l.ctx.Err() != nil {
		return
	}
	l.authorized = value
	if l.expiry != nil {
		l.expiry.Stop()
		l.expiry = nil
	}
	l.publishLocked(OAuthLoginAuthorized, nil, "", "")
}

// startOAuthWork is called only while the service gate is held. Close crosses
// that gate after cancellation before waiting for the worker set.
func (s *Service) startOAuthWork(login *oauthLoginSession, run func() error) {
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		if err := run(); err != nil {
			login.fail(err)
		}
	}()
}

func (s *Service) initializeOAuthLogin(login *oauthLoginSession) error {
	login.mu.Lock()
	before, owner := login.before, login.owner
	operation := config.AuthenticationJournalKey{Kind: config.AuthenticationJournalOAuth, WorkspaceID: login.state.Login.Target.WorkspaceID, OperationID: login.state.Login.OperationID}
	login.mu.Unlock()
	prepared, err := s.store.PrepareOAuthLogin(config.ContextWithAuthenticationOperation(login.ctx, operation), before, owner)
	if err != nil {
		return err
	}
	login.mu.Lock()
	if oauthLoginTerminal(login.state.Phase) || login.ctx.Err() != nil {
		login.mu.Unlock()
		_ = prepared.DiscardUnstartedJournal(login.ctx)
		return login.ctx.Err()
	}
	login.preparation = prepared
	login.mu.Unlock()
	switch prepared.Adapter() {
	case providerregistry.LoginBrowser:
		callback := prepared.CallbackRequirement()
		if callback == nil {
			return errors.New("OAuth owner has no callback requirement")
		}
		public := &OAuthLoginCallback{Mode: callback.Mode, Port: callback.Port, Path: callback.Path}
		if err := public.Validate(); err != nil {
			return err
		}
		login.mu.Lock()
		if !oauthLoginTerminal(login.state.Phase) && login.ctx.Err() == nil {
			login.publishLocked(OAuthLoginWaitingLoopback, public, "", "")
		}
		login.mu.Unlock()
		return nil
	case providerregistry.LoginHostedPaste:
		return s.prepareOAuthCode(login, prepared, 0)
	case providerregistry.LoginDeviceCode:
		device, err := s.store.RequestOAuthDeviceCode(login.ctx, prepared)
		if err != nil {
			return err
		}
		userCode, url := device.Interaction()
		login.mu.Lock()
		if oauthLoginTerminal(login.state.Phase) || login.ctx.Err() != nil {
			login.mu.Unlock()
			return login.ctx.Err()
		}
		login.setExpiryLocked(device.ExpiresAt())
		err = login.publishInteractionLocked(OAuthLoginWaitingDevice, nil, url, userCode)
		login.mu.Unlock()
		if err != nil {
			return err
		}
		authorized, err := s.store.PollOAuthDeviceCode(login.ctx, device)
		if err != nil {
			return err
		}
		login.authorize(authorized)
		return nil
	default:
		return errors.New("OAuth login adapter is unavailable")
	}
}

func (s *Service) prepareOAuthCode(login *oauthLoginSession, prepared config.OAuthLoginPreparation, port uint16) error {
	code, err := s.store.PrepareOAuthCodeChallenge(login.ctx, prepared, port)
	if err != nil {
		return err
	}
	login.mu.Lock()
	if oauthLoginTerminal(login.state.Phase) || login.ctx.Err() != nil {
		login.mu.Unlock()
		code.Close()
		return login.ctx.Err()
	}
	login.code = code
	login.setExpiryLocked(code.ExpiresAt())
	phase := OAuthLoginWaitingCode
	var callback *OAuthLoginCallback
	if prepared.Adapter() == providerregistry.LoginBrowser {
		phase = OAuthLoginWaitingBrowser
		descriptor := prepared.CallbackRequirement()
		callback = &OAuthLoginCallback{Mode: descriptor.Mode, Port: descriptor.Port, Path: descriptor.Path}
	}
	err = login.publishInteractionLocked(phase, callback, code.AuthorizationURL(), "")
	login.mu.Unlock()
	return err
}
