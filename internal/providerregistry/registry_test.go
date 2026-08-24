package providerregistry

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestRegistryResolvesAliasesAndReturnsClones(t *testing.T) {
	registry, err := New(Registration{
		ProviderID:   "example",
		Name:         "Example",
		Aliases:      []string{"EX"},
		Construction: ConstructionOpenAIResponses,
	})
	require.NoError(t, err)

	registration, ok := registry.Lookup("ex")
	require.True(t, ok)
	require.Equal(t, "example", registration.ProviderID)
	registration.Aliases[0] = "changed"

	registration, ok = registry.Lookup("example")
	require.True(t, ok)
	require.Equal(t, []string{"EX"}, registration.Aliases)
}

func TestRegistryRejectsDuplicateProviderAndAliasClaims(t *testing.T) {
	_, err := New(
		Registration{ProviderID: "one", Aliases: []string{"shared"}},
		Registration{ProviderID: "two", Aliases: []string{"shared"}},
	)
	require.ErrorContains(t, err, `provider alias "shared"`)

	_, err = New(Registration{ProviderID: "one"}, Registration{ProviderID: "one"})
	require.ErrorContains(t, err, `provider "one" is registered more than once`)
}

func TestRegistryRejectsDuplicateAccountNamespacesWithoutPublishingMappings(t *testing.T) {
	const providerID = "registry-duplicate-account-test"
	_, err := New(
		Registration{ProviderID: providerID, AccountNamespace: "registry-shared-account-test"},
		Registration{ProviderID: "registry-duplicate-account-other", AccountNamespace: "registry-shared-account-test"},
	)
	require.ErrorContains(t, err, `account namespace "registry-shared-account-test"`)
	require.Empty(t, accounts.StoreKey(providerID))
}

func TestRegistryRegistersAccountAliases(t *testing.T) {
	registry, err := New(Registration{
		ProviderID:       "registry-account-alias-test",
		AccountNamespace: "registry-account-current",
		AccountAliases:   []string{"registry-account-legacy"},
	})
	require.NoError(t, err)
	require.Equal(t, "registry-account-alias-test", accounts.ProviderID("registry-account-current"))
	require.Equal(t, "registry-account-alias-test", accounts.ProviderID("registry-account-legacy"))
	require.True(t, registry.HasAccountNamespace("registry-account-current"))
	require.True(t, registry.HasAccountNamespace("registry-account-legacy"))
	require.False(t, registry.HasAccountNamespace("registry-account-inactive"))
	require.False(t, (*Registry)(nil).HasAccountNamespace("registry-account-current"))
}

func TestIntegratedRegistrationsDeclareBehavioralCapabilities(t *testing.T) {
	registry, err := New(Integrated()...)
	require.NoError(t, err)

	codex, ok := registry.Lookup("codex")
	require.True(t, ok)
	require.Equal(t, int64(14*1024*1024), codex.Images.HistoryBudget.RequestBytes)
	require.Equal(t, []int64{512 * 1024, 256 * 1024, 64 * 1024}, codex.Images.HistoryBudget.PerImageTargets)
	require.NotEmpty(t, codex.RuntimeControls)
	require.NotNil(t, codex.Runtime)
	require.NotNil(t, codex.Reasoning)

	gemini, ok := registry.Lookup("gemini-ag")
	require.True(t, ok)
	require.NotNil(t, gemini.Quota)
	require.NotNil(t, gemini.Images)
	require.NotNil(t, gemini.Reasoning)

	copilotRegistration, ok := registry.Lookup("copilot")
	require.True(t, ok)
	require.Equal(t, ConstructionCopilot, copilotRegistration.Construction)
	require.NotNil(t, copilotRegistration.OAuth)
}

func TestFromManifestCompilesDeclarativeOAuthBehavior(t *testing.T) {
	registration, err := FromManifest(manifest.Manifest{
		Provider: manifest.Provider{
			ID: "manifest-provider", Name: "Manifest Provider", AccountNamespace: "manifest.current",
			LegacyAccountAliases: []string{"manifest.legacy"},
		},
		Capabilities: manifest.Capabilities{
			OAuth: []manifest.OAuthFlow{{
				ID: "login", AuthorizationEndpoint: "authorize", TokenEndpoint: "token",
				Redirect: manifest.OAuthRedirect{Mode: "hosted-paste"},
			}},
			Endpoints: []manifest.Endpoint{
				{ID: "authorize", BaseURL: "https://example.invalid/oauth/authorize", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"},
				{ID: "token", BaseURL: "https://example.invalid/oauth/token", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"},
				{ID: "api", BaseURL: "https://example.invalid"},
			},
			Operations: []manifest.Operation{{
				ID: "inference", Kind: "inference", Protocol: string(ConstructionOpenAIResponses),
				Transport: "sse", Endpoint: "api", Method: http.MethodPost, Path: "/v1/responses",
				Retry: &manifest.RetryPolicy{MaxAttempts: 2, Authentication: "refresh-once", ReplayRequirement: "before-first-event"},
			}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, "manifest.current", registration.AccountNamespace)
	require.Equal(t, []string{"manifest.legacy"}, registration.AccountAliases)
	require.NotNil(t, registration.OAuth)
	require.Equal(t, LoginHostedPaste, registration.OAuth.Adapter)
	require.NotNil(t, registration.OAuth.Authorize)
	require.NotNil(t, registration.OAuth.Refresh)
	require.NotNil(t, registration.Operation)
	require.Equal(t, "sse", registration.Operation.Key.Transport)
	require.Equal(t, 2, registration.Operation.Retry.MaxAttempts)
}

func TestFromManifestProjectsDeclarativePoliciesAndResolvedInstructions(t *testing.T) {
	value := manifest.Manifest{
		Provider: manifest.Provider{ID: "manifest-policies", Name: "Manifest Policies"},
		Capabilities: manifest.Capabilities{
			Endpoints:       []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations:      []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: string(ConstructionOpenAIResponses), Transport: "sse", Endpoint: "api", Path: "/v1/responses"}},
			Usage:           &manifest.UsagePolicy{Source: "stream", Fallback: "estimate", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input"}}},
			Images:          &manifest.ImagePolicy{AcceptedMediaTypes: []string{"image/png"}, MaxSourceBytes: 1024},
			Instructions:    &manifest.InstructionPolicy{Default: "native", Profiles: map[string]string{"native": "instructions/native.txt"}},
			RuntimeControls: []manifest.RuntimeControl{{ID: "effort", Type: "enum", Values: []string{"low", "high"}, Default: "low", Scope: "model", RequestPath: "/reasoning/effort"}},
			Errors:          []manifest.ErrorMapping{{Class: "authentication", Statuses: []int{401}}},
		},
	}
	registration, err := FromManifest(value, map[string]string{"instructions/native.txt": "Native instructions"})
	require.NoError(t, err)
	require.Equal(t, "stream", registration.Usage.Source)
	require.Equal(t, int64(1024), registration.Images.MaxSourceBytes)
	require.Equal(t, "native", registration.Instructions.Default)
	require.Equal(t, "Native instructions", registration.Instructions.Profiles["native"])
	require.Equal(t, "effort", registration.RuntimeControls[0].ID)
	require.Equal(t, []int{401}, registration.Errors[0].Statuses)
	require.Nil(t, registration.Quota)
	require.Nil(t, registration.Runtime)
	require.Nil(t, registration.Reasoning)

	registration.Instructions.Profiles["native"] = "mutated"
	registry, err := New(registration)
	require.NoError(t, err)
	first, ok := registry.Lookup("manifest-policies")
	require.True(t, ok)
	first.Images.AcceptedMediaTypes[0] = "mutated"
	first.Instructions.Profiles["native"] = "mutated again"
	first.Operation.Key.Protocol = "mutated"
	second, ok := registry.Lookup("manifest-policies")
	require.True(t, ok)
	require.Equal(t, "image/png", second.Images.AcceptedMediaTypes[0])
	require.Equal(t, "mutated", second.Instructions.Profiles["native"])
	require.Equal(t, string(ConstructionOpenAIResponses), second.Operation.Key.Protocol)
}

func TestSelectOwnersDefaultsToIntegratedAndRejectsInvalidCompat(t *testing.T) {
	integrated := Registration{ProviderID: "target", Construction: "integrated-target"}
	plugin := Registration{ProviderID: "target", Construction: "integrated-target", CompatibilityAdapter: "integrated-other"}

	selected, err := SelectOwners([]Registration{integrated}, []Registration{plugin}, nil)
	require.NoError(t, err)
	require.Equal(t, Construction(""), selected[0].CompatibilityAdapter)
	_, err = SelectOwners([]Registration{integrated}, []Registration{plugin}, map[string]OwnerMode{"target": OwnerPluginCompat})
	require.ErrorContains(t, err, "does not match integrated owner")
}

func functionPointer(value any) uintptr {
	if value == nil {
		return 0
	}
	return reflect.ValueOf(value).Pointer()
}

func TestRegistrationMapErrorUsesFirstMatchingDeclarativeMapping(t *testing.T) {
	registration := Registration{Errors: []manifest.ErrorMapping{
		{
			Class: "authentication", Statuses: []int{http.StatusUnauthorized}, Codes: []string{"token_expired"},
			CodePointer: "/error/code", MessagePointer: "/error/message", Title: "Sign in again",
		},
		{Class: "server", Statuses: []int{http.StatusUnauthorized}, Title: "Fallback"},
	}}
	providerErr := &fantasy.ProviderError{
		StatusCode:   http.StatusUnauthorized,
		Message:      "raw message",
		ResponseBody: []byte(`{"error":{"code":"token_expired","message":"Session expired"}}`),
	}
	wrapped := fmt.Errorf("request failed: %w", providerErr)

	require.Same(t, wrapped, registration.MapError(wrapped))
	require.True(t, providerErr.AuthError)
	require.Equal(t, "Sign in again", providerErr.Title)
	require.Equal(t, "Session expired", providerErr.Message)
}

func TestRegistrationMapErrorAppliesRetryAndContextFlags(t *testing.T) {
	registration := Registration{Errors: []manifest.ErrorMapping{
		{Class: "context-overflow", Codes: []string{"context_length"}, CodePointer: "/code", Retryable: true},
	}}
	providerErr := &fantasy.ProviderError{ResponseBody: []byte(`{"code":"context_length"}`)}

	require.Same(t, providerErr, registration.MapError(providerErr))
	require.True(t, providerErr.TransientError)
	require.True(t, providerErr.IsContextTooLarge())
}

func TestRegistrationMapErrorBoundsExtractedProviderText(t *testing.T) {
	registration := Registration{Errors: []manifest.ErrorMapping{{Class: "unknown", MessagePointer: "/message"}}}
	providerErr := &fantasy.ProviderError{ResponseBody: []byte(`{"message":"` + strings.Repeat("x", maxMappedErrorTextRunes+100) + `\u0000"}`)}

	registration.MapError(providerErr)
	require.Len(t, []rune(providerErr.Message), maxMappedErrorTextRunes)
	require.NotContains(t, providerErr.Message, "\x00")
}

func TestRegistrationMapErrorFallsThroughAfterCodeMismatch(t *testing.T) {
	registration := Registration{Errors: []manifest.ErrorMapping{
		{Class: "authentication", Codes: []string{"expected"}, CodePointer: "/code", Title: "Specific"},
		{Class: "unknown", Title: "Fallback"},
	}}
	providerErr := &fantasy.ProviderError{ResponseBody: []byte(`{"code":"different"}`)}

	registration.MapError(providerErr)
	require.False(t, providerErr.AuthError)
	require.Equal(t, "Fallback", providerErr.Title)
}

func TestFromManifestRejectsMissingValidatedInstructionText(t *testing.T) {
	_, err := FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "missing-text", Name: "Missing Text"},
		Capabilities: manifest.Capabilities{
			Endpoints:    []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations:   []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: string(ConstructionOpenAIResponses), Transport: "sse", Endpoint: "api", Path: "/v1/responses"}},
			Instructions: &manifest.InstructionPolicy{Default: "native", Profiles: map[string]string{"native": "instructions/native.txt"}},
		},
	})
	require.ErrorContains(t, err, "no validated static text")
}

func TestRegistryRegistersAccountNamespaceAndRefresher(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	refresh := func(_ context.Context, refreshToken string) (*oauth.Token, error) {
		require.Equal(t, "refresh-old", refreshToken)
		return &oauth.Token{
			AccessToken:  "access-new",
			RefreshToken: "refresh-new",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	_, err := New(Registration{
		ProviderID:       "registry-account-test",
		AccountNamespace: "registry-account-namespace",
		AccountOrder:     500,
		OAuth:            &OAuthCapability{Refresh: refresh},
	})
	require.NoError(t, err)
	require.Equal(t, "registry-account-namespace", accounts.StoreKey("registry-account-test"))
	require.Equal(t, "registry-account-test", accounts.ProviderID("registry-account-namespace"))

	ctx := t.Context()
	require.NoError(t, accounts.Save(ctx, "registry-account-namespace", accounts.Entry{
		ID:           "account",
		AccessToken:  "access-old",
		RefreshToken: "refresh-old",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}))
	accessToken, err := accounts.AccessToken(ctx, "registry-account-namespace")
	require.NoError(t, err)
	require.Equal(t, "access-new", accessToken)
}
