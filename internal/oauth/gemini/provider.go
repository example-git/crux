package gemini

import (
	"net/http"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/gemini/antigravity"
	"github.com/example-git/crux/internal/oauth/useragent"
)

// TokenSource supplies a current access token for outgoing requests. It is
// called per request so refreshed credentials are picked up without
// rebuilding the provider.
type TokenSource = antigravity.TokenSource

// UserAgent returns the Antigravity CLI user agent presented to the endpoint.
func UserAgent() string {
	return useragent.Gemini()
}

// NewProvider builds the Antigravity fantasy provider: a native
// Antigravity-dialect provider pointed at the Cloud Code endpoint,
// authenticating with the given OAuth token source and resolving the Cloud
// AI Companion project from the credential.
//
// Every model in the lineup (Gemini, GPT-OSS, and Claude ids alike) is served
// over the Gemini protocol, which is what the endpoint expects.
func NewProvider(baseURL string, token TokenSource, headers map[string]string) (fantasy.Provider, error) {
	if baseURL == "" {
		baseURL = APIEndpoint
	}
	opts := []antigravity.Option{
		antigravity.WithBaseURL(baseURL),
		antigravity.WithName(ID),
		antigravity.WithHTTPClient(http.DefaultClient),
		antigravity.WithUserAgent(UserAgent()),
		antigravity.WithTokenSource(token),
		antigravity.WithProjectLoader(Project),
	}
	if len(headers) > 0 {
		opts = append(opts, antigravity.WithHeaders(headers))
	}
	return antigravity.New(opts...)
}
