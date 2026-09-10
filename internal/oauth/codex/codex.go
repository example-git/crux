// Package codex implements native OAuth mechanics for a manifest-bound Codex client.
// Endpoints and scopes are supplied by the registered provider bundle.
package codex

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callback"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providertransport"
)

const (
	// ID is the provider identifier.
	ID = "codex"
	// Name is the human-readable provider name.
	Name = "Codex (ChatGPT)"

	// redirectPort is the fixed loopback port the OAuth client registration
	// expects for its redirect URI.
	redirectPort = 1455
	redirectPath = "/auth/callback"

	authorizeTimeout = 5 * time.Minute

	maxResponseBytes = 1 << 20
)

func oauthClientID(ctx context.Context) (string, error) {
	clientID, _ := oauth.LookupEnvironment(ctx, "CODEX_OAUTH_CLIENT_ID")
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return "", errors.New("Codex OAuth client ID is not configured; set CODEX_OAUTH_CLIENT_ID")
	}
	return clientID, nil
}

// tokenResponse is the subset of the OpenAI token response we use.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

func (client Client) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Token.BaseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.EndpointHTTPClient(http.DefaultClient, client.Token)).Do(req)
	if err != nil {
		return tokenResponse{}, err
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return tokenResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return tokenResponse{}, &oauth.TokenExchangeError{
			StatusCode: resp.StatusCode,
			Body:       strings.TrimSpace(string(data)),
		}
	}
	var out tokenResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return tokenResponse{}, err
	}
	if out.AccessToken == "" {
		return tokenResponse{}, errors.New("codex: token response missing access_token")
	}
	return out, nil
}

// ExchangeCode exchanges an authorization code for tokens.
func (client Client) ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*oauth.Token, error) {
	clientID, err := oauthClientID(ctx)
	if err != nil {
		return nil, err
	}
	return client.exchangeCodeWithClientID(ctx, code, verifier, redirectURI, clientID)
}

func (client Client) exchangeCodeWithClientID(ctx context.Context, code, verifier, redirectURI, clientID string) (*oauth.Token, error) {
	res, err := client.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {clientID},
		"redirect_uri":  {redirectURI},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	token := &oauth.Token{
		AccessToken:  res.AccessToken,
		RefreshToken: res.RefreshToken,
		ExpiresIn:    res.ExpiresIn,
	}
	token.SetExpiresAt()
	return token, nil
}

// RefreshToken refreshes an expired access token.
func (client Client) RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	clientID, err := oauthClientID(ctx)
	if err != nil {
		return nil, err
	}
	res, err := client.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {clientID},
		"refresh_token": {refreshToken},
	})
	if err != nil {
		return nil, err
	}
	next := res.RefreshToken
	if next == "" {
		next = refreshToken
	}
	token := &oauth.Token{
		AccessToken:  res.AccessToken,
		RefreshToken: next,
		ExpiresIn:    res.ExpiresIn,
	}
	token.SetExpiresAt()
	return token, nil
}

// Authorize runs the browser-based OAuth authorization-code flow with PKCE.
// It binds the fixed loopback port the Codex client registration requires
// and blocks until the browser completes the callback or ctx is cancelled.
func (client Client) Authorize(ctx context.Context, open func(string) error) (*oauth.Token, error) {
	challenge, err := client.PrepareCode(ctx, redirectPort)
	if err != nil {
		return nil, err
	}
	defer challenge.Close()
	ctx, cancel := context.WithDeadline(ctx, challenge.ExpiresAt())
	defer cancel()
	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", fmt.Sprintf("localhost:%d", redirectPort))
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server on port %d: %w", redirectPort, err)
	}

	type result struct {
		token *oauth.Token
		err   error
	}
	resultCh := make(chan result, 1)
	var finishOnce sync.Once
	finish := func(value result) { finishOnce.Do(func() { resultCh <- value }) }
	var claimed atomic.Bool

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != redirectPath {
			http.NotFound(w, r)
			return
		}
		if !claimed.CompareAndSwap(false, true) {
			http.Error(w, "OAuth callback already received.", http.StatusConflict)
			return
		}
		if err := ctx.Err(); err != nil {
			http.Error(w, "OAuth authorization has ended.", http.StatusGone)
			finish(result{err: err})
			return
		}
		token, err := challenge.Exchange(ctx, r.URL.RawQuery)
		if err != nil {
			_ = callback.Serve(w, callback.Result{
				Subject:          Name,
				ErrorCode:        "token_exchange_failed",
				ErrorDescription: "Authorization could not be completed.",
			})
			finish(result{err: err})
			return
		}
		_ = callback.Serve(w, callback.Result{Subject: Name})
		finish(result{token: token})
	})

	server := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 15 * time.Second}
	go func() {
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			finish(result{err: errors.New("OAuth callback server stopped")})
		}
	}()
	defer func() {
		cancel()
		shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		_ = server.Shutdown(shutdownCtx)
		_ = server.Close()
	}()

	authURL := challenge.AuthorizationURL()
	if open != nil {
		if err := providertransport.OpenURLWithContextOwnerValidator(ctx, open, authURL); err != nil {
			return nil, fmt.Errorf("open authorization URL: %w", err)
		}
	}

	select {
	case res := <-resultCh:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return res.token, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (client Client) buildAuthorizeURL(redirectURI, challenge, state, clientID string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(client.Scopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		// Extra params the Codex CLI sends.
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return client.Authorization.BaseURL + "?" + q.Encode()
}

// AccountID extracts the ChatGPT account id from the JWT access token's
// "https://api.openai.com/auth" claim. Returns "" on any failure.
func AccountID(accessToken string) string {
	parts := strings.Split(accessToken, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

// AccountEmail returns the email associated with the credential via the
// whoami endpoint. Errors are non-fatal and yield an empty string.
func (client Client) AccountEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Identity.BaseURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("originator", useragent.CodexOriginatorForContext(ctx))
	userAgent, err := useragent.CodexForContext(ctx)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", userAgent)

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.EndpointHTTPClient(http.DefaultClient, client.Identity)).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return ""
	}
	var out struct {
		Email string `json:"email"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	return out.Email
}

func createPKCE() (verifier, challenge string, err error) {
	verifier, err = randomString(32)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
