package providerregistry

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestOAuthChallengeRegistrationCloneAndCompatibility(t *testing.T) {
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_OAUTH_CLIENT_ID=captured-codex", "GEMINI_OAUTH_CLIENT_ID=captured-gemini", "GEMINI_OAUTH_CLIENT_SECRET=captured-secret"})
	for _, registration := range Integrated() {
		if registration.Construction != ConstructionCodex && registration.Construction != ConstructionGeminiAntigravity {
			continue
		}
		t.Run(registration.ProviderID, func(t *testing.T) {
			require.NotNil(t, registration.OAuth.Callback)
			require.NotNil(t, registration.OAuth.PrepareCode)
			original := *registration.OAuth.Callback
			clone := registration.Clone()
			clone.OAuth.Callback.Mode = "changed"
			require.Equal(t, original, *registration.OAuth.Callback)
			delegated := Registration{ProviderID: registration.ProviderID, AccountNamespace: registration.AccountNamespace, OAuth: &OAuthCapability{Adapter: LoginDeviceCode, Callback: &oauth.CallbackRequirement{Mode: "wrong"}, FlowID: "declared-flow"}}
			require.NoError(t, attachCompatibilityAdapter(&delegated, manifest.CompatibilityAdapter{ID: string(registration.Construction), Delegates: []string{"oauth"}}))
			require.Equal(t, registration.OAuth.Adapter, delegated.OAuth.Adapter)
			require.Equal(t, original, *delegated.OAuth.Callback)
			require.Equal(t, "declared-flow", delegated.OAuth.FlowID)
			bound, err := BindRegistrationConfiguration(delegated, map[string]any{"irrelevant": "value"})
			require.NoError(t, err)
			challenge, err := bound.OAuth.PrepareCode(ctx, original.Port)
			require.NoError(t, err)
			defer challenge.Close()
			parsed, err := url.Parse(challenge.AuthorizationURL())
			require.NoError(t, err)
			if registration.Construction == ConstructionCodex {
				require.Equal(t, "captured-codex", parsed.Query().Get("client_id"))
				require.Equal(t, "http://localhost:1455/auth/callback", parsed.Query().Get("redirect_uri"))
			} else {
				require.Equal(t, "captured-gemini", parsed.Query().Get("client_id"))
				require.Equal(t, "hosted-paste", bound.OAuth.Callback.Mode)
			}
			bound.OAuth.Callback.Path = "changed"
			require.Equal(t, original, *delegated.OAuth.Callback)
		})
	}
}

func TestManifestBoundChallengeFactoryKeepsExactDescriptorAndConfiguration(t *testing.T) {
	value := manifest.Manifest{Provider: manifest.Provider{ID: "challenge-provider", Name: "Challenge Provider"}, Capabilities: manifest.Capabilities{
		OAuth:      []manifest.OAuthFlow{{ID: "login", AuthorizationEndpoint: "authorize", TokenEndpoint: "token", ClientID: manifest.Template{Kind: "config", Ref: "client_id"}, PKCE: "s256", TimeoutSeconds: 60, Redirect: manifest.OAuthRedirect{Mode: "loopback-dynamic", CallbackPath: "/callback%2Fpart", StateRequired: true}}},
		Endpoints:  []manifest.Endpoint{{ID: "authorize", BaseURL: "https://example.invalid/authorize", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}}, {ID: "token", BaseURL: "https://example.invalid/token", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}}, {ID: "api", BaseURL: "https://example.invalid"}},
		Operations: []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: string(ConstructionOpenAIResponses), Transport: "sse", Endpoint: "api", Method: http.MethodPost, Path: "/v1/responses"}},
	}}
	registration, err := FromManifest(value)
	require.NoError(t, err)
	settings := map[string]any{"client_id": "captured-client"}
	bound, err := BindRegistrationConfiguration(registration, settings)
	require.NoError(t, err)
	settings["client_id"] = "changed"
	require.Equal(t, oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback%2Fpart"}, *bound.OAuth.Callback)
	clone := bound.Clone()
	clone.OAuth.Callback.Path = "changed"
	require.Equal(t, "/callback%2Fpart", bound.OAuth.Callback.Path)
	_, err = bound.OAuth.PrepareCode(t.Context(), 0)
	require.Error(t, err)
	challenge, err := bound.OAuth.PrepareCode(t.Context(), 4321)
	require.NoError(t, err)
	defer challenge.Close()
	parsed, err := url.Parse(challenge.AuthorizationURL())
	require.NoError(t, err)
	require.Equal(t, "captured-client", parsed.Query().Get("client_id"))
	require.Equal(t, "http://localhost:4321/callback%2Fpart", parsed.Query().Get("redirect_uri"))
	require.False(t, challenge.ExpiresAt().IsZero())
}
