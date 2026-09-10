// Package gemini implements native OAuth and project metadata mechanics for
// manifest-bound Antigravity clients. Provider bundles supply endpoint policy.
package gemini

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
)

const (
	// ID is the provider identifier used across config, login, and models.
	ID = "gemini-ag"
	// Name is the human-readable provider name.
	Name = "Gemini / Antigravity (OAuth)"

	// maxResponseBytes caps how much of an auth/metadata response we read.
	maxResponseBytes = 1 << 20
)

func oauthClientCredentials(ctx context.Context) (string, string, error) {
	clientID, _ := oauth.LookupEnvironment(ctx, "GEMINI_OAUTH_CLIENT_ID")
	clientSecret, _ := oauth.LookupEnvironment(ctx, "GEMINI_OAUTH_CLIENT_SECRET")
	clientID, clientSecret = strings.TrimSpace(clientID), strings.TrimSpace(clientSecret)
	if clientID == "" || clientSecret == "" {
		return "", "", errors.New("Gemini OAuth client credentials are not configured; set GEMINI_OAUTH_CLIENT_ID and GEMINI_OAUTH_CLIENT_SECRET")
	}
	return clientID, clientSecret, nil
}

// version returns the Antigravity CLI version presented to the endpoint.

// tokenResponse is the subset of the Google OAuth token response we use.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

// tokenRequest posts a form to the Google token endpoint and decodes the
// response. Non-2xx responses are returned as a TokenExchangeError so callers
// can detect a revoked refresh token and trigger interactive re-auth.
func (client Client) tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.Token.BaseURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

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
		detail := strings.TrimSpace(string(data))
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return tokenResponse{}, &oauth.TokenExchangeError{StatusCode: resp.StatusCode, Body: detail}
	}

	var tr tokenResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return tokenResponse{}, err
	}
	if tr.AccessToken == "" {
		return tokenResponse{}, errors.New("Google OAuth response did not include an access token")
	}
	return tr, nil
}

// ExchangeCode exchanges an authorization code for an access token.
func (client Client) ExchangeCode(ctx context.Context, code, verifier string) (*oauth.Token, error) {
	clientID, clientSecret, err := oauthClientCredentials(ctx)
	if err != nil {
		return nil, err
	}
	return client.exchangeCodeWithClientCredentials(ctx, code, verifier, clientID, clientSecret)
}

func (client Client) exchangeCodeWithClientCredentials(ctx context.Context, code, verifier, clientID, clientSecret string) (*oauth.Token, error) {
	tr, err := client.tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {client.RedirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	return client.toToken(tr, clientID), nil
}

// Refresh exchanges a refresh token for a fresh access token. Google does not
// rotate the refresh token here, so the previous one is carried forward.
func (client Client) Refresh(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	clientID, clientSecret, err := oauthClientCredentials(ctx)
	if err != nil {
		return nil, err
	}
	tr, err := client.tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if err != nil {
		return nil, err
	}
	tok := client.toToken(tr, clientID)
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func (client Client) toToken(tr tokenResponse, clientID string) *oauth.Token {
	tok := &oauth.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresIn:    tr.ExpiresIn,
		Client: &oauth.OAuthClient{
			ClientID: clientID,
			AuthURL:  client.Authorization.BaseURL,
			TokenURL: client.Token.BaseURL,
		},
	}
	if tok.ExpiresIn <= 0 {
		tok.ExpiresIn = 3600
	}
	tok.SetExpiresAt()
	return tok
}

// Authorize runs the browser-based OAuth authorization-code flow with PKCE.
//
// Antigravity registers a hosted (non-loopback) redirect, so there is no
// callback server to listen on: open is called with the authorization URL and
// readCode must return whatever the user pasted back, which may be the bare
// code, a "code=..." fragment, or the full callback URL.
func (client Client) Authorize(ctx context.Context, open func(string) error, readCode func() (string, error)) (*oauth.Token, error) {
	challenge, err := client.PrepareCode(ctx, 0)
	if err != nil {
		return nil, err
	}
	defer challenge.Close()
	if err := providertransport.OpenURLWithContextOwnerValidator(ctx, open, challenge.AuthorizationURL()); err != nil {
		return nil, err
	}
	if readCode == nil {
		return nil, errors.New("Gemini OAuth requires pasted callback input")
	}
	type codeResult struct {
		value string
		err   error
	}
	codes := make(chan codeResult, 1)
	go func() { value, err := readCode(); codes <- codeResult{value, err} }()
	var pasted string
	select {
	case result := <-codes:
		if result.err != nil {
			return nil, result.err
		}
		pasted = result.value
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	return challenge.Exchange(ctx, pasted)
}

// parsePastedCode extracts the authorization code (and state, when present)
// from whatever the user pasted: a bare code, a query fragment, or the full
// callback URL.
func parsePastedCode(in string) (code, state string, err error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "", errors.New("no authorization code provided")
	}

	// Callback-shaped input must validate as a callback. Do not send malformed
	// queries, duplicated state/code, or provider errors as a bare token code.
	raw, callback := in, strings.Contains(in, "code=") || strings.Contains(in, "state=") || strings.Contains(in, "error=")
	if parsed, parseErr := url.Parse(in); parseErr == nil && parsed.IsAbs() {
		callback = true
		raw = parsed.RawQuery
		if raw == "" {
			raw = parsed.Fragment
		}
	} else if i := strings.IndexByte(raw, '?'); i >= 0 {
		raw, callback = raw[i+1:], true
	}
	if callback {
		raw = strings.TrimPrefix(raw, "#")
		q, parseErr := url.ParseQuery(raw)
		if parseErr != nil || len(q["code"]) != 1 || len(q["state"]) > 1 || len(q["error"]) > 0 || q.Get("code") == "" {
			return "", "", errors.New("could not parse an authorization code from the pasted callback")
		}
		return q.Get("code"), q.Get("state"), nil
	}

	if strings.ContainsAny(in, " \t\r\n") {
		return "", "", errors.New("could not parse an authorization code from the pasted value")
	}
	return in, "", nil
}

func (client Client) buildAuthorizeURL(challenge, state, clientID string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {client.RedirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(client.Scopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return client.Authorization.BaseURL + "?" + q.Encode()
}

// projectCache memoizes the Cloud AI Companion project per access token so
// the loadCodeAssist round-trip happens once per credential.
var projectCache sync.Map // access token -> project id

// Project resolves the Cloud AI Companion project bound to the credential.
// The value is required in the Antigravity request envelope. An empty string
// is returned (without error) when the endpoint does not provide one, which
// the endpoint tolerates for some accounts.
func (client Client) Project(ctx context.Context, accessToken string) string {
	ownerBound := providertransport.OwnerValidatorFromContext(ctx) != nil
	if ownerBound {
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return ""
		}
	}
	if value := os.Getenv("GEMINI_PROJECT_ID"); value != "" {
		return value
	}
	if !ownerBound {
		if value, ok := projectCache.Load(client.ProjectEndpoint.BaseURL + "\x00" + accessToken); ok {
			return value.(string)
		}
	}

	project := client.fetchProject(ctx, accessToken)
	if ownerBound {
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return ""
		}
		return project
	}
	projectCache.Store(client.ProjectEndpoint.BaseURL+"\x00"+accessToken, project)
	return project
}

// ProjectForCredential resolves optional metadata without process environment
// overrides or the unscoped cache. Empty metadata retains the provider's existing
// empty-project behavior for accounts that do not return a project identifier.
func (client Client) ProjectForCredential(ctx context.Context, accessToken string) string {
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return ""
	}
	project := client.fetchProject(ctx, accessToken)
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return ""
	}
	return project
}

func (client Client) fetchProject(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.ProjectEndpoint.BaseURL, strings.NewReader("{}"))
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	userAgent, err := UserAgentForContext(ctx)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.EndpointHTTPClient(http.DefaultClient, client.ProjectEndpoint)).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var out struct {
		CloudaicompanionProject string `json:"cloudaicompanionProject"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	return out.CloudaicompanionProject
}

// AccountEmail returns the email associated with the credential, used to
// label the stored account. Errors are non-fatal and yield an empty string.
func (client Client) AccountEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, client.Identity.BaseURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.EndpointHTTPClient(http.DefaultClient, client.Identity)).Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var out struct {
		Email string `json:"email"`
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return ""
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return ""
	}
	return out.Email
}

func createPKCE() (verifier, challenge string, err error) {
	verifier, err = randomString(64)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func randomString(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate random string: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf)[:n], nil
}
