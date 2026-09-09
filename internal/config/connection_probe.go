package config

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/discover"
	"github.com/example-git/crux/internal/providertransport"
)

type ConnectionProbeKind string

const (
	ConnectionProbeNotProbed    ConnectionProbeKind = "not-probed"
	ConnectionProbeFormatOnly   ConnectionProbeKind = "format-only"
	ConnectionProbeHTTPResponse ConnectionProbeKind = "http-response"
	ConnectionProbeUnsupported  ConnectionProbeKind = "unsupported"
)

type ConnectionProbePolicy string

const (
	ConnectionProbePolicyNone     ConnectionProbePolicy = "none"
	ConnectionProbePolicySKPrefix ConnectionProbePolicy = "sk-prefix"
	ConnectionProbePolicyHTTP200  ConnectionProbePolicy = "http-200"
	ConnectionProbePolicyNon401   ConnectionProbePolicy = "non-401"
)

// ConnectionProbeResult reports the evidence from the existing connection
// policies, not inference authorization or permission to use a selected model.
// The error remains authoritative, including when HTTPStatus records a response
// whose policy or subsequent owner check failed. It contains no credential,
// endpoint, header value or executable provider definition.
type ConnectionProbeResult struct {
	Kind       ConnectionProbeKind   `json:"kind"`
	Policy     ConnectionProbePolicy `json:"policy"`
	HTTPStatus int                   `json:"http_status"`
	// Entered key means the resolved APIKey from the supplied ProviderConfig.
	// These fields describe the initial request's construction, not delivery or
	// headers on a redirected request. Any explicit Authorization override is
	// reported conservatively even if its bytes happen to equal the entered key.
	EnteredKeyInAuthorization bool `json:"entered_key_in_authorization"`
	AuthorizationOverridden   bool `json:"authorization_overridden"`
}

// ProbeConnection uses the complete supplied provider configuration and the
// same exact-owner and HTTP policies as TestConnection. Native or manifest
// protocol probes are not replaced with generic OpenAI-compatible requests.
// Context-aware resolvers receive ctx during expansion; legacy resolvers are
// checked for cancellation before and after their synchronous call.
func (c *ProviderConfig) ProbeConnection(ctx context.Context, resolver VariableResolver, validate providertransport.OwnerValidator) (ConnectionProbeResult, error) {
	result := ConnectionProbeResult{Kind: ConnectionProbeNotProbed, Policy: ConnectionProbePolicyNone}
	if validate == nil {
		return result, fmt.Errorf("provider owner validator is unavailable")
	}
	ctx = providertransport.ContextWithOwnerValidator(ctx, validate)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return result, err
	}
	if err := validateConfiguredProviderOwner(c.ID, *c); err != nil {
		return result, err
	}
	if resolver == nil {
		return result, fmt.Errorf("provider connection resolver is unavailable")
	}
	resolve := func(value string) (string, error) {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		var resolved string
		var err error
		if contextual, ok := resolver.(contextVariableResolver); ok {
			resolved, err = contextual.ResolveValueContext(ctx, value)
		} else {
			resolved, err = resolver.ResolveValue(value)
		}
		if err == nil {
			err = ctx.Err()
		}
		return resolved, err
	}
	providerID := catalog.ProviderID(c.ID)
	apiKey, err := ResolveProviderAPIKey(*c, resolve)
	if err != nil {
		return result, fmt.Errorf("resolve provider %s credential: %w", c.ID, err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := providertransport.ValidateContextOwner(ctx); err != nil {
		return result, err
	}
	exactPreset := c.Owner.Type == ProviderOwnerPreset
	if exactPreset && (c.Preset.ID == "" || c.Preset.Version == "" || c.Preset.Digest == "") {
		return result, fmt.Errorf("provider preset for provider %s has an incomplete owner reference", c.ID)
	}
	switch {
	case exactPreset && (providerID == catalog.ProviderMiniMax || providerID == catalog.ProviderMiniMaxChina):
		return result, nil
	case exactPreset && providerID == catalog.ProviderAlibabaSingapore:
		result.Kind, result.Policy = ConnectionProbeFormatOnly, ConnectionProbePolicySKPrefix
		if !strings.HasPrefix(apiKey, "sk-") {
			return result, fmt.Errorf("invalid API key format for provider %s", c.ID)
		}
		return result, nil
	}
	providerType := cmp.Or(c.Type, catalog.TypeOpenAICompat)
	if providerType != catalog.TypeOpenAICompat && !discover.IsKnownCustomProvider(string(providerType)) {
		result.Kind = ConnectionProbeUnsupported
		return result, fmt.Errorf("unsupported provider type %q", providerType)
	}
	result.Policy = ConnectionProbePolicyHTTP200
	if exactPreset && providerID == catalog.ProviderZAI {
		result.Policy = ConnectionProbePolicyNon401
	}
	baseURL, err := resolve(c.BaseURL)
	if err != nil {
		return result, fmt.Errorf("resolve provider %s base URL: %w", c.ID, err)
	}
	if baseURL == "" {
		return result, fmt.Errorf("provider %s is missing an API endpoint", c.ID)
	}
	testURL := strings.TrimRight(baseURL, "/") + "/models"
	if exactPreset && providerID == catalog.ProviderOpenCodeGo {
		testURL = strings.TrimRight(strings.Replace(baseURL, "/go", "", 1), "/") + "/models"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return result, fmt.Errorf("failed to create request for provider %s: %w", c.ID, err)
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		result.EnteredKeyInAuthorization = true
	}
	for key, value := range c.ExtraHeaders {
		req.Header.Set(key, value)
		if strings.EqualFold(key, "Authorization") {
			result.AuthorizationOverridden = true
			result.EnteredKeyInAuthorization = false
		}
	}
	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, http.DefaultClient).Do(req)
	if resp != nil {
		result.Kind, result.HTTPStatus = ConnectionProbeHTTPResponse, resp.StatusCode
	}
	if ownerErr := providertransport.ValidateContextOwner(ctx); ownerErr != nil {
		if resp != nil {
			resp.Body.Close()
		}
		return result, ownerErr
	}
	if err != nil {
		return result, fmt.Errorf("failed to connect to provider %s: %w", c.ID, err)
	}
	defer resp.Body.Close()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if result.Policy == ConnectionProbePolicyNon401 {
		if resp.StatusCode == http.StatusUnauthorized {
			return result, fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
		}
		return result, nil
	}
	if resp.StatusCode != http.StatusOK {
		return result, fmt.Errorf("failed to connect to provider %s: %s", c.ID, resp.Status)
	}
	return result, nil
}
