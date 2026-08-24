package codex

import (
	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/useragent"
)

// TokenSource supplies a current access token for outgoing requests. It is
// called per request so refreshed credentials are picked up without
// rebuilding the provider.
type TokenSource = responses.TokenSource

// AccountIDSource supplies the ChatGPT account id paired with the OAuth token.
type AccountIDSource = responses.AccountIDSource

// NewProvider builds the Codex fantasy provider: a native Responses-over-
// WebSocket adapter pointed at the ChatGPT Codex endpoint, presenting the
// Codex CLI identity and authenticating with the given OAuth token source.
func NewProvider(baseURL string, token TokenSource, accountID AccountIDSource, headers map[string]string, sessionStore *responses.SessionStore) (fantasy.Provider, error) {
	if baseURL == "" {
		baseURL = APIEndpoint
	}
	opts := []responses.Option{
		responses.WithURL(baseURL),
		responses.WithName(ID),
		responses.WithTokenSource(token),
		responses.WithAccountIDSource(accountID),
		responses.WithUserAgent(useragent.Codex()),
		responses.WithOriginator(useragent.CodexOriginator()),
		responses.WithVersion(useragent.CodexVersion()),
		responses.WithSessionStore(sessionStore),
	}
	if len(headers) > 0 {
		opts = append(opts, responses.WithHeaders(headers))
	}
	return responses.New(opts...)
}
