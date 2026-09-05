package gemini

import (
	"context"
	"fmt"
	"net/http"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/gemini/antigravity"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providertransport"
)

// TokenSource supplies a current access token for outgoing requests. It is
// called per request so refreshed credentials are picked up without
// rebuilding the provider.
type TokenSource = antigravity.TokenSource

// UserAgent returns the Antigravity CLI user agent presented to the endpoint.
func UserAgent() string {
	return useragent.Gemini()
}

func UserAgentForContext(ctx context.Context) (string, error) {
	return useragent.GeminiForContext(ctx)
}

// NewProvider builds the Antigravity fantasy provider: a native
// Antigravity-dialect provider pointed at the Cloud Code endpoint,
// authenticating with the given OAuth token source and resolving the Cloud
// AI Companion project from the credential.
//
// Every model in the lineup (Gemini, GPT-OSS, and Claude ids alike) is served
// over the Gemini protocol, which is what the endpoint expects.
func inferenceHTTPClient(operation *providertransport.Operation, validate providertransport.OwnerValidator) *http.Client {
	httpClient := http.DefaultClient
	if operation != nil {
		httpClient = operation.HTTPClient(http.DefaultClient)
		httpClient.Transport = providertransport.TransportWithStreamIdleTimeout(
			providertransport.TransportWithConnectTimeout(httpClient.Transport, operation.ConnectTimeout),
			operation.StreamIdleTimeout,
		)
	}
	return providertransport.ClientWithOwnerValidator(httpClient, validate)
}

func NewProvider(baseURL string, token TokenSource, headers map[string]string, operation *providertransport.Operation, validate providertransport.OwnerValidator) (fantasy.Provider, error) {
	if validate == nil {
		return nil, fmt.Errorf("Gemini provider owner validator is unavailable")
	}
	if baseURL == "" {
		baseURL = APIEndpoint
	}
	httpClient := inferenceHTTPClient(operation, validate)
	opts := []antigravity.Option{
		antigravity.WithBaseURL(baseURL),
		antigravity.WithName(ID),
		antigravity.WithHTTPClient(httpClient),
		antigravity.WithUserAgent(UserAgent()),
		antigravity.WithTokenSource(token),
		antigravity.WithProjectLoader(func(ctx context.Context, token string) string {
			return Project(providertransport.ContextWithOwnerValidator(ctx, validate), token)
		}),
	}
	if operation != nil {
		opts = append(opts,
			antigravity.WithRetryPolicy(operation.Retry),
			antigravity.WithErrorMappings(operation.Errors),
		)
		if operation.Streaming != nil {
			opts = append(opts, antigravity.WithMaxEventBytes(operation.Streaming.MaxEventBytes))
		}
	}
	if len(headers) > 0 {
		opts = append(opts, antigravity.WithHeaders(headers))
	}
	return antigravity.New(opts...)
}
