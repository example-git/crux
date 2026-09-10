package config

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry"
)

// OAuthCodeLogin is an owner-side challenge, not a listener or a token. It
// retains the original preparation and keeps PKCE/state in the exact captured
// interpreter. Copies share the challenge and the one authorization result.
type OAuthCodeLogin struct{ state *oauthCodeLogin }

type oauthCodeLogin struct {
	preparation *oauthLoginPreparation
	challenge   *oauth.CodeChallenge
	inputMu     sync.Mutex
	input       string
	inputSet    bool
}

func (OAuthCodeLogin) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth code logins are private")
}

func (OAuthCodeLogin) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth code login]"))
}

func (*oauthCodeLogin) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth code state is private")
}

func (*oauthCodeLogin) Format(s fmt.State, _ rune) {
	_, _ = s.Write([]byte("[private OAuth code state]"))
}

// CallbackRequirement returns a copy of the admitted owner's declaration. No
// callback destination is inferred from a provider ID or the receiving host.
func (p OAuthLoginPreparation) CallbackRequirement() *oauth.CallbackRequirement {
	if p.state == nil || p.state.registration.OAuth == nil || p.state.registration.OAuth.Callback == nil {
		return nil
	}
	copy := *p.state.registration.OAuth.Callback
	return &copy
}

func (c OAuthCodeLogin) AuthorizationURL() string {
	if c.state == nil || c.state.challenge == nil {
		return ""
	}
	return c.state.challenge.AuthorizationURL()
}

func (c OAuthCodeLogin) ExpiresAt() time.Time {
	if c.state == nil || c.state.challenge == nil {
		return time.Time{}
	}
	return c.state.challenge.ExpiresAt()
}

func (c OAuthCodeLogin) Close() {
	if c.state != nil && c.state.challenge != nil {
		c.state.challenge.Close()
	}
}

// PrepareOAuthCodeChallenge accepts only the actual UI-bound port required by
// the captured callback descriptor. The owner creates the authorization URL;
// this method never opens a browser/listener or accepts an arbitrary redirect.
func (s *ConfigStore) PrepareOAuthCodeChallenge(ctx context.Context, prepared OAuthLoginPreparation, port uint16) (OAuthCodeLogin, error) {
	p := prepared.state
	if p == nil || p.store != s || (prepared.Adapter() != providerregistry.LoginBrowser && prepared.Adapter() != providerregistry.LoginHostedPaste) {
		return OAuthCodeLogin{}, errors.New("matching code OAuth preparation is required")
	}
	if err := ctx.Err(); err != nil {
		return OAuthCodeLogin{}, err
	}
	capability := p.registration.OAuth
	if capability.Callback == nil || capability.PrepareCode == nil {
		return OAuthCodeLogin{}, errors.New("captured OAuth owner has no code challenge capability")
	}
	if err := capability.Callback.Validate(); err != nil {
		return OAuthCodeLogin{}, oauthLoginFailure("callback requirement", err)
	}
	if (prepared.Adapter() == providerregistry.LoginHostedPaste) != (capability.Callback.Mode == "hosted-paste") {
		return OAuthCodeLogin{}, errors.New("OAuth callback requirement does not match its captured adapter")
	}
	if err := capability.Callback.ValidatePort(port); err != nil {
		return OAuthCodeLogin{}, oauthLoginFailure("callback binding", err)
	}
	p.routeMu.Lock()
	if p.route != "" && p.route != "code" || p.codePortSet && p.codePort != port {
		p.routeMu.Unlock()
		return OAuthCodeLogin{}, errors.New("OAuth code binding differs from its original attempt")
	}
	p.route, p.codePort, p.codePortSet = "code", port, true
	p.routeMu.Unlock()
	return p.code.execute(ctx, func() (OAuthCodeLogin, error) {
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return OAuthCodeLogin{}, oauthLoginFailure("code preparation", err)
		}
		challenge, err := capability.PrepareCode(s.oauthLoginContext(ctx, p), port)
		if err != nil {
			if challenge != nil {
				challenge.Close()
			}
			return OAuthCodeLogin{}, oauthLoginFailure("code preparation", err)
		}
		if challenge == nil || challenge.AuthorizationURL() == "" {
			if challenge != nil {
				challenge.Close()
			}
			return OAuthCodeLogin{}, errors.New("OAuth code challenge has no authorization URL")
		}
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			challenge.Close()
			return OAuthCodeLogin{}, oauthLoginFailure("code preparation", err)
		}
		return OAuthCodeLogin{state: &oauthCodeLogin{preparation: p, challenge: challenge}}, nil
	})
}

// ExchangeOAuthCode retains the exact first input, even after a failed attempt.
// A retry joins that attempt; a conflicting input cannot receive an unrelated
// successful authorization and no retry can repeat the interpreter exchange.
func (s *ConfigStore) ExchangeOAuthCode(ctx context.Context, code OAuthCodeLogin, input string) (AuthorizedOAuthPreparation, error) {
	state := code.state
	if state == nil || state.preparation == nil || state.preparation.store != s || state.challenge == nil {
		return AuthorizedOAuthPreparation{}, errors.New("matching private OAuth code login is required")
	}
	if err := ctx.Err(); err != nil {
		return AuthorizedOAuthPreparation{}, err
	}
	state.inputMu.Lock()
	if state.inputSet && state.input != input {
		state.inputMu.Unlock()
		return AuthorizedOAuthPreparation{}, errors.New("OAuth callback input differs from its original submission")
	}
	state.input, state.inputSet = input, true
	state.inputMu.Unlock()
	p := state.preparation
	return p.authorize.execute(ctx, func() (AuthorizedOAuthPreparation, error) {
		if err := s.validateOAuthLogin(ctx, p); err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("code exchange", err)
		}
		bound := s.oauthLoginContext(ctx, p)
		releaseExchange, err := s.startOAuthLoginExchange(bound, p)
		if err != nil {
			return AuthorizedOAuthPreparation{}, err
		}
		defer releaseExchange()
		token, err := state.challenge.Exchange(bound, input)
		if err != nil {
			return AuthorizedOAuthPreparation{}, oauthLoginFailure("code exchange", err)
		}
		return s.finishOAuthAuthorization(bound, p, token)
	})
}
