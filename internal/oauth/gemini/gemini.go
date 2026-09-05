// Package gemini implements a built-in Crux provider backed by Google's
// Antigravity Cloud Code endpoint, driven through the Gemini API using a
// Google OAuth credential.
//
// The provider is registered as a built-in google-type provider. It logs in
// via the OAuth2 authorization-code flow with PKCE against a non-loopback
// redirect (the user pastes the resulting code back into the terminal) and
// shapes outgoing requests into the Antigravity request envelope so the
// v1internal endpoint accepts them (see transport.go).
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
	"slices"
	"strings"
	"sync"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
)

const (
	// ID is the provider identifier used across config, login, and models.
	ID = "gemini-ag"
	// Name is the human-readable provider name.
	Name = "Gemini / Antigravity (OAuth)"

	authorizeURL = "https://accounts.google.com/o/oauth2/auth"
	tokenURL     = "https://oauth2.googleapis.com/token"

	// redirectURI is a hosted callback page rather than a loopback server:
	// the user copies the code (or the whole callback URL) back into the
	// terminal.
	redirectURI = "https://antigravity.google/oauth-callback"

	// APIEndpoint is the Antigravity Cloud Code base URL. Requests are
	// rewritten onto the v1internal path by the transport.
	APIEndpoint = "https://daily-cloudcode-pa.googleapis.com"

	// loadCodeAssistURL resolves the Cloud AI Companion project bound to a
	// credential. The project is required in the request envelope.
	loadCodeAssistURL = APIEndpoint + "/v1internal:loadCodeAssist"

	// userInfoURL is used to label the stored credential with an account.
	userInfoURL = "https://www.googleapis.com/oauth2/v2/userinfo"

	// maxResponseBytes caps how much of an auth/metadata response we read.
	maxResponseBytes = 1 << 20
)

var scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
	"openid",
}

func oauthClientCredentials() (string, string, error) {
	clientID := strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("GEMINI_OAUTH_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return "", "", errors.New("Gemini OAuth client credentials are not configured; set GEMINI_OAUTH_CLIENT_ID and GEMINI_OAUTH_CLIENT_SECRET")
	}
	return clientID, clientSecret, nil
}

// version returns the Antigravity CLI version presented to the endpoint.

// modelSpec describes a single model exposed by the provider.
type modelSpec struct {
	name    string
	context int64
	output  int64
}

// geminiModels is the fallback model catalog used when the live model list
// cannot be fetched. The Antigravity endpoint serves Gemini, GPT-OSS, and
// Claude ids; all of them go through this package's transport.
var geminiModels = map[string]modelSpec{
	"gemini-3.1-pro-high":        {name: "Gemini 3.1 Pro (High)", context: 1_048_576, output: 65_535},
	"gemini-3.1-pro-low":         {name: "Gemini 3.1 Pro (Low)", context: 1_048_576, output: 65_535},
	"gemini-pro-agent":           {name: "Gemini Pro (agent)", context: 1_048_576, output: 65_535},
	"gemini-3-flash":             {name: "Gemini 3 Flash", context: 1_048_576, output: 65_536},
	"gemini-3-flash-agent":       {name: "Gemini 3.5 Flash (High)", context: 1_048_576, output: 65_536},
	"gemini-3.5-flash-low":       {name: "Gemini 3.5 Flash (Medium)", context: 1_048_576, output: 65_536},
	"gemini-3.5-flash-extra-low": {name: "Gemini 3.5 Flash (Low)", context: 1_048_576, output: 65_536},
	"gemini-3.6-flash-high":      {name: "Gemini 3.6 Flash (High)", context: 1_048_576, output: 65_536},
	"gemini-3.6-flash-medium":    {name: "Gemini 3.6 Flash (Medium)", context: 1_048_576, output: 65_536},
	"gemini-3.6-flash-low":       {name: "Gemini 3.6 Flash (Low)", context: 1_048_576, output: 65_536},
	"gemini-3.1-flash-lite":      {name: "Gemini 3.1 Flash Lite", context: 1_048_576, output: 65_535},
	"gemini-2.5-pro":             {name: "Gemini 2.5 Pro", context: 1_048_576, output: 65_535},
	"gpt-oss-120b-medium":        {name: "GPT-OSS 120B (Medium)", context: 131_072, output: 32_768},
	"claude-opus-4-6-thinking":   {name: "Claude Opus 4.6 (Thinking, via Antigravity)", context: 250_000, output: 64_000},
	"claude-sonnet-4-6":          {name: "Claude Sonnet 4.6 (Thinking, via Antigravity)", context: 250_000, output: 64_000},
}

// Models returns the Antigravity lineup as catalog models.
func Models() []catalog.Model {
	ids := make([]string, 0, len(geminiModels))
	for id := range geminiModels {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	models := make([]catalog.Model, 0, len(ids))
	for _, id := range ids {
		spec := geminiModels[id]
		model := catalog.Model{
			ID:               id,
			Name:             spec.name,
			ContextWindow:    spec.context,
			DefaultMaxTokens: spec.output,
			SupportsImages:   true,
		}
		if id != "gpt-oss-120b-medium" {
			model.CanReason = true
			model.ReasoningLevels = []string{"LOW", "MEDIUM", "HIGH"}
			model.DefaultReasoningEffort = "MEDIUM"
		}
		models = append(models, model)
	}
	return models
}

// CatalogProvider returns the built-in "gemini-ag" provider definition. It is
// appended to the known-provider catalog at load time so the provider is
// selectable and login can attach credentials to it.
func CatalogProvider() catalog.Provider {
	return catalog.Provider{
		Name:                Name,
		ID:                  catalog.ProviderID(ID),
		APIEndpoint:         APIEndpoint,
		Type:                catalog.TypeGoogle,
		DefaultLargeModelID: "gemini-3.1-pro-high",
		DefaultSmallModelID: "gemini-3-flash",
		Models:              Models(),
	}
}

// tokenResponse is the subset of the Google OAuth token response we use.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

// tokenRequest posts a form to the Google token endpoint and decodes the
// response. Non-2xx responses are returned as a TokenExchangeError so callers
// can detect a revoked refresh token and trigger interactive re-auth.
func tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
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
func ExchangeCode(ctx context.Context, code, verifier string) (*oauth.Token, error) {
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		return nil, err
	}
	tr, err := tokenRequest(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code_verifier": {verifier},
	})
	if err != nil {
		return nil, err
	}
	return toToken(tr, clientID), nil
}

// Refresh exchanges a refresh token for a fresh access token. Google does not
// rotate the refresh token here, so the previous one is carried forward.
func Refresh(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	clientID, clientSecret, err := oauthClientCredentials()
	if err != nil {
		return nil, err
	}
	tr, err := tokenRequest(ctx, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
		"client_secret": {clientSecret},
	})
	if err != nil {
		return nil, err
	}
	tok := toToken(tr, clientID)
	if tok.RefreshToken == "" {
		tok.RefreshToken = refreshToken
	}
	return tok, nil
}

func toToken(tr tokenResponse, clientID string) *oauth.Token {
	tok := &oauth.Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		ExpiresIn:    tr.ExpiresIn,
		Client: &oauth.OAuthClient{
			ClientID: clientID,
			AuthURL:  authorizeURL,
			TokenURL: tokenURL,
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
func Authorize(ctx context.Context, open func(string) error, readCode func() (string, error)) (*oauth.Token, error) {
	clientID, _, err := oauthClientCredentials()
	if err != nil {
		return nil, err
	}
	verifier, challenge, err := createPKCE()
	if err != nil {
		return nil, err
	}
	state, err := randomString(32)
	if err != nil {
		return nil, err
	}

	authURL := buildAuthorizeURL(challenge, state, clientID)
	if err := open(authURL); err != nil {
		return nil, err
	}

	pasted, err := readCode()
	if err != nil {
		return nil, err
	}

	code, gotState, err := parsePastedCode(pasted)
	if err != nil {
		return nil, err
	}
	if gotState != "" && gotState != state {
		return nil, errors.New("OAuth state mismatch — possible CSRF, please try again")
	}

	return ExchangeCode(ctx, code, verifier)
}

// parsePastedCode extracts the authorization code (and state, when present)
// from whatever the user pasted: a bare code, a query fragment, or the full
// callback URL.
func parsePastedCode(in string) (code, state string, err error) {
	in = strings.TrimSpace(in)
	if in == "" {
		return "", "", errors.New("no authorization code provided")
	}

	// Full callback URL or a bare query fragment.
	if strings.Contains(in, "code=") {
		raw := in
		if i := strings.Index(raw, "?"); i >= 0 {
			raw = raw[i+1:]
		}
		raw = strings.TrimPrefix(raw, "#")
		q, parseErr := url.ParseQuery(raw)
		if parseErr == nil && q.Get("code") != "" {
			return q.Get("code"), q.Get("state"), nil
		}
	}

	if strings.ContainsAny(in, " \t\n") {
		return "", "", errors.New("could not parse an authorization code from the pasted value")
	}
	return in, "", nil
}

func buildAuthorizeURL(challenge, state, clientID string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(scopes, " ")},
		"access_type":           {"offline"},
		"prompt":                {"consent"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
	}
	return authorizeURL + "?" + q.Encode()
}

// projectCache memoizes the Cloud AI Companion project per access token so
// the loadCodeAssist round-trip happens once per credential.
var projectCache sync.Map // access token -> project id

// Project resolves the Cloud AI Companion project bound to the credential.
// The value is required in the Antigravity request envelope. An empty string
// is returned (without error) when the endpoint does not provide one, which
// the endpoint tolerates for some accounts.
func Project(ctx context.Context, accessToken string) string {
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
		if value, ok := projectCache.Load(accessToken); ok {
			return value.(string)
		}
	}

	project := fetchProject(ctx, accessToken)
	if ownerBound {
		if err := providertransport.ValidateContextOwner(ctx); err != nil {
			return ""
		}
		return project
	}
	projectCache.Store(accessToken, project)
	return project
}

func fetchProject(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, loadCodeAssistURL, strings.NewReader("{}"))
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

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
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
func AccountEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userInfoURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("User-Agent", "Go-http-client/2.0")

	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
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
