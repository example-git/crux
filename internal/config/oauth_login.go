package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providertransport"
)

// OAuthLoginPreparation retains one owning store observation and its exact
// executable registration. Copies share their authorization attempt; a failed
// or completed attempt cannot be executed again through another handle.
type OAuthLoginPreparation struct{ state *oauthLoginPreparation }

type oauthLoginPreparation struct {
	store        *ConfigStore
	before       AuthenticationCapture
	owner        providerregistry.RegistrationOwner
	registration providerregistry.Registration
	settings     checkedAPIKeySettings
	layers       authenticationLayers
	authorize    oauthLoginAttempt[AuthorizedOAuthPreparation]
	device       oauthLoginAttempt[OAuthDeviceLogin]
	code         oauthLoginAttempt[OAuthCodeLogin]
	routeMu      sync.Mutex
	route        string
	codePort     uint16
	codePortSet  bool
	journal      *oauthLoginJournal
}

// AuthorizedOAuthPreparation can only be produced by the retained owner's
// authorization callback. It has no public token/entry constructor or accessor.
// Persistence copies share the exact original commit result, including errors.
type AuthorizedOAuthPreparation struct{ state *authorizedOAuthPreparation }

type authorizedOAuthPreparation struct {
	preparation *oauthLoginPreparation
	provider    ProviderConfig
	entry       *accounts.Entry
	commit      oauthLoginAttempt[AuthenticationMutationResult]
	scopeMu     sync.Mutex
	scope       Scope
	scopeSet    bool
}

// OAuthDeviceLogin keeps the device code and interpreter state private. Only
// Interaction's copied display strings may be presented outside the owner.
type OAuthDeviceLogin struct{ state *oauthDeviceLogin }

type oauthDeviceLogin struct {
	preparation   *oauthLoginPreparation
	authorization providerregistry.DeviceAuthorization
}

func (p OAuthLoginPreparation) Adapter() providerregistry.LoginAdapter {
	if p.state == nil || p.state.registration.OAuth == nil {
		return ""
	}
	return p.state.registration.OAuth.Adapter
}

func (d OAuthDeviceLogin) Interaction() (userCode, verificationURL string) {
	if d.state == nil {
		return "", ""
	}
	return d.state.authorization.UserCode, d.state.authorization.VerificationURL
}

// ExpiresAt is the exact deadline retained by the device interpreter. Zero
// remains unknown; this accessor never creates a replacement timeout.
func (d OAuthDeviceLogin) ExpiresAt() time.Time {
	if d.state == nil {
		return time.Time{}
	}
	return d.state.authorization.ExpiresAt
}

// One preparation selects one interaction path. A split code challenge cannot
// later be satisfied by a separate monolithic browser flow or vice versa.
func (p *oauthLoginPreparation) claimRoute(route string) error {
	p.routeMu.Lock()
	defer p.routeMu.Unlock()
	if p.route != "" && p.route != route {
		return errors.New("OAuth login interaction path differs from its original attempt")
	}
	p.route = route
	return nil
}

func (OAuthLoginPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth login preparations are private")
}
func (OAuthLoginPreparation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth login preparation]"))
}
func (AuthorizedOAuthPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authorized OAuth preparations are private")
}
func (AuthorizedOAuthPreparation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private authorized OAuth preparation]"))
}
func (OAuthDeviceLogin) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth device logins are private")
}
func (OAuthDeviceLogin) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth device login]"))
}
func (*oauthLoginPreparation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth login state]"))
}
func (*authorizedOAuthPreparation) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private authorized OAuth state]"))
}
func (*oauthDeviceLogin) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth device state]"))
}
func (*oauthLoginPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth login state is private")
}
func (*authorizedOAuthPreparation) MarshalJSON() ([]byte, error) {
	return nil, errors.New("authorized OAuth state is private")
}
func (*oauthDeviceLogin) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth device state is private")
}

// An already dispatched attempt belongs to its initiating context. A later
// waiter's cancellation stops only its wait; it never restarts the attempt.
type oauthLoginAttempt[T any] struct {
	mu    sync.Mutex
	done  chan struct{}
	value T
	err   error
}

func (a *oauthLoginAttempt[T]) execute(ctx context.Context, run func() (T, error)) (T, error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, err
	}
	a.mu.Lock()
	if a.done != nil {
		done := a.done
		a.mu.Unlock()
		select {
		case <-done:
			return a.value, a.err
		case <-ctx.Done():
			return zero, ctx.Err()
		}
	}
	a.done = make(chan struct{})
	a.mu.Unlock()
	a.value, a.err = run()
	close(a.done)
	return a.value, a.err
}

type oauthLoginError struct {
	stage string
	cause error
}

func (e *oauthLoginError) Error() string              { return "OAuth login " + e.stage + " failed" }
func (e *oauthLoginError) Unwrap() error              { return e.cause }
func (e *oauthLoginError) Format(s fmt.State, _ rune) { _, _ = s.Write([]byte(e.Error())) }

func oauthLoginFailure(stage string, err error) error {
	if err == nil {
		return nil
	}
	return &oauthLoginError{stage: stage, cause: err}
}

// PrepareOAuthLogin admits the exact accepted raw/evaluated configuration,
// account file and owner before invoking any authorization or identity callback.
// It reuses accepted shell output, rather than rerunning configuration programs.
func (s *ConfigStore) PrepareOAuthLogin(ctx context.Context, before AuthenticationCapture, owner providerregistry.RegistrationOwner) (OAuthLoginPreparation, error) {
	if err := s.validateCheckedAPIKeyCapture(ctx, before, owner); err != nil {
		return OAuthLoginPreparation{}, oauthLoginFailure("preparation", err)
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return OAuthLoginPreparation{}, err
	}
	settings := s.checkedAPIKeySettingsLocked(before)
	s.writeMu.RUnlock()
	layers, err := before.checkedAPIKeyLayers(settings)
	if err == nil {
		err = before.validateConfigBasis(layers, "")
	}
	if err != nil {
		return OAuthLoginPreparation{}, oauthLoginFailure("preparation", err)
	}
	registration, err := authenticationRegistration(before, owner)
	if err != nil {
		return OAuthLoginPreparation{}, oauthLoginFailure("preparation", err)
	}
	registration = registration.Clone()
	capability := registration.OAuth
	switch capability.Adapter {
	case providerregistry.LoginBrowser, providerregistry.LoginHostedPaste:
		if capability.Authorize == nil && capability.PrepareCode == nil {
			return OAuthLoginPreparation{}, errors.New("OAuth authorization capability is unavailable")
		}
	case providerregistry.LoginDeviceCode:
		if capability.RequestDeviceCode == nil || capability.PollDeviceCode == nil {
			return OAuthLoginPreparation{}, errors.New("OAuth device capability is unavailable")
		}
	default:
		return OAuthLoginPreparation{}, errors.New("OAuth adapter is unsupported")
	}
	p := &oauthLoginPreparation{store: s, before: before, owner: owner, registration: registration, settings: settings, layers: layers}
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return OAuthLoginPreparation{}, oauthLoginFailure("preparation", err)
	}
	if err := s.prepareOAuthLoginJournal(ctx, p); err != nil {
		return OAuthLoginPreparation{}, oauthLoginFailure("durable preparation", err)
	}
	return OAuthLoginPreparation{state: p}, nil
}

func (s *ConfigStore) validateOAuthLogin(ctx context.Context, p *oauthLoginPreparation) error {
	if p == nil || p.store != s || p.before.runtime.publicationStore != s {
		return errors.New("OAuth login does not belong to this owning store")
	}
	if err := s.validateCheckedAPIKeyCapture(ctx, p.before, p.owner); err != nil {
		return err
	}
	if err := lockAuthenticationMutex(ctx, s.writeMu.TryRLock, s.writeMu.RUnlock); err != nil {
		return err
	}
	defer s.writeMu.RUnlock()
	if !reflect.DeepEqual(p.settings, s.checkedAPIKeySettingsLocked(p.before)) {
		return errors.New("OAuth login settings changed")
	}
	return nil
}

func (s *ConfigStore) oauthLoginContext(ctx context.Context, p *oauthLoginPreparation) context.Context {
	ctx = oauth.ContextWithEnvironment(ctx, p.before.runtime.Environment())
	return providertransport.ContextWithOwnerValidator(ctx, func() error { return s.validateOAuthLogin(ctx, p) })
}

// AuthorizeOAuthLogin invokes only the captured browser/hosted-paste adapter.
// Interaction callbacks can supply display/code input, never token results.
func (s *ConfigStore) AuthorizeOAuthLogin(ctx context.Context, prepared OAuthLoginPreparation, open providerregistry.OpenURL, read providerregistry.ReadCode) (AuthorizedOAuthPreparation, error) {
	p := prepared.state
	if p == nil || p.store != s || (prepared.Adapter() != providerregistry.LoginBrowser && prepared.Adapter() != providerregistry.LoginHostedPaste) || p.registration.OAuth.Authorize == nil {
		return AuthorizedOAuthPreparation{}, errors.New("matching browser OAuth preparation is required")
	}
	if err := ctx.Err(); err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	if err := p.claimRoute("authorize"); err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	return p.authorize.execute(ctx, func() (AuthorizedOAuthPreparation, error) {
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("authorization", err)
		}
		bound := s.oauthLoginContext(ctx, p)
		releaseExchange, err := s.startOAuthLoginExchange(bound, p)
		if err != nil {
			return AuthorizedOAuthPreparation{}, err
		}
		defer releaseExchange()
		token, err := p.registration.OAuth.Authorize(bound, open, read)
		if err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("authorization", err)
		}
		return s.finishOAuthAuthorization(bound, p, token)
	})
}

func (s *ConfigStore) RequestOAuthDeviceCode(ctx context.Context, prepared OAuthLoginPreparation) (OAuthDeviceLogin, error) {
	p := prepared.state
	if p == nil || p.store != s || prepared.Adapter() != providerregistry.LoginDeviceCode {
		return OAuthDeviceLogin{}, errors.New("matching device OAuth preparation is required")
	}
	if err := ctx.Err(); err != nil {
		return OAuthDeviceLogin{}, err
	}
	if err := p.claimRoute("device"); err != nil {
		return OAuthDeviceLogin{}, err
	}
	return p.device.execute(ctx, func() (OAuthDeviceLogin, error) {
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return OAuthDeviceLogin{}, oauthLoginFailure("device request", err)
		}
		authorization, err := p.registration.OAuth.RequestDeviceCode(s.oauthLoginContext(ctx, p))
		if err != nil {
			return OAuthDeviceLogin{}, oauthLoginFailure("device request", err)
		}
		if authorization == nil || authorization.UserCode == "" || authorization.VerificationURL == "" {
			return OAuthDeviceLogin{}, errors.New("OAuth device response has no interaction data")
		}
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return OAuthDeviceLogin{}, oauthLoginFailure("device request", err)
		}
		return OAuthDeviceLogin{state: &oauthDeviceLogin{preparation: p, authorization: *authorization}}, nil
	})
}

func (s *ConfigStore) PollOAuthDeviceCode(ctx context.Context, device OAuthDeviceLogin) (AuthorizedOAuthPreparation, error) {
	if device.state == nil || device.state.preparation == nil || device.state.preparation.store != s {
		return AuthorizedOAuthPreparation{}, errors.New("matching private OAuth device login is required")
	}
	p := device.state.preparation
	return p.authorize.execute(ctx, func() (AuthorizedOAuthPreparation, error) {
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("device polling", err)
		}
		bound := s.oauthLoginContext(ctx, p)
		releaseExchange, err := s.startOAuthLoginExchange(bound, p)
		if err != nil {
			return AuthorizedOAuthPreparation{}, err
		}
		defer releaseExchange()
		token, err := p.registration.OAuth.PollDeviceCode(bound, &device.state.authorization)
		if err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("device polling", err)
		}
		return s.finishOAuthAuthorization(bound, p, token)
	})
}

func (s *ConfigStore) finishOAuthAuthorization(ctx context.Context, p *oauthLoginPreparation, token *oauth.Token) (AuthorizedOAuthPreparation, error) {
	token = cloneOAuthToken(token)
	if token == nil || token.AccessToken == "" {
		return AuthorizedOAuthPreparation{}, errors.New("OAuth authorization returned no access token")
	}
	if err := s.retainOAuthLoginResult(ctx, p, token); err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return AuthorizedOAuthPreparation{}, oauthLoginFailure("result admission", err)
	}
	var entry *accounts.Entry
	if p.owner.AccountNamespace != "" {
		id, display := "", p.owner.ProviderID
		var raw json.RawMessage
		if p.registration.Identity != nil {
			id, display, raw = p.registration.Identity(ctx, token.AccessToken)
		}
		if len(raw) > 0 && (!utf8.Valid(raw) || !json.Valid(raw)) {
			return AuthorizedOAuthPreparation{}, errors.New("OAuth account metadata is invalid")
		}
		if id == "" {
			id = "default"
		}
		if display == "" {
			display = id
		}
		value := accounts.FromToken(id, display, token, nil)
		value.Raw = slices.Clone(raw)
		entry = &value
	}
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return AuthorizedOAuthPreparation{}, oauthLoginFailure("identity admission", err)
	}
	provider, err := prepareAuthenticationProvider(ctx, p.before, p.owner, token)
	if err != nil {
		return AuthorizedOAuthPreparation{}, oauthLoginFailure("provider preparation", err)
	}
	if err := s.validateOAuthLogin(ctx, p); err != nil {
		return AuthorizedOAuthPreparation{}, oauthLoginFailure("result admission", err)
	}
	return AuthorizedOAuthPreparation{state: &authorizedOAuthPreparation{preparation: p, provider: cloneProviderConfig(provider), entry: cloneConstructionAccount(entry)}}, nil
}
