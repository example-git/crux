package codex

import (
	"fmt"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth/codex/responses"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
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
func NewProvider(baseURL string, token TokenSource, accountID AccountIDSource, headers map[string]string, sessionStore *responses.SessionStore, operation, compactionOperation *providertransport.Operation, images *manifest.ImagePolicy, validate providertransport.OwnerValidator, runtimeScope ...string) (fantasy.Provider, error) {
	return newProvider(baseURL, token, accountID, headers, sessionStore, operation, compactionOperation, images, validate, nil, runtimeScope...)
}

// NewProviderWithIdentity uses the owning runtime's resolved literal identity.
func NewProviderWithIdentity(baseURL string, token TokenSource, accountID AccountIDSource, headers map[string]string, sessionStore *responses.SessionStore, operation, compactionOperation *providertransport.Operation, images *manifest.ImagePolicy, validate providertransport.OwnerValidator, identity useragent.NativeIdentity, runtimeScope ...string) (fantasy.Provider, error) {
	if err := identity.ValidateCodex(); err != nil {
		return nil, err
	}
	return newProvider(baseURL, token, accountID, headers, sessionStore, operation, compactionOperation, images, validate, &identity, runtimeScope...)
}

func newProvider(baseURL string, token TokenSource, accountID AccountIDSource, headers map[string]string, sessionStore *responses.SessionStore, operation, compactionOperation *providertransport.Operation, images *manifest.ImagePolicy, validate providertransport.OwnerValidator, identity *useragent.NativeIdentity, runtimeScope ...string) (fantasy.Provider, error) {
	if validate == nil {
		return nil, fmt.Errorf("Codex provider owner validator is unavailable")
	}
	if baseURL == "" {
		return nil, fmt.Errorf("unsupported provider: manifest inference endpoint is required")
	}
	if images == nil || images.HistoryBudget == nil {
		return nil, fmt.Errorf("Codex image history budget is unavailable")
	}
	if identity == nil {
		identity = &useragent.NativeIdentity{UserAgent: useragent.Codex(), Originator: useragent.CodexOriginator(), Version: useragent.CodexVersion()}
	}
	opts := []responses.Option{
		responses.WithURL(baseURL),
		responses.WithName(ID),
		responses.WithTokenSource(token),
		responses.WithAccountIDSource(accountID),
		responses.WithUserAgent(identity.UserAgent),
		responses.WithOriginator(identity.Originator),
		responses.WithVersion(identity.Version),
		responses.WithSessionStore(sessionStore),
		responses.WithOwnerValidator(validate),
		responses.WithImagePolicy(images),
	}
	if len(runtimeScope) > 1 {
		return nil, fmt.Errorf("Codex provider accepts one runtime scope")
	}
	if len(runtimeScope) == 1 {
		opts = append(opts, responses.WithRuntimeScope(runtimeScope[0]))
	}
	if operation != nil {
		opts = append(opts,
			responses.WithTimeouts(operation.ConnectTimeout, operation.RequestTimeout, operation.StreamIdleTimeout),
			responses.WithRetryPolicy(operation.Retry),
			responses.WithErrorMappings(operation.Errors),
		)
		if operation.Streaming != nil {
			opts = append(opts, responses.WithMaxEventBytes(operation.Streaming.MaxEventBytes))
		}
	}
	if compactionOperation != nil {
		maxEventBytes := int64(0)
		if compactionOperation.Streaming != nil {
			maxEventBytes = compactionOperation.Streaming.MaxEventBytes
		}
		opts = append(opts, responses.WithCompactionPolicy(
			compactionOperation.ConnectTimeout,
			compactionOperation.RequestTimeout,
			compactionOperation.StreamIdleTimeout,
			maxEventBytes,
			compactionOperation.Retry,
			compactionOperation.Errors,
		))
	}
	if len(headers) > 0 {
		opts = append(opts, responses.WithHeaders(headers))
	}
	return responses.New(opts...)
}
