package oauth

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
)

// CallbackRequirement describes where the UI receives an authorization code.
// It never permits the caller to supply an arbitrary redirect URL.
type CallbackRequirement struct {
	Mode string
	Port uint16
	Path string
}

func (r CallbackRequirement) Validate() error {
	switch r.Mode {
	case "hosted-paste":
		if r.Port != 0 || r.Path != "" {
			return errors.New("hosted OAuth callback cannot specify a local port or path")
		}
	case "loopback-fixed", "loopback-dynamic":
		if (r.Mode == "loopback-fixed") != (r.Port != 0) {
			return errors.New("OAuth callback port does not match its mode")
		}
		path, err := url.Parse(r.Path)
		if err != nil || len(r.Path) > 256 || !strings.HasPrefix(r.Path, "/") || path.Host != "" || path.RawQuery != "" || path.Fragment != "" || path.EscapedPath() != r.Path {
			return errors.New("OAuth callback path is invalid")
		}
	default:
		return errors.New("OAuth callback mode is unsupported")
	}
	return nil
}

// ValidatePort checks the already-bound UI listener before challenge creation.
func (r CallbackRequirement) ValidatePort(port uint16) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Mode == "hosted-paste" && port != 0 || r.Mode == "loopback-dynamic" && port == 0 || r.Mode == "loopback-fixed" && port != r.Port {
		return errors.New("OAuth callback port does not match the captured requirement")
	}
	return nil
}

// CodeChallenge contains private interaction and exchange state. Value copies
// share the same single attempt; tokens and callback input never appear in JSON
// or formatting. The authorization URL is available only through its accessor.
type CodeChallenge struct{ state *codeChallengeState }

type codeChallengeState struct {
	ctx       context.Context
	cancel    context.CancelFunc
	url       string
	expiresAt time.Time
	exchange  func(context.Context, string) (*Token, error)
	mu        sync.Mutex
	attempted bool
	input     string
	done      chan struct{}
	token     *Token
	err       error
}

// NewCodeChallenge is for trusted host OAuth adapters. It does not authorize
// configuration publication and cannot mint the config layer's login receipt.
// The adapter captures its registration/client/parser and performs owner checks.
func NewCodeChallenge(ctx context.Context, authorizationURL string, expiresAt time.Time, exchange func(context.Context, string) (*Token, error)) (*CodeChallenge, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if authorizationURL == "" || exchange == nil {
		return nil, errors.New("OAuth challenge is incomplete")
	}
	var cancel context.CancelFunc
	if expiresAt.IsZero() {
		ctx, cancel = context.WithCancel(ctx)
	} else {
		ctx, cancel = context.WithDeadline(ctx, expiresAt)
	}
	if deadline, ok := ctx.Deadline(); ok {
		expiresAt = deadline
	}
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	return &CodeChallenge{state: &codeChallengeState{ctx: ctx, cancel: cancel, url: authorizationURL, expiresAt: expiresAt, exchange: exchange, done: make(chan struct{})}}, nil
}

func (CodeChallenge) Format(s fmt.State, verb rune) { fmt.Fprint(s, "[private OAuth code challenge]") }

func (CodeChallenge) MarshalJSON() ([]byte, error) {
	return nil, errors.New("OAuth code challenge is private")
}

func (c *CodeChallenge) AuthorizationURL() string {
	if c == nil || c.state == nil {
		return ""
	}
	return c.state.url
}

func (c *CodeChallenge) ExpiresAt() time.Time {
	if c == nil || c.state == nil {
		return time.Time{}
	}
	return c.state.expiresAt
}

func (c *CodeChallenge) Close() {
	if c != nil && c.state != nil {
		c.state.cancel()
	}
}

func (c *CodeChallenge) Exchange(ctx context.Context, input string) (*Token, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.state == nil {
		return nil, errors.New("OAuth code challenge is unavailable")
	}
	s := c.state
	s.mu.Lock()
	if s.attempted {
		if s.input != input {
			s.mu.Unlock()
			return nil, errors.New("OAuth code challenge already received different input")
		}
		s.mu.Unlock()
		select {
		case <-s.done:
			return cloneChallengeToken(s.token), s.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	s.attempted, s.input = true, input
	s.mu.Unlock()
	// Preserve preparing context values (owner and captured environment); only
	// the submitting caller's cancellation is added to that authority.
	work, cancel := context.WithCancel(s.ctx)
	stop := context.AfterFunc(ctx, cancel)
	if ctx.Err() != nil {
		cancel()
	}
	var token *Token
	err := work.Err()
	if err == nil && len(input) > 64<<10 {
		err = errors.New("OAuth callback input exceeds its limit")
	}
	if err == nil {
		token, err = s.exchange(work, input)
	}
	if err == nil {
		err = work.Err()
	}
	stop()
	cancel()
	if err != nil {
		token = nil
	}
	s.token, s.err = cloneChallengeToken(token), err
	close(s.done)
	return cloneChallengeToken(s.token), s.err
}

func cloneChallengeToken(token *Token) *Token {
	if token == nil {
		return nil
	}
	value := *token
	if token.Client != nil {
		client := *token.Client
		value.Client = &client
	}
	return &value
}
