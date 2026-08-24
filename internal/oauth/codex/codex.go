// Package codex implements the OpenAI Codex OAuth flow and provider
// definition. Codex authenticates with a ChatGPT-account OAuth token
// (PKCE authorization-code flow against auth.openai.com) and serves models
// over the Responses API on a WebSocket endpoint; see the responses
// subpackage for the native adapter.
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
	"os"
	"slices"
	"strings"
	"time"

	"charm.land/catwalk/pkg/catwalk"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/callback"
	"github.com/example-git/crux/internal/oauth/useragent"
)

const (
	// ID is the provider identifier.
	ID = "codex"
	// Name is the human-readable provider name.
	Name = "Codex (ChatGPT)"

	authBase     = "https://auth.openai.com"
	authorizeURL = authBase + "/oauth/authorize"
	tokenURL     = authBase + "/oauth/token"
	whoamiURL    = authBase + "/api/accounts/v1/user-auth-credential/whoami"

	// APIEndpoint is the Codex Responses WebSocket endpoint.
	APIEndpoint = "wss://chatgpt.com/backend-api/codex/responses"

	// redirectPort is the fixed loopback port the OAuth client registration
	// expects for its redirect URI.
	redirectPort = 1455
	redirectPath = "/auth/callback"

	authorizeTimeout = 5 * time.Minute

	maxResponseBytes = 1 << 20
)

var scopes = []string{
	"openid", "profile", "email", "offline_access",
	"api.connectors.read", "api.connectors.invoke",
}

func oauthClientID() (string, error) {
	clientID := strings.TrimSpace(os.Getenv("CODEX_OAUTH_CLIENT_ID"))
	if clientID == "" {
		return "", errors.New("Codex OAuth client ID is not configured; set CODEX_OAUTH_CLIENT_ID")
	}
	return clientID, nil
}

// modelSpec describes a single model exposed by the provider.
type modelSpec struct {
	name    string
	context int64
	output  int64
}

// codexModels is the model catalog served over the Codex endpoint.
var codexModels = map[string]modelSpec{
	"gpt-5.6-sol":         {name: "GPT-5.6 Sol", context: 272_000, output: 128_000},
	"gpt-5.6-terra":       {name: "GPT-5.6 Terra", context: 272_000, output: 128_000},
	"gpt-5.6-luna":        {name: "GPT-5.6 Luna", context: 272_000, output: 128_000},
	"gpt-5.5":             {name: "GPT-5.5", context: 272_000, output: 128_000},
	"gpt-5.4":             {name: "GPT-5.4", context: 272_000, output: 128_000},
	"gpt-5.3-codex-spark": {name: "GPT-5.3 Codex Spark", context: 272_000, output: 128_000},
	"gpt-5.3-codex":       {name: "GPT-5.3 Codex", context: 272_000, output: 128_000},
	"gpt-5.2-codex":       {name: "GPT-5.2 Codex", context: 272_000, output: 128_000},
	"gpt-5.2":             {name: "GPT-5.2", context: 272_000, output: 128_000},
	"gpt-5.1-codex-max":   {name: "GPT-5.1 Codex Max", context: 272_000, output: 128_000},
	"gpt-5.1-codex":       {name: "GPT-5.1 Codex", context: 272_000, output: 128_000},
	"gpt-5-codex":         {name: "GPT-5 Codex", context: 272_000, output: 128_000},
	"gpt-5.1":             {name: "GPT-5.1", context: 272_000, output: 128_000},
	"gpt-5":               {name: "GPT-5", context: 272_000, output: 128_000},
	"gpt-5.1-codex-mini": {
		name: "GPT-5.1 Codex Mini", context: 272_000, output: 128_000,
	},
}

// Models returns the Codex lineup as catwalk models.
func Models() []catwalk.Model {
	ids := make([]string, 0, len(codexModels))
	for id := range codexModels {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	models := make([]catwalk.Model, 0, len(ids))
	for _, id := range ids {
		spec := codexModels[id]
		models = append(models, catwalk.Model{
			ID:                     id,
			Name:                   spec.name,
			ContextWindow:          spec.context,
			DefaultMaxTokens:       spec.output,
			CanReason:              true,
			ReasoningLevels:        []string{"low", "medium", "high", "xhigh"},
			DefaultReasoningEffort: "medium",
			SupportsImages:         true,
		})
	}
	return models
}

// CatwalkProvider returns the built-in "codex" provider definition.
func CatwalkProvider() catwalk.Provider {
	return catwalk.Provider{
		Name:                Name,
		ID:                  catwalk.InferenceProvider(ID),
		APIEndpoint:         APIEndpoint,
		Type:                catwalk.TypeOpenAI,
		DefaultLargeModelID: "gpt-5.5",
		DefaultSmallModelID: "gpt-5.1-codex-mini",
		Models:              Models(),
	}
}

// tokenResponse is the subset of the OpenAI token response we use.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	IDToken      string `json:"id_token,omitempty"`
	ExpiresIn    int    `json:"expires_in,omitempty"`
}

func tokenRequest(ctx context.Context, form url.Values) (tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := http.DefaultClient.Do(req)
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
func ExchangeCode(ctx context.Context, code, verifier, redirectURI string) (*oauth.Token, error) {
	clientID, err := oauthClientID()
	if err != nil {
		return nil, err
	}
	res, err := tokenRequest(ctx, url.Values{
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
func RefreshToken(ctx context.Context, refreshToken string) (*oauth.Token, error) {
	clientID, err := oauthClientID()
	if err != nil {
		return nil, err
	}
	res, err := tokenRequest(ctx, url.Values{
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
func Authorize(ctx context.Context, open func(string) error) (*oauth.Token, error) {
	clientID, err := oauthClientID()
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

	listener, err := new(net.ListenConfig).Listen(ctx, "tcp", fmt.Sprintf("localhost:%d", redirectPort))
	if err != nil {
		return nil, fmt.Errorf("start OAuth callback server on port %d: %w", redirectPort, err)
	}
	redirectURI := fmt.Sprintf("http://localhost:%d%s", redirectPort, redirectPath)

	type result struct {
		token *oauth.Token
		err   error
	}
	resultCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(redirectPath, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("state") != state || q.Get("code") == "" {
			_ = callback.Serve(w, callback.Result{
				Subject:          Name,
				ErrorCode:        "invalid_request",
				ErrorDescription: "Invalid OAuth callback.",
			})
			resultCh <- result{err: errors.New("codex OAuth callback validation failed")}
			return
		}
		token, err := ExchangeCode(r.Context(), q.Get("code"), verifier, redirectURI)
		if err != nil {
			_ = callback.Serve(w, callback.Result{
				Subject:          Name,
				ErrorCode:        "token_exchange_failed",
				ErrorDescription: err.Error(),
			})
			resultCh <- result{err: err}
			return
		}
		_ = callback.Serve(w, callback.Result{Subject: Name})
		resultCh <- result{token: token}
	})

	server := &http.Server{Handler: mux}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	authURL := buildAuthorizeURL(redirectURI, challenge, state, clientID)
	if open != nil {
		if err := open(authURL); err != nil {
			return nil, fmt.Errorf("open authorization URL: %w", err)
		}
	}

	select {
	case res := <-resultCh:
		return res.token, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(authorizeTimeout):
		return nil, errors.New("codex OAuth authorization timed out")
	}
}

func buildAuthorizeURL(redirectURI, challenge, state, clientID string) string {
	q := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {redirectURI},
		"response_type":         {"code"},
		"scope":                 {strings.Join(scopes, " ")},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"state":                 {state},
		// Extra params the Codex CLI sends.
		"id_token_add_organizations": {"true"},
		"codex_cli_simplified_flow":  {"true"},
	}
	return authorizeURL + "?" + q.Encode()
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
func AccountEmail(ctx context.Context, accessToken string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whoamiURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("originator", useragent.CodexOriginator())
	req.Header.Set("User-Agent", useragent.Codex())

	resp, err := http.DefaultClient.Do(req)
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
