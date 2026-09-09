package mcpoauth

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/example-git/crux/internal/providertransport"
	"golang.org/x/oauth2"
)

// ResourceHTTPClient binds every resource dispatch, including an SSE-advertised
// POST endpoint, to the configured MCP origin. The guard wraps the fully
// composed transport so it runs before bearer-token lookup or header injection.
// Redirect callbacks still run before the shared final-destination check.
func ResourceHTTPClient(base *http.Client, endpoint string) *http.Client {
	client := providertransport.CapturedOriginHTTPClient(base, endpoint)
	guard := providertransport.CapturedOriginHTTPClient(&http.Client{}, endpoint).CheckRedirect
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	bound := &resourceDestinationTransport{base: transport, guard: guard}
	redirect := client.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		err := redirect(request, via)
		bound.refusal.record(err)
		return err
	}
	client.Transport = bound
	return client
}

type resourceDestinationTransport struct {
	base    http.RoundTripper
	guard   func(*http.Request, []*http.Request) error
	refusal oauthFlowState
}

func (t *resourceDestinationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if err := t.guard(request, nil); err != nil {
		t.refusal.record(err)
		if request != nil && request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	response, err := t.base.RoundTrip(request)
	t.refusal.record(err)
	return response, err
}

// ResourceHTTPError preserves an initialization refusal when the MCP SDK's
// protocol fallback replaces a failed SSE Write with a final connection EOF.
// The client belongs to one session attempt. This only annotates a failed
// result; it neither replaces success nor disables later protocol requests.
func ResourceHTTPError(client *http.Client, err error) error {
	if client == nil || err == nil {
		return err
	}
	if bound, ok := client.Transport.(*resourceDestinationTransport); ok {
		if refusal := bound.refusal.current(); refusal != nil && !errors.Is(err, refusal) {
			return errors.Join(refusal, err)
		}
	}
	return err
}

// discoveredEndpointHTTPClient retains each request's selected origin rather
// than pinning all OAuth traffic to the resource server. Discovery may select
// independently hosted authorization, token and registration endpoints.
func discoveredEndpointHTTPClient(base *http.Client) *http.Client {
	client := *base
	previous := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		if len(via) == 0 || via[0] == nil || via[0].URL == nil {
			return errors.New("MCP OAuth redirect has no captured endpoint")
		}
		scoped := providertransport.CapturedOriginHTTPClient(&http.Client{CheckRedirect: previous}, via[0].URL.String())
		return scoped.CheckRedirect(request, via)
	}
	return &client
}

// oauthFlowState retains a destination refusal only for one logical OAuth
// attempt. oauth2's AuthStyleAutoDetect otherwise retries any first failure,
// including a refused redirect, with the secret moved into the request body.
type oauthFlowState struct {
	mu      sync.Mutex
	refusal error
}
type oauthFlowKey struct{}

func (s *oauthFlowState) current() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refusal
}
func (s *oauthFlowState) record(err error) {
	if s == nil || !nonRetryableOAuthHTTPError(err) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refusal == nil {
		s.refusal = err
	}
}
func nonRetryableOAuthHTTPError(err error) bool {
	var refusal interface{ NonRetryable() bool }
	return errors.As(err, &refusal) && refusal.NonRetryable()
}
func oauthFlowFrom(ctx context.Context) *oauthFlowState {
	state, _ := ctx.Value(oauthFlowKey{}).(*oauthFlowState)
	return state
}

func oauthFlowHTTPClient(base *http.Client) *http.Client {
	client := *base
	previous := base.CheckRedirect
	client.CheckRedirect = func(request *http.Request, via []*http.Request) error {
		var err error
		if previous != nil {
			err = previous(request, via)
		} else if len(via) >= 10 {
			err = errors.New("stopped after 10 redirects")
		}
		oauthFlowFrom(request.Context()).record(err)
		return err
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &oauthFlowTransport{base: transport}
	return &client
}

type oauthFlowTransport struct{ base http.RoundTripper }

func (t *oauthFlowTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	state := oauthFlowFrom(request.Context())
	if err := state.current(); err != nil {
		if request.Body != nil {
			_ = request.Body.Close()
		}
		return nil, err
	}
	response, err := t.base.RoundTrip(request)
	state.record(err)
	return response, err
}

type oauthFlowTokenSource struct {
	mu     sync.Mutex
	state  *oauthFlowState
	source oauth2.TokenSource
}

func (s *oauthFlowTokenSource) Token() (*oauth2.Token, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state.mu.Lock()
	s.state.refusal = nil
	s.state.mu.Unlock()
	return s.source.Token()
}

func newEndpointTokenSource(ctx context.Context, cfg *oauth2.Config, initial *oauth2.Token) oauth2.TokenSource {
	state := &oauthFlowState{}
	client := oauthFlowHTTPClient(providertransport.CapturedOriginHTTPClient(NewSessionHTTPClient(ctx), cfg.Endpoint.TokenURL))
	flow := context.WithValue(ctx, oauthFlowKey{}, state)
	flow = context.WithValue(flow, oauth2.HTTPClient, client)
	return &oauthFlowTokenSource{state: state, source: cfg.TokenSource(flow, initial)}
}
