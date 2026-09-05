package providerregistry

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	declarativetransport "github.com/example-git/crux/internal/providertransport/declarative"
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

func TestCandidateRegistryDoesNotReplacePublishedAccountGeneration(t *testing.T) {
	oldRefresh := func(context.Context, string) (*oauth.Token, error) { return &oauth.Token{AccessToken: "old"}, nil }
	oldRegistry, err := New(Registration{ProviderID: "published", AccountNamespace: "published-accounts", OAuth: &OAuthCapability{Refresh: oldRefresh}})
	require.NoError(t, err)
	accounts.PublishProviders(oldRegistry.AccountRegistrations())
	t.Cleanup(func() { accounts.PublishProviders(nil) })

	_, err = New(Registration{ProviderID: "candidate", AccountNamespace: "candidate-accounts"})
	require.NoError(t, err)
	_, err = New(
		Registration{ProviderID: "broken-one", AccountNamespace: "broken-accounts"},
		Registration{ProviderID: "broken-two", AccountNamespace: "broken-accounts"},
	)
	require.Error(t, err)

	namespace, refresher, ok := accounts.ProviderSnapshot("published")
	require.True(t, ok)
	require.Equal(t, "published-accounts", namespace)
	token, err := refresher(t.Context(), "refresh")
	require.NoError(t, err)
	require.Equal(t, "old", token.AccessToken)
	require.Empty(t, accounts.StoreKey("candidate"))
	require.Empty(t, accounts.StoreKey("broken-one"))
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

func TestRegistryCapturesAccountAliasesWithoutPublishing(t *testing.T) {
	before := accounts.ProviderID("registry-account-current")
	registry, err := New(Registration{
		ProviderID:       "registry-account-alias-test",
		AccountNamespace: "registry-account-current",
		AccountAliases:   []string{"registry-account-legacy"},
	})
	require.NoError(t, err)
	require.Equal(t, before, accounts.ProviderID("registry-account-current"))
	require.Equal(t, []accounts.ProviderRegistration{{
		ProviderID: "registry-account-alias-test",
		Namespace:  "registry-account-current",
		Aliases:    []string{"registry-account-legacy"},
	}}, registry.AccountRegistrations())
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
	require.Nil(t, gemini.Quota)
	require.NotNil(t, gemini.Images)
	require.NotNil(t, gemini.Reasoning)

	copilotRegistration, ok := registry.Lookup("copilot")
	require.True(t, ok)
	require.Equal(t, ConstructionCopilot, copilotRegistration.Construction)
	require.NotNil(t, copilotRegistration.OAuth)
	require.NotNil(t, copilotRegistration.Quota)
	require.Equal(t, QuotaCredentialRefreshToken, copilotRegistration.QuotaCredential)
}

func TestRegistryRestrictsQuotaCredentialSelection(t *testing.T) {
	fetcher := func(context.Context, string) (*oauthusage.Usage, error) {
		return &oauthusage.Usage{}, nil
	}
	base := Registration{
		ProviderID:      "copilot",
		Construction:    ConstructionCopilot,
		OAuth:           &OAuthCapability{},
		Quota:           fetcher,
		QuotaCredential: QuotaCredentialRefreshToken,
	}
	for _, test := range []struct {
		name         string
		registration Registration
		wantError    string
	}{
		{name: "core Copilot", registration: base},
		{name: "manifest plugin", registration: func() Registration {
			value := base
			value.Manifest = &manifest.Manifest{ID: "copilot", Version: "1.0.0"}
			return value
		}(), wantError: "restricted to core Copilot"},
		{name: "non-Copilot core", registration: func() Registration {
			value := base
			value.ProviderID = "codex"
			value.Construction = ConstructionCodex
			return value
		}(), wantError: "restricted to core Copilot"},
		{name: "unsupported credential", registration: func() Registration {
			value := base
			value.QuotaCredential = QuotaCredential("provider-secret")
			return value
		}(), wantError: "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := New(test.registration)
			if test.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, test.wantError)
		})
	}
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

func TestBindRegistrationConfigurationResolvesOAuthTemplates(t *testing.T) {
	value := manifest.Manifest{
		Provider: manifest.Provider{ID: "manifest-provider", Name: "Manifest Provider"},
		Capabilities: manifest.Capabilities{
			OAuth: []manifest.OAuthFlow{{
				ID: "login", Credential: "account", AuthorizationEndpoint: "authorize", TokenEndpoint: "token",
				ClientID: manifest.Template{Kind: "config", Ref: "oauth_client_id"}, Scopes: []string{"openid"}, PKCE: "disabled",
				Redirect: manifest.OAuthRedirect{Mode: "hosted-paste", URI: "https://example.invalid/callback"},
			}},
			Endpoints: []manifest.Endpoint{
				{ID: "authorize", BaseURL: "https://example.invalid/oauth/authorize", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"},
				{ID: "token", BaseURL: "https://example.invalid/oauth/token", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"},
				{ID: "api", BaseURL: "https://example.invalid", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"},
			},
			Operations: []manifest.Operation{{
				ID: "inference", Kind: "inference", Protocol: string(ConstructionOpenAIResponses),
				Transport: "sse", Endpoint: "api", Method: http.MethodPost, Path: "/v1/responses",
			}},
		},
	}
	registration, err := FromManifest(value)
	require.NoError(t, err)
	bound, err := BindRegistrationConfiguration(registration, map[string]any{"oauth_client_id": "configured-client"})
	require.NoError(t, err)

	stop := errors.New("stop before token exchange")
	_, err = bound.OAuth.Authorize(t.Context(), func(raw string) error {
		parsed, parseErr := url.Parse(raw)
		require.NoError(t, parseErr)
		require.Equal(t, "configured-client", parsed.Query().Get("client_id"))
		return stop
	}, func() (string, error) { return "", errors.New("unexpected callback read") })
	require.ErrorIs(t, err, stop)
}

func TestFromManifestProjectsDeclarativePoliciesAndResolvedInstructions(t *testing.T) {
	value := manifest.Manifest{
		Provider: manifest.Provider{ID: "manifest-policies", Name: "Manifest Policies"},
		Capabilities: manifest.Capabilities{
			Endpoints:       []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations:      []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: string(ConstructionGenericJSON), Transport: "sse", Endpoint: "api", Path: "/v1/responses", Streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}, MaxEventBytes: 1024}}},
			Usage:           &manifest.UsagePolicy{Source: "stream", Fallback: "zero", Mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input"}}},
			Images:          &manifest.ImagePolicy{AcceptedMediaTypes: []string{"image/png"}, MaxSourceBytes: 1024},
			Instructions:    &manifest.InstructionPolicy{Default: "native", SelectionDefault: "native", Profiles: map[string]string{"native": "instructions/native.txt"}, HiddenSkills: []string{"imagegen"}},
			RuntimeControls: []manifest.RuntimeControl{{ID: "effort", Type: "enum", Values: []string{"low", "high"}, Default: "low", Scope: "model", RequestPath: "/reasoning/effort"}},
			Errors:          []manifest.ErrorMapping{{Class: "authentication", Statuses: []int{401}}},
		},
	}
	registration, err := FromManifest(value, map[string]string{"instructions/native.txt": "Native instructions"})
	require.NoError(t, err)
	require.Equal(t, "stream", registration.Usage.Source)
	require.Equal(t, int64(1024), registration.Images.MaxSourceBytes)
	require.Equal(t, "native", registration.Instructions.Default)
	require.Equal(t, "native", registration.Instructions.SelectionDefault)
	require.Equal(t, "Native instructions", registration.Instructions.Profiles["native"])
	require.Equal(t, []string{"imagegen"}, registration.Instructions.HiddenSkills)
	require.Equal(t, "effort", registration.RuntimeControls[0].ID)
	require.Equal(t, []int{401}, registration.Errors[0].Statuses)
	require.Nil(t, registration.Quota)
	require.NotNil(t, registration.Runtime)
	require.True(t, registration.Runtime.Available("model"))
	runtimeOptions := registration.Runtime.Apply(RuntimeValues{AnalysisEffort: "high"}, nil)
	runtime, ok := runtimeOptions["manifest-policies"].(*declarativetransport.Options)
	require.True(t, ok)
	require.Equal(t, map[string]any{"/reasoning/effort": "high"}, runtime.Controls)
	require.NotNil(t, registration.Reasoning)
	reasoningOptions, err := registration.Reasoning.Options("model", "high", true, nil)
	require.NoError(t, err)
	reasoning, ok := reasoningOptions["manifest-policies"].(*declarativetransport.Options)
	require.True(t, ok)
	require.Equal(t, map[string]any{"/reasoning/effort": "high"}, reasoning.Controls)

	registration.Instructions.Profiles["native"] = "mutated"
	registry, err := New(registration)
	require.NoError(t, err)
	first, ok := registry.Lookup("manifest-policies")
	require.True(t, ok)
	first.Images.AcceptedMediaTypes[0] = "mutated"
	first.Instructions.Profiles["native"] = "mutated again"
	first.Instructions.HiddenSkills[0] = "mutated"
	first.Operation.Key.Protocol = "mutated"
	second, ok := registry.Lookup("manifest-policies")
	require.True(t, ok)
	require.Equal(t, "image/png", second.Images.AcceptedMediaTypes[0])
	require.Equal(t, "mutated", second.Instructions.Profiles["native"])
	require.Equal(t, []string{"imagegen"}, second.Instructions.HiddenSkills)
	require.Equal(t, string(ConstructionGenericJSON), second.Operation.Key.Protocol)
}

func TestFromManifestExecutesManifestUsagePipeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer exact-token" {
			response.WriteHeader(http.StatusUnauthorized)
			return
		}
		var document map[string]any
		if err := json.NewDecoder(request.Body).Decode(&document); err != nil {
			response.WriteHeader(http.StatusBadRequest)
			return
		}
		switch request.URL.Path {
		case "/usage/setup":
			require.Equal(t, "synthetic", providertransport.JSONPointer(document, "/metadata/client"))
			_, _ = response.Write([]byte(`{"context":{"project":"project-one"},"plans":{"preferred":"Premium","fallback":"Free"}}`))
		case "/usage/summary":
			require.Equal(t, "project-one", providertransport.JSONPointer(document, "/project"))
			_, _ = response.Write([]byte(`{"windows":[{"remaining":0.4,"reset":"2026-09-07T00:00:00Z"}]}`))
		default:
			response.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	endpoint := manifest.Endpoint{ID: "api", BaseURL: server.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}}
	registration, err := FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "manifest-usage", Name: "Manifest Usage"},
		Capabilities: manifest.Capabilities{
			Endpoints: []manifest.Endpoint{endpoint},
			JSONTransforms: map[string]manifest.JSONPipeline{
				"usage-setup": {MaxOperations: 1, Operations: []manifest.JSONOperation{{
					Operation: "set", Path: "/metadata/client", Value: &manifest.Template{Kind: "literal", Value: "synthetic"},
				}}},
				"usage-summary": {MaxOperations: 1, Operations: []manifest.JSONOperation{{
					Operation: "set", Path: "/project", Value: &manifest.Template{Kind: "context", Ref: "usage.project"},
				}}},
			},
			Operations: []manifest.Operation{
				{ID: "inference", Kind: "inference", Protocol: string(ConstructionGenericJSON), Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/inference"},
				{ID: "usage-setup", Kind: "account", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/usage/setup", RequestTransform: "usage-setup"},
				{ID: "usage-summary", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/usage/summary", RequestTransform: "usage-summary"},
			},
			Usage: &manifest.UsagePolicy{
				Setup: []manifest.UsageSetup{{
					Operation:    "usage-setup",
					Extract:      []manifest.UsageContextExtraction{{Context: "usage.project", Pointer: "/context/project"}},
					PlanPointers: []string{"/plans/preferred", "/plans/fallback"},
				}},
				Operation: "usage-summary", Source: "operation", Fallback: "unavailable",
				Windows: []manifest.WindowMap{{ID: "weekly", RemainingFractionPointer: "/windows/0/remaining", ResetPointer: "/windows/0/reset", ResetFormat: "rfc3339"}},
			},
		},
	})
	require.NoError(t, err)
	require.NotNil(t, registration.Quota)
	result, err := registration.Quota(t.Context(), "exact-token")
	require.NoError(t, err)
	require.Equal(t, "Premium", result.Plan)
	require.Equal(t, []oauthusage.Window{{Name: "weekly", Percent: 60, ResetsAt: time.Date(2026, time.September, 7, 0, 0, 0, 0, time.UTC)}}, result.Windows)
}

func TestFromManifestResolvesAnthropicInstructionBlock(t *testing.T) {
	value := manifest.Manifest{
		Provider: manifest.Provider{ID: "manifest-anthropic", Name: "Manifest Anthropic"},
		Capabilities: manifest.Capabilities{
			Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations: []manifest.Operation{{
				ID: "inference", Kind: "inference", Protocol: string(ConstructionAnthropicMessages),
				Transport: "sse", Endpoint: "api", Path: "/v1/messages",
			}},
			Instructions: &manifest.InstructionPolicy{
				Default: "stock", SelectionDefault: "native",
				Profiles: map[string]string{"stock": "instructions/stock.txt"},
			},
			Anthropic: &manifest.AnthropicPolicy{
				InstructionBlock: &manifest.AnthropicInstructionBlock{
					Profile: "stock",
					CacheControl: &manifest.AnthropicCacheControl{
						Type: "ephemeral", TTL: "1h", Scope: "global",
					},
				},
			},
		},
	}
	registration, err := FromManifest(value, map[string]string{"instructions/stock.txt": "Exact stock instructions"})
	require.NoError(t, err)
	require.Equal(t, "native", registration.Instructions.SelectionDefault)
	require.Equal(t, "Exact stock instructions", registration.Operation.SystemInstruction.Text)
	require.Equal(t, "global", registration.Operation.SystemInstruction.CacheControl.Scope)

	registry, err := New(registration)
	require.NoError(t, err)
	resolved, ok := registry.Lookup("manifest-anthropic")
	require.True(t, ok)
	require.Equal(t, "Exact stock instructions", resolved.Operation.SystemInstruction.Text)
	registration.Operation.SystemInstruction.Text = "mutated"
	require.Equal(t, "Exact stock instructions", resolved.Operation.SystemInstruction.Text)
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

func TestRegistryRejectsUnsupportedNativeOperationPoliciesAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionOpenAIResponses, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "sse"},
			Retry: manifest.RetryPolicy{MaxAttempts: 2, Statuses: []int{http.StatusServiceUnavailable}, Authentication: "never", ReplayRequirement: "before-first-event"},
		},
	}
	tests := []struct {
		name string
		edit func(*Registration)
		want string
	}{
		{name: "websocket", edit: func(value *Registration) { value.Operation.Key.Transport = "websocket-json" }, want: "unsupported openai-responses transport"},
		{name: "continuation response ID pointer", edit: func(value *Registration) {
			value.Operation.Continuation = &manifest.ContinuationPolicy{Mode: "previous-response", ResponseIDPointer: "/response/id", RequestField: "previous_response_id", AppendOnlyHistory: true, Store: "required", Fallback: "full-replay"}
		}, want: "native OpenAI Responses requires /id"},
		{name: "refresh once", edit: func(value *Registration) {
			value.Operation.Retry.Authentication = "refresh-once"
		}, want: "refresh-once authentication is unavailable for the complete native OpenAI Responses language-model contract"},
		{name: "mixed HTTP and EOF retries", edit: func(value *Registration) {
			value.Operation.Retry.UnexpectedEOF = true
		}, want: "cannot share one max-attempts budget across HTTP and unexpected-EOF retries"},
		{name: "compaction", edit: func(value *Registration) {
			value.Operation.Compaction = &manifest.CompactionPolicy{Mode: "remote-operation"}
		}, want: "not executed by its native constructor"},
		{name: "usage", edit: func(value *Registration) { value.Usage = &manifest.UsagePolicy{Source: "stream", Fallback: "zero"} }, want: "usage source \"stream\" is unavailable"},
		{name: "operation usage without executor", edit: func(value *Registration) {
			value.Usage = &manifest.UsagePolicy{Source: "operation", Fallback: "unavailable"}
		}, want: "has no compiled quota executor"},
		{name: "metadata", edit: func(value *Registration) {
			value.Metadata = []manifest.MetadataContract{{Namespace: "synthetic.meta", Version: 1, Scope: "message"}}
		}, want: "continuation metadata contract is unavailable"},
		{name: "streaming policy", edit: func(value *Registration) {
			value.Operation.Streaming = &manifest.StreamingPolicy{EventSource: "sse-data-json", Mappings: []manifest.EventMapping{{Source: "response.completed", Event: "finish"}}, RequireTerminal: true, UnknownEvent: "warn"}
		}, want: "fixed native parser owns SSE semantics"},
		{name: "join adjacent role prompt transform", edit: func(value *Registration) {
			value.Operation.PromptTransform = &manifest.PromptPipeline{Operations: []manifest.PromptOperation{{Operation: "join-adjacent-role", Role: "user"}}}
		}, want: "prompt transform \"join-adjacent-role\" is unavailable for native OpenAI Responses construction"},
		{name: "connect timeout", edit: func(value *Registration) {
			value.Operation.Timeouts = &manifest.TimeoutHints{ConnectSeconds: 30}
		}, want: "connect timeout hint is unavailable"},
		{name: "request timeout", edit: func(value *Registration) {
			value.Operation.Timeouts = &manifest.TimeoutHints{RequestSeconds: 300}
		}, want: "request timeout hint is unavailable"},
		{name: "idle timeout", edit: func(value *Registration) {
			value.Operation.Timeouts = &manifest.TimeoutHints{IdleSeconds: 60}
		}, want: "idle timeout hint is unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registration := base.Clone()
			test.edit(&registration)
			_, err := New(registration)
			require.ErrorContains(t, err, test.want)
		})
	}
	_, err := New(base)
	require.NoError(t, err)

	for _, promptOperation := range []manifest.PromptOperation{
		{Operation: "prepend", Role: "system", Text: &manifest.Template{Kind: "literal", Value: "prefix"}},
		{Operation: "append", Role: "system", Text: &manifest.Template{Kind: "literal", Value: "suffix"}},
		{Operation: "insert-after-role", Role: "user", Text: &manifest.Template{Kind: "literal", Value: "inserted"}},
		{Operation: "remove-lines-with-prefix", Role: "user", Prefix: "remove:"},
		{Operation: "drop-role", Role: "assistant"},
	} {
		t.Run("accepted prompt transform "+promptOperation.Operation, func(t *testing.T) {
			registration := base.Clone()
			registration.Operation.PromptTransform = &manifest.PromptPipeline{Operations: []manifest.PromptOperation{promptOperation}}
			_, err := New(registration)
			require.NoError(t, err)
		})
	}

	eofOnly := base.Clone()
	eofOnly.Operation.Retry.Statuses = nil
	eofOnly.Operation.Retry.UnexpectedEOF = true
	_, err = New(eofOnly)
	require.NoError(t, err)

	lifecycle := base.Clone()
	lifecycle.Operation.Retry.Authentication = "never"
	lifecycle.Operation.Retry.UnexpectedEOF = false
	lifecycle.Operation.Continuation = &manifest.ContinuationPolicy{
		Mode:                 "previous-response",
		ResponseIDPointer:    "/id",
		RequestField:         "previous_response_id",
		RequiredStableFields: []string{"model", "instructions", "tools"},
		AppendOnlyHistory:    true,
		Store:                "required",
		Fallback:             "full-replay",
	}
	lifecycle.Operation.Continuation.MetadataNamespace = "synthetic.responses.continuation"
	lifecycle.Metadata = []manifest.MetadataContract{{
		Namespace: "synthetic.responses.continuation", Version: 1, Scope: "continuation",
		Schema: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object",
			"properties": map[string]any{
				"response_id": map[string]any{"type": "string"},
			},
			"required": []any{"response_id"}, "additionalProperties": false,
		},
	}}
	_, err = New(lifecycle)
	require.NoError(t, err)

	wrongContinuationMetadata := lifecycle.Clone()
	wrongContinuationMetadata.Metadata[0].RequiredForReplay = true
	_, err = New(wrongContinuationMetadata)
	require.ErrorContains(t, err, "continuation metadata contract is unavailable")

	wrongContinuationNamespace := lifecycle.Clone()
	wrongContinuationNamespace.Metadata[0].Namespace = "synthetic.responses.other"
	_, err = New(wrongContinuationNamespace)
	require.ErrorContains(t, err, "continuation metadata contract is unavailable")

	wrongContinuationSchema := lifecycle.Clone()
	wrongContinuationSchema.Metadata[0].Schema["additionalProperties"] = true
	_, err = New(wrongContinuationSchema)
	require.ErrorContains(t, err, "continuation metadata contract is unavailable")

	responsesUsage := func() *manifest.UsagePolicy {
		return &manifest.UsagePolicy{
			Source: "stream", Fallback: "estimate",
			Mappings: []manifest.UsageMapping{
				{Target: "input_tokens", Pointer: "/response/usage/input_tokens", Operation: "subtract-cache-read"},
				{Target: "output_tokens", Pointer: "/response/usage/output_tokens", Operation: "replace"},
				{Target: "cache_read_tokens", Pointer: "/response/usage/input_tokens_details/cached_tokens", Operation: "replace"},
			},
		}
	}
	nativeUsage := base.Clone()
	nativeUsage.Usage = responsesUsage()
	_, err = New(nativeUsage)
	require.NoError(t, err)

	for _, test := range []struct {
		name string
		edit func(*manifest.UsagePolicy)
	}{
		{name: "inclusive input", edit: func(value *manifest.UsagePolicy) { value.Mappings[0].Operation = "replace" }},
		{name: "wrong pointer", edit: func(value *manifest.UsagePolicy) { value.Mappings[1].Pointer = "/usage/output_tokens" }},
		{name: "unsupported fallback", edit: func(value *manifest.UsagePolicy) { value.Fallback = "zero" }},
		{name: "extra mapping", edit: func(value *manifest.UsagePolicy) {
			value.Mappings = append(value.Mappings, manifest.UsageMapping{Target: "total_tokens", Pointer: "/response/usage/total_tokens", Operation: "replace"})
		}},
	} {
		t.Run("native responses usage "+test.name, func(t *testing.T) {
			registration := base.Clone()
			registration.Usage = responsesUsage()
			test.edit(registration.Usage)
			_, err := New(registration)
			require.ErrorContains(t, err, "usage source \"stream\" is unavailable")
		})
	}

	operationUsage := base.Clone()
	operationUsage.Usage = &manifest.UsagePolicy{Source: "operation", Fallback: "unavailable"}
	operationUsage.Quota = func(context.Context, string) (*oauthusage.Usage, error) { return &oauthusage.Usage{}, nil }
	_, err = New(operationUsage)
	require.NoError(t, err)

	localSummary := base.Clone()
	localSummary.Operation.Compaction = &manifest.CompactionPolicy{
		Mode:                "local-summary",
		RetainedTokenBudget: 30_000,
	}
	registry, err := New(localSummary)
	require.NoError(t, err)
	resolved, ok := registry.Lookup("synthetic")
	require.True(t, ok)
	require.Equal(t, localSummary.Operation.Compaction, resolved.Operation.Compaction)

	for _, test := range []struct {
		name string
		edit func(*manifest.CompactionPolicy)
		want string
	}{
		{name: "operation", edit: func(value *manifest.CompactionPolicy) { value.Operation = "compact" }, want: "local-summary compaction operation"},
		{name: "preserve tool pairs", edit: func(value *manifest.CompactionPolicy) { value.PreserveToolPairs = true }, want: "tool-pair preservation is not executed"},
		{name: "metadata namespace", edit: func(value *manifest.CompactionPolicy) { value.MetadataNamespace = "synthetic.compaction" }, want: "metadata namespace"},
	} {
		t.Run("local summary "+test.name, func(t *testing.T) {
			registration := localSummary.Clone()
			test.edit(registration.Operation.Compaction)
			_, err := New(registration)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestRegistryValidatesDelegatedConstructionAtActivation(t *testing.T) {
	manifestFor := func(adapter Construction, delegates ...string) *manifest.Manifest {
		return &manifest.Manifest{
			Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
			Capabilities: manifest.Capabilities{Compatibility: &manifest.CompatibilityAdapter{
				ID: string(adapter), Delegates: delegates,
			}},
		}
	}
	operationFor := func(protocol, transport string) *providertransport.Operation {
		return &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: protocol, Transport: transport},
			Method: http.MethodPost, Path: "/",
			Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
		}
	}
	codex := Registration{
		ProviderID: "synthetic", Construction: ConstructionCodex, CompatibilityAdapter: ConstructionCodex,
		Manifest:  manifestFor(ConstructionCodex, "construction"),
		Operation: operationFor(string(ConstructionOpenAIResponses), "websocket-json"),
	}
	require.NoError(t, ValidateActivation(codex))
	codexImages := codex.Clone()
	codexImages.Images = integratedCodexImagePolicy()
	codexImages.Manifest.Capabilities.Images = integratedCodexImagePolicy()
	require.NoError(t, ValidateActivation(codexImages))
	registeredImageMismatch := codexImages.Clone()
	registeredImageMismatch.Images.MaxSourceBytes--
	require.ErrorContains(t, ValidateActivation(registeredImageMismatch), "registered image policy does not match")
	noncanonicalImages := codexImages.Clone()
	noncanonicalImages.Images.MaxSourceBytes--
	noncanonicalImages.Manifest.Capabilities.Images.MaxSourceBytes--
	require.ErrorContains(t, ValidateActivation(noncanonicalImages), "image history policy is unavailable")
	codexTimeouts := codex.Clone()
	codexTimeouts.Operation.Timeouts = &manifest.TimeoutHints{IdleSeconds: 1}
	codexTimeouts.Operation.StreamIdleTimeout = time.Second
	require.NoError(t, ValidateActivation(codexTimeouts))
	gemini := Registration{
		ProviderID: "synthetic", Construction: ConstructionGeminiAntigravity, CompatibilityAdapter: ConstructionGeminiAntigravity,
		Manifest:  manifestFor(ConstructionGeminiAntigravity, "construction"),
		Operation: operationFor(string(ConstructionGeminiContent), "sse"),
	}
	require.NoError(t, ValidateActivation(gemini))
	geminiTransform := gemini.Clone()
	geminiTransform.Operation.RoleMap = &manifest.RoleMap{System: "system", Developer: "system", User: "user", Assistant: "model", Tool: "user", Unknown: "reject"}
	geminiTransform.Operation.ResponseTransform = &manifest.JSONPipeline{MaxOperations: 4, Operations: []manifest.JSONOperation{
		{Operation: "move", From: "/response/candidates", Path: "/candidates"},
		{Operation: "move", From: "/response/usageMetadata", Path: "/usageMetadata"},
		{Operation: "move", From: "/response/error", Path: "/error"},
		{Operation: "delete", Path: "/response"},
	}}
	require.NoError(t, ValidateActivation(geminiTransform))
	geminiInvalidRole := gemini.Clone()
	geminiInvalidRole.Operation.RoleMap = &manifest.RoleMap{System: "system", Developer: "system", User: "user", Assistant: "assistant", Tool: "user", Unknown: "reject"}
	require.ErrorContains(t, ValidateActivation(geminiInvalidRole), "role map")
	codexRuntime := codex.Clone()
	codexRuntime.Manifest.Capabilities.Compatibility.Delegates = append(codexRuntime.Manifest.Capabilities.Compatibility.Delegates, "runtime")
	codexRuntime.Runtime = codexRuntimeCapability()
	codexRuntime.RuntimeControls = codexRuntimeControls()
	require.NoError(t, ValidateActivation(codexRuntime))

	for _, test := range []struct {
		name string
		edit func(*Registration)
		want string
	}{
		{name: "missing operation", edit: func(value *Registration) { value.Operation = nil }, want: "has no inference operation"},
		{name: "adapter mismatch", edit: func(value *Registration) { value.CompatibilityAdapter = ConstructionGeminiAntigravity }, want: "does not match"},
		{name: "unknown adapter", edit: func(value *Registration) {
			value.Construction = Construction("integrated-synthetic")
			value.CompatibilityAdapter = value.Construction
			value.Manifest.Capabilities.Compatibility.ID = string(value.Construction)
		}, want: "unsupported"},
		{name: "wrong protocol", edit: func(value *Registration) { value.Operation.Key.Protocol = string(ConstructionGeminiContent) }, want: "protocol"},
		{name: "wrong transport", edit: func(value *Registration) { value.Operation.Key.Transport = "sse" }, want: "transport"},
		{name: "wrong method", edit: func(value *Registration) { value.Operation.Method = http.MethodGet }, want: "method"},
		{name: "wrong Codex path", edit: func(value *Registration) { value.Operation.Path = "/responses" }, want: "path"},
		{name: "client identity", edit: func(value *Registration) { value.Operation.ClientIdentity = &manifest.ResolvedClientIdentity{} }, want: "client identity"},
		{name: "request transform", edit: func(value *Registration) { value.Operation.RequestTransform = &manifest.JSONPipeline{} }, want: "request transform"},
		{name: "prompt transform", edit: func(value *Registration) { value.Operation.PromptTransform = &manifest.PromptPipeline{} }, want: "prompt transform"},
		{name: "tool codec", edit: func(value *Registration) { value.Operation.ToolCodec = &manifest.ToolCodec{} }, want: "tool codec"},
		{name: "Anthropic policy", edit: func(value *Registration) { value.Operation.Anthropic = &manifest.AnthropicPolicy{} }, want: "Anthropic policy"},
		{name: "response transform", edit: func(value *Registration) { value.Operation.ResponseTransform = &manifest.JSONPipeline{} }, want: "response transform"},
		{name: "role map", edit: func(value *Registration) { value.Operation.RoleMap = &manifest.RoleMap{} }, want: "role map"},
		{name: "streaming policy", edit: func(value *Registration) {
			value.Operation.Streaming = &manifest.StreamingPolicy{EventSource: "websocket-json", MaxEventBytes: 1024, Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}}
		}, want: "streaming policy"},
		{name: "continuation policy", edit: func(value *Registration) {
			value.Operation.Continuation = &manifest.ContinuationPolicy{Mode: "previous-response"}
		}, want: "continuation policy"},
		{name: "compaction policy", edit: func(value *Registration) {
			value.Operation.Compaction = &manifest.CompactionPolicy{Mode: "local-summary"}
		}, want: "compaction policy"},
		{name: "retry policy", edit: func(value *Registration) {
			value.Manifest.Capabilities.Operations = []manifest.Operation{{
				ID: "inference", Kind: "inference", Retry: &manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "before-first-event"},
			}}
		}, want: "retry policy"},
		{name: "metadata", edit: func(value *Registration) {
			value.Metadata = []manifest.MetadataContract{{
				Namespace: "synthetic.metadata", Version: 1, Scope: "message",
				Schema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"},
			}}
		}, want: "metadata contracts"},
		{name: "runtime controls without delegation", edit: func(value *Registration) {
			value.RuntimeControls = []manifest.RuntimeControl{{ID: "mode", Type: "string", Scope: "model", RequestPath: "/mode"}}
		}, want: "require runtime compatibility delegation"},
		{name: "runtime control mismatch", edit: func(value *Registration) {
			value.Manifest.Capabilities.Compatibility.Delegates = append(value.Manifest.Capabilities.Compatibility.Delegates, "runtime")
			value.Runtime = codexRuntimeCapability()
			value.RuntimeControls = []manifest.RuntimeControl{{ID: "mode", Type: "string", Scope: "model", RequestPath: "/mode"}}
		}, want: "do not match"},
		{name: "stream usage", edit: func(value *Registration) {
			value.Usage = &manifest.UsagePolicy{Source: "stream", Fallback: "estimate"}
		}, want: "is unavailable for compatibility construction"},
		{name: "identity delegate without executor", edit: func(value *Registration) {
			value.Manifest.Capabilities.Compatibility.Delegates = append(value.Manifest.Capabilities.Compatibility.Delegates, "identity")
		}, want: "identity has no compatibility executor"},
		{name: "delegated identity operation", edit: func(value *Registration) {
			value.Manifest.Capabilities.Compatibility.Delegates = append(value.Manifest.Capabilities.Compatibility.Delegates, "identity")
			value.Identity = func(context.Context, string) (string, string, json.RawMessage) { return "id", "display", nil }
			value.Manifest.Capabilities.Operations = []manifest.Operation{
				{ID: "inference", Kind: "inference"},
				{ID: "account", Kind: "account"},
			}
		}, want: `operation "account" at index 1 has no host executor`},
		{name: "delegated usage setup operation", edit: func(value *Registration) {
			value.Manifest.Capabilities.Compatibility.Delegates = append(value.Manifest.Capabilities.Compatibility.Delegates, "usage")
			value.Quota = func(context.Context, string) (*oauthusage.Usage, error) { return &oauthusage.Usage{}, nil }
			value.Usage = &manifest.UsagePolicy{
				Source: "operation", Operation: "usage", Setup: []manifest.UsageSetup{{Operation: "setup"}}, Fallback: "unavailable",
			}
			value.Manifest.Capabilities.Operations = []manifest.Operation{
				{ID: "inference", Kind: "inference"},
				{ID: "setup", Kind: "custom"},
				{ID: "usage", Kind: "usage"},
			}
		}, want: `operation "setup" at index 1 has no host executor`},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := codex.Clone()
			test.edit(&registration)
			err := ValidateActivation(registration)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestRegistryValidatesDelegatedRemoteCompactionOperationAtActivation(t *testing.T) {
	streaming := delegatedStreamingPolicy(ConstructionCodex)
	streaming.MaxEventBytes = 2048
	inference := &providertransport.Operation{
		ID: "inference", Kind: "inference",
		Key:    providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "websocket-json"},
		Method: http.MethodPost,
		Path:   "/",
		Retry:  *delegatedRetryPolicy(ConstructionCodex),
		Compaction: &manifest.CompactionPolicy{
			Mode: "remote-operation", Operation: "remote-compact", RetainedTokenBudget: 64000,
			PreserveToolPairs: true, MetadataNamespace: "codex",
		},
	}
	compaction := &providertransport.Operation{
		ID: "remote-compact", Kind: "compaction",
		Key:       providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "websocket-json"},
		Endpoint:  inference.Endpoint,
		Method:    http.MethodPost,
		Path:      "/",
		Streaming: streaming,
		Retry: manifest.RetryPolicy{
			MaxAttempts:       3,
			InitialDelayMS:    10,
			MaxDelayMS:        20,
			Factor:            2,
			TransportErrors:   true,
			UnexpectedEOF:     true,
			Authentication:    "refresh-once",
			ReplayRequirement: "before-first-event",
		},
	}
	registration := Registration{
		ProviderID: "synthetic", Construction: ConstructionCodex, CompatibilityAdapter: ConstructionCodex,
		Manifest: &manifest.Manifest{
			Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
			Capabilities: manifest.Capabilities{Compatibility: &manifest.CompatibilityAdapter{
				ID: string(ConstructionCodex), Delegates: []string{"construction"},
			}},
		},
		Operation:  inference,
		Operations: map[string]*providertransport.Operation{"inference": inference, "remote-compact": compaction},
		Metadata:   delegatedMetadataContracts(ConstructionCodex),
	}
	require.NoError(t, ValidateActivation(registration))
	cloned := registration.Clone()
	require.Contains(t, cloned.Operations, "remote-compact")
	require.NotSame(t, registration.Operations["remote-compact"], cloned.Operations["remote-compact"])
	cloned.Operations["remote-compact"].Streaming.MaxEventBytes = 4096
	require.Equal(t, int64(2048), registration.Operations["remote-compact"].Streaming.MaxEventBytes)

	for _, test := range []struct {
		name string
		edit func(*providertransport.Operation)
		want string
	}{
		{name: "kind", edit: func(operation *providertransport.Operation) { operation.Kind = "custom" }, want: "has kind"},
		{name: "method", edit: func(operation *providertransport.Operation) { operation.Method = http.MethodGet }, want: "method"},
		{name: "path", edit: func(operation *providertransport.Operation) { operation.Path = "/compact" }, want: "path"},
		{name: "endpoint", edit: func(operation *providertransport.Operation) { operation.Endpoint.BaseURL = "wss://different.invalid" }, want: "endpoint does not match"},
		{name: "missing event bound", edit: func(operation *providertransport.Operation) { operation.Streaming.MaxEventBytes = 0 }, want: "streaming declaration"},
		{name: "event source", edit: func(operation *providertransport.Operation) { operation.Streaming.EventSource = "sse-data-json" }, want: "streaming declaration"},
		{name: "event mapping", edit: func(operation *providertransport.Operation) { operation.Streaming.Mappings[0].Event = "warning" }, want: "streaming declaration"},
		{name: "retry code", edit: func(operation *providertransport.Operation) { operation.Retry.Codes = []string{"temporary"} }, want: "retry codes"},
		{name: "retry after", edit: func(operation *providertransport.Operation) { operation.Retry.RetryAfter = true }, want: "Retry-After"},
		{name: "header", edit: func(operation *providertransport.Operation) {
			operation.Headers = []manifest.HeaderRule{{Name: "X-Synthetic"}}
		}, want: "headers are unavailable"},
		{name: "client identity", edit: func(operation *providertransport.Operation) {
			operation.ClientIdentity = &manifest.ResolvedClientIdentity{}
		}, want: "client identity is unavailable"},
		{name: "request transform", edit: func(operation *providertransport.Operation) { operation.RequestTransform = &manifest.JSONPipeline{} }, want: "transforms are unavailable"},
		{name: "response transform", edit: func(operation *providertransport.Operation) { operation.ResponseTransform = &manifest.JSONPipeline{} }, want: "transforms are unavailable"},
		{name: "prompt transform", edit: func(operation *providertransport.Operation) { operation.PromptTransform = &manifest.PromptPipeline{} }, want: "transforms are unavailable"},
		{name: "role map", edit: func(operation *providertransport.Operation) { operation.RoleMap = &manifest.RoleMap{} }, want: "role or tool declarations are unavailable"},
		{name: "tool codec", edit: func(operation *providertransport.Operation) { operation.ToolCodec = &manifest.ToolCodec{} }, want: "role or tool declarations are unavailable"},
		{name: "Anthropic policy", edit: func(operation *providertransport.Operation) { operation.Anthropic = &manifest.AnthropicPolicy{} }, want: "Anthropic declarations are unavailable"},
		{name: "system instruction", edit: func(operation *providertransport.Operation) {
			operation.SystemInstruction = &providertransport.ResolvedSystemInstruction{}
		}, want: "Anthropic declarations are unavailable"},
		{name: "continuation", edit: func(operation *providertransport.Operation) { operation.Continuation = &manifest.ContinuationPolicy{} }, want: "lifecycle declaration is unavailable"},
		{name: "nested compaction", edit: func(operation *providertransport.Operation) { operation.Compaction = &manifest.CompactionPolicy{} }, want: "lifecycle declaration is unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := registration.Clone()
			test.edit(candidate.Operations["remote-compact"])
			err := ValidateActivation(candidate)
			require.ErrorContains(t, err, test.want)
		})
	}
}

func TestRegistryValidatesErrorMappingBindingsAtActivation(t *testing.T) {
	mappings := []manifest.ErrorMapping{{Class: "authentication", Statuses: []int{401}}}
	operation := &providertransport.Operation{
		ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "websocket-json"},
		Method: http.MethodPost, Path: "/",
		Retry:  manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
		Errors: mappings,
	}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionCodex, CompatibilityAdapter: ConstructionCodex,
		Manifest: &manifest.Manifest{
			Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
			Capabilities: manifest.Capabilities{
				Compatibility: &manifest.CompatibilityAdapter{ID: string(ConstructionCodex), Delegates: []string{"construction"}},
				Errors:        mappings,
			},
		},
		Operation: operation,
		Operations: map[string]*providertransport.Operation{
			"inference": operation,
		},
		Errors: mappings,
	}
	require.NoError(t, ValidateActivation(base))

	for _, kind := range []string{"account", "usage"} {
		scoped := base.Clone()
		scoped.Operations[kind] = &providertransport.Operation{ID: kind, Kind: kind}
		require.NoError(t, validateManifestErrorBindings(scoped))
		scoped.Operations[kind].Errors = mappings
		require.ErrorContains(t, validateManifestErrorBindings(scoped), "do not match manifest scope")
	}

	empty := base.Clone()
	empty.Manifest.Capabilities.Errors = nil
	empty.Errors = []manifest.ErrorMapping{}
	empty.Operation.Errors = nil
	empty.Operations["inference"].Errors = []manifest.ErrorMapping{}
	require.NoError(t, ValidateActivation(empty))

	registeredMismatch := base.Clone()
	registeredMismatch.Errors = nil
	require.ErrorContains(t, ValidateActivation(registeredMismatch), "registered error mappings do not match")

	inferenceMismatch := base.Clone()
	inferenceMismatch.Operation.Errors = nil
	require.ErrorContains(t, ValidateActivation(inferenceMismatch), "inference operation error mappings do not match")

	compiledMismatch := base.Clone()
	compiledMismatch.Operations["inference"].Errors = nil
	require.ErrorContains(t, ValidateActivation(compiledMismatch), "compiled operation error mappings do not match")

	noBudget := base.Clone()
	noBudget.Manifest.Capabilities.Errors[0].Retryable = true
	noBudget.Errors[0].Retryable = true
	noBudget.Operation.Errors[0].Retryable = true
	noBudget.Operations["inference"].Errors[0].Retryable = true
	require.ErrorContains(t, ValidateActivation(noBudget), "has no retry attempt budget")

	noReplay := base.Clone()
	noReplay.Manifest.Capabilities.Errors[0].Retryable = true
	noReplay.Errors[0].Retryable = true
	noReplay.Operation.Errors[0].Retryable = true
	noReplay.Operation.Retry.MaxAttempts = 2
	noReplay.Operations["inference"].Errors[0].Retryable = true
	noReplay.Operations["inference"].Retry.MaxAttempts = 2
	require.ErrorContains(t, ValidateActivation(noReplay), "forbids request replay")

	compactionBudget := base.Clone()
	compactionBudget.Manifest.Capabilities.Errors[0].Retryable = true
	compactionBudget.Errors[0].Retryable = true
	compactionBudget.Operation.Errors[0].Retryable = true
	compactionBudget.Operation.Retry = manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "before-first-event"}
	compactionBudget.Operations["inference"].Errors[0].Retryable = true
	compactionBudget.Operations["inference"].Retry = compactionBudget.Operation.Retry
	compactionBudget.Operations["remote-compact"] = &providertransport.Operation{
		ID: "remote-compact", Kind: "compaction", Errors: cloneJSON(compactionBudget.Errors),
		Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
	}
	require.ErrorContains(t, validateManifestErrorBindings(compactionBudget), `no retry attempt budget for operation "remote-compact"`)
	compactionBudget.Operations["remote-compact"].Retry = manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "before-first-event"}
	require.NoError(t, validateManifestErrorBindings(compactionBudget))
}

func TestRegistryAcceptsAnthropicTransformsExecutedBySpecializedPolicy(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionAnthropicMessages, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionAnthropicMessages), Transport: "sse"},
			Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			Anthropic: &manifest.AnthropicPolicy{
				SystemLinePrefixes: []string{"Synthetic prefix:"},
			},
			PromptTransform: &manifest.PromptPipeline{Operations: []manifest.PromptOperation{{
				Operation: "remove-lines-with-prefix", Role: "system", Prefix: "Synthetic prefix:",
			}}},
			RoleMap: &manifest.RoleMap{System: "system", Developer: "system", User: "user", Assistant: "assistant", Tool: "tool", Unknown: "reject"},
		},
	}
	_, err := New(base)
	require.NoError(t, err)

	unsupportedPrompt := base.Clone()
	unsupportedPrompt.Operation.PromptTransform.Operations[0].Prefix = "Other prefix:"
	_, err = New(unsupportedPrompt)
	require.ErrorContains(t, err, "prompt transform is not executed")

	unsupportedRoles := base.Clone()
	unsupportedRoles.Operation.RoleMap.Developer = "developer"
	_, err = New(unsupportedRoles)
	require.ErrorContains(t, err, "role map is not executed")

	unsupportedResponse := base.Clone()
	unsupportedResponse.Operation.ResponseTransform = &manifest.JSONPipeline{Operations: []manifest.JSONOperation{{Operation: "delete", Path: "/unused"}}}
	_, err = New(unsupportedResponse)
	require.ErrorContains(t, err, "response transforms are unavailable")

	unboundedRetry := base.Clone()
	unboundedRetry.Manifest.Capabilities.Errors = []manifest.ErrorMapping{{Class: "server", Retryable: true}}
	unboundedRetry.Errors = []manifest.ErrorMapping{{Class: "server", Retryable: true}}
	unboundedRetry.Operation.Errors = []manifest.ErrorMapping{{Class: "server", Retryable: true}}
	unboundedRetry.Operation.Retry = manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "before-first-event"}
	_, err = New(unboundedRetry)
	require.ErrorContains(t, err, "requires an HTTP status predicate")

	statusRetry := unboundedRetry.Clone()
	statusRetry.Manifest.Capabilities.Errors[0].Statuses = []int{http.StatusServiceUnavailable}
	statusRetry.Errors[0].Statuses = []int{http.StatusServiceUnavailable}
	statusRetry.Operation.Errors[0].Statuses = []int{http.StatusServiceUnavailable}
	_, err = New(statusRetry)
	require.NoError(t, err)
}

func TestRegistryValidatesInferenceTimeoutsAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	for _, construction := range []Construction{ConstructionGenericJSON, ConstructionAnthropicMessages} {
		t.Run(string(construction), func(t *testing.T) {
			base := Registration{
				ProviderID: "synthetic", Construction: construction, Manifest: manifestValue,
				Operation: &providertransport.Operation{
					ID: "inference", Key: providertransport.Key{Protocol: string(construction), Transport: "sse"},
					Retry:    manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
					Timeouts: &manifest.TimeoutHints{RequestSeconds: 10}, RequestTimeout: 10 * time.Second,
				},
			}
			if construction == ConstructionGenericJSON {
				base.Operation.Streaming = &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}}
			} else {
				base.Operation.Anthropic = &manifest.AnthropicPolicy{}
			}
			_, err := New(base)
			require.NoError(t, err)

			connect := base.Clone()
			connect.Operation.Timeouts.ConnectSeconds = 1
			connect.Operation.ConnectTimeout = time.Second
			_, err = New(connect)
			require.NoError(t, err)

			idle := base.Clone()
			idle.Operation.Timeouts.IdleSeconds = 1
			idle.Operation.StreamIdleTimeout = time.Second
			_, err = New(idle)
			require.NoError(t, err)

			if construction == ConstructionGenericJSON {
				nonStreaming := idle.Clone()
				nonStreaming.Operation.Key.Transport = "http-json"
				nonStreaming.Operation.Streaming = nil
				_, err = New(nonStreaming)
				require.ErrorContains(t, err, "idle timeout requires SSE inference transport")
			}
		})
	}
}

func TestRegistryRejectsUnsupportedDeclarativeFramingAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionGenericJSON, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionGenericJSON), Transport: "sse"},
			Retry:     manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			Streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}},
		},
	}
	_, err := New(base)
	require.NoError(t, err)

	for _, test := range []struct {
		name      string
		transport string
		streaming *manifest.StreamingPolicy
		want      string
	}{
		{name: "missing SSE policy", transport: "sse", want: "requires a streaming policy using sse-data-json"},
		{name: "JSON sequence", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "json-sequence"}, want: "requires sse-data-json"},
		{name: "WebSocket events", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "websocket-json"}, want: "requires sse-data-json"},
		{name: "missing event type pointer", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}}, want: "require an event type pointer"},
		{name: "malformed event type pointer", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/~bad", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}}, want: "event type pointer is not a valid non-root JSON Pointer"},
		{name: "missing mapped finish", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "usage", Event: "usage"}}}, want: "requires a mapped finish event"},
		{name: "unsupported event field", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish", Fields: map[string]string{"ignored": "/value"}}}}, want: "unsupported normalized field"},
		{name: "malformed event field pointer", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish", Fields: map[string]string{"finish_reason": "/~bad"}}}}, want: "is not a valid non-root JSON Pointer"},
		{name: "metadata field without namespace", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish", Fields: map[string]string{"metadata.trace": "/trace"}}}}, want: "metadata field requires metadata_namespace"},
		{name: "duplicate event mapping", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "delta", Event: "text-delta"}, {Source: "delta", Event: "text-delta"}, {Source: "done", Event: "finish"}}}, want: `duplicates source "delta" event "text-delta"`},
		{name: "overlapping conditional event mapping", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "delta", Event: "text-delta", Condition: &manifest.Predicate{Operation: "equals", Path: "/kind", Value: &manifest.Template{Kind: "literal", Value: "text"}}}, {Source: "delta", Event: "text-delta", Condition: &manifest.Predicate{Operation: "equals", Path: "/other", Value: &manifest.Template{Kind: "literal", Value: "alternate"}}}, {Source: "done", Event: "finish"}}}, want: `duplicates source "delta" event "text-delta"`},
		{name: "multiple terminal mappings", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}, {Source: "done", Event: "error"}}}, want: `source "done" declares more than one terminal mapping`},
		{name: "conditionally disjoint terminal mappings", transport: "sse", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish", Condition: &manifest.Predicate{Operation: "equals", Path: "/status", Value: &manifest.Template{Kind: "literal", Value: "complete"}}}, {Source: "done", Event: "error", Condition: &manifest.Predicate{Operation: "equals", Path: "/status", Value: &manifest.Template{Kind: "literal", Value: "failed"}}}}}, want: `source "done" declares more than one terminal mapping`},
		{name: "HTTP JSON streaming policy", transport: "http-json", streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json"}, want: "must not declare streaming policy"},
	} {
		t.Run(test.name, func(t *testing.T) {
			registration := base.Clone()
			registration.Operation.Key.Transport = test.transport
			registration.Operation.Streaming = test.streaming
			_, err := New(registration)
			require.ErrorContains(t, err, test.want)
		})
	}

	httpJSON := base.Clone()
	httpJSON.Operation.Key.Transport = "http-json"
	httpJSON.Operation.Streaming = nil
	_, err = New(httpJSON)
	require.NoError(t, err)

	conditional := base.Clone()
	conditional.Operation.Streaming.Mappings = []manifest.EventMapping{
		{Source: "item", Event: "text-delta", Condition: &manifest.Predicate{Operation: "equals", Path: "/kind", Value: &manifest.Template{Kind: "literal", Value: "text"}}},
		{Source: "item", Event: "text-delta", Condition: &manifest.Predicate{Operation: "equals", Path: "/kind", Value: &manifest.Template{Kind: "literal", Value: "alternate"}}},
		{Source: "done", Event: "finish"},
	}
	_, err = New(conditional)
	require.NoError(t, err)
}

func TestRegistryValidatesDeclarativeUsageAndRuntimeControlScopes(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionGenericJSON, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionGenericJSON), Transport: "http-json"},
			Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
		},
		Usage:           &manifest.UsagePolicy{Source: "response", Fallback: "zero"},
		RuntimeControls: []manifest.RuntimeControl{{ID: "synthetic.mode", Type: "string", Scope: "model", RequestPath: "/mode"}},
	}
	_, err := New(base)
	require.NoError(t, err)

	streamUsage := base.Clone()
	streamUsage.Usage.Source = "stream"
	_, err = New(streamUsage)
	require.ErrorContains(t, err, "stream-sourced usage requires SSE transport")

	operationUsage := base.Clone()
	operationUsage.Usage.Source = "operation"
	operationUsage.Usage.Fallback = "unavailable"
	_, err = New(operationUsage)
	require.ErrorContains(t, err, "operation-sourced usage has no compiled quota executor")

	operationUsage.Quota = func(context.Context, string) (*oauthusage.Usage, error) { return &oauthusage.Usage{}, nil }
	operationUsage.Usage.Mappings = []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}
	_, err = New(operationUsage)
	require.ErrorContains(t, err, "operation-sourced usage mappings are unavailable")

	for _, test := range []struct {
		name string
		edit func(*manifest.UsagePolicy)
	}{
		{name: "operation", edit: func(value *manifest.UsagePolicy) { value.Operation = "usage" }},
		{name: "setup", edit: func(value *manifest.UsagePolicy) { value.Setup = []manifest.UsageSetup{{Operation: "setup"}} }},
		{name: "windows", edit: func(value *manifest.UsagePolicy) { value.Windows = []manifest.WindowMap{{ID: "daily"}} }},
		{name: "plan pointers", edit: func(value *manifest.UsagePolicy) { value.PlanPointers = []string{"/plan"} }},
	} {
		t.Run("response usage "+test.name, func(t *testing.T) {
			registration := base.Clone()
			test.edit(registration.Usage)
			_, err := New(registration)
			require.ErrorContains(t, err, "operation-only fields")
		})
	}

	for _, test := range []struct {
		name     string
		mappings []manifest.UsageMapping
		want     string
	}{
		{name: "duplicate target", mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input"}, {Target: "input_tokens", Pointer: "/usage/other"}}, want: "duplicates target"},
		{name: "malformed pointer", mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/~bad"}}, want: "not a valid non-root JSON Pointer"},
		{name: "subtract wrong target", mappings: []manifest.UsageMapping{{Target: "output_tokens", Pointer: "/usage/output", Operation: "subtract-cache-read"}}, want: "subtract-cache-read requires input_tokens"},
		{name: "subtract missing cache", mappings: []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "subtract-cache-read"}}, want: "requires a cache_read_tokens mapping"},
	} {
		t.Run("response usage "+test.name, func(t *testing.T) {
			registration := base.Clone()
			registration.Usage.Mappings = test.mappings
			_, err := New(registration)
			require.ErrorContains(t, err, test.want)
		})
	}

	usageEventWithoutPolicy := base.Clone()
	usageEventWithoutPolicy.Operation.Key.Transport = "sse"
	usageEventWithoutPolicy.Operation.Streaming = &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "usage", Event: "usage"}, {Source: "done", Event: "finish"}}}
	usageEventWithoutPolicy.Usage = nil
	_, err = New(usageEventWithoutPolicy)
	require.ErrorContains(t, err, "usage event requires stream-sourced usage policy")

	accumulate := base.Clone()
	accumulate.Operation.Key.Transport = "sse"
	accumulate.Operation.Streaming = &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish"}}}
	accumulate.Usage.Source = "stream"
	accumulate.Usage.Mappings = []manifest.UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "accumulate"}}
	_, err = New(accumulate)
	require.ErrorContains(t, err, "accumulation is unavailable")

	for _, scope := range []string{"provider", "request"} {
		registration := base.Clone()
		registration.RuntimeControls[0].Scope = scope
		_, err = New(registration)
		require.ErrorContains(t, err, "scope \""+scope+"\" is unavailable for declarative construction")
	}
}

func TestRegistryValidatesDeclarativeMetadataAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionGenericJSON, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionGenericJSON), Transport: "sse"},
			Retry:     manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			Streaming: &manifest.StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", Mappings: []manifest.EventMapping{{Source: "done", Event: "finish", MetadataNamespace: "synthetic.meta", Fields: map[string]string{"metadata.trace": "/trace"}}}},
		},
		Metadata: []manifest.MetadataContract{{Namespace: "synthetic.meta", Version: 1, Scope: "message", Schema: map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "object"}}},
	}
	_, err := New(base)
	require.NoError(t, err)

	textMetadata := base.Clone()
	textMetadata.Operation.Streaming.Mappings = []manifest.EventMapping{
		{Source: "delta", Event: "text-delta", Fields: map[string]string{"id": "/id", "delta": "/delta"}},
		{Source: "text-done", Event: "text-end", MetadataNamespace: "synthetic.meta", Fields: map[string]string{"id": "/id", "metadata.trace": "/trace"}},
		{Source: "done", Event: "finish"},
	}
	textMetadata.Metadata[0].Scope = "text"
	_, err = New(textMetadata)
	require.NoError(t, err)

	invalidSchema := base.Clone()
	invalidSchema.Metadata[0].Schema = map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "type": "not-a-type"}
	_, err = New(invalidSchema)
	require.ErrorContains(t, err, "schema is invalid")

	oversizedSchema := base.Clone()
	oversizedSchema.Metadata[0].Schema = map[string]any{"$schema": "https://json-schema.org/draft/2020-12/schema", "description": strings.Repeat("x", manifest.MaxMetadataSchemaBytes)}
	_, err = New(oversizedSchema)
	require.ErrorContains(t, err, "schema exceeds")

	requiredForReplay := base.Clone()
	requiredForReplay.Metadata[0].RequiredForReplay = true
	_, err = New(requiredForReplay)
	require.ErrorContains(t, err, "replay requirement is unavailable")

	legacyProjection := base.Clone()
	legacyProjection.Metadata[0].LegacyProjection = "legacy"
	_, err = New(legacyProjection)
	require.ErrorContains(t, err, "legacy projection")

	noMetadataFields := base.Clone()
	noMetadataFields.Operation.Streaming.Mappings[0].Fields = nil
	_, err = New(noMetadataFields)
	require.ErrorContains(t, err, "has no metadata fields")

	wrongVersion := base.Clone()
	wrongVersion.Metadata[0].Version = 2
	_, err = New(wrongVersion)
	require.ErrorContains(t, err, "unsupported envelope version")

	wrongScope := base.Clone()
	wrongScope.Metadata[0].Scope = "text"
	_, err = New(wrongScope)
	require.ErrorContains(t, err, "cannot be preserved")

	unreferenced := base.Clone()
	unreferenced.Metadata = append(unreferenced.Metadata, manifest.MetadataContract{Namespace: "synthetic.unused", Version: 1, Scope: "message"})
	_, err = New(unreferenced)
	require.ErrorContains(t, err, "is not emitted")

	duplicateNamespace := base.Clone()
	duplicate := duplicateNamespace.Metadata[0]
	duplicate.Scope = "reasoning"
	duplicateNamespace.Metadata = append(duplicateNamespace.Metadata, duplicate)
	_, err = New(duplicateNamespace)
	require.ErrorContains(t, err, "metadata namespace \"synthetic.meta\" is declared more than once")
}

func TestNativeResponsesRuntimeControlsUseOpenAINamespace(t *testing.T) {
	controls := []manifest.RuntimeControl{
		{ID: "vendor.mode", Type: "string", Scope: "model", RequestPath: "/vendor/mode"},
		{ID: "reasoning_effort", Type: "enum", Values: []string{"low", "high"}, Scope: "model", RequestPath: "/reasoning/effort"},
	}
	reasoning := declarativeReasoningCapability("synthetic", ConstructionOpenAIResponses, controls)
	options, err := reasoning.Options("model", "", false, map[string]any{"vendor.mode": "selected", "reasoning_effort": "high"})
	require.NoError(t, err)
	native, ok := options[openai.Name].(*openai.ResponsesProviderOptions)
	require.True(t, ok)
	require.Equal(t, map[string]any{"/vendor/mode": "selected", "/reasoning/effort": "high"}, native.RuntimeControls)
	require.NotContains(t, options, "synthetic")

	runtime := declarativeRuntimeCapability("synthetic", ConstructionOpenAIResponses, controls)
	options = runtime.Apply(RuntimeValues{AnalysisEffort: "low"}, options)
	native, ok = options[openai.Name].(*openai.ResponsesProviderOptions)
	require.True(t, ok)
	require.Equal(t, "selected", native.RuntimeControls["/vendor/mode"])
	require.Equal(t, "low", native.RuntimeControls["/reasoning/effort"])
}

func TestRegistryValidatesOpenAIRuntimeControlsAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionOpenAIResponses, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "sse"},
			Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
		},
	}
	withDefault := base.Clone()
	withDefault.RuntimeControls = []manifest.RuntimeControl{{ID: "synthetic.mode", Type: "string", Scope: "provider", RequestPath: "/mode", Default: "safe"}}
	_, err := New(withDefault)
	require.NoError(t, err)

	requestScoped := base.Clone()
	requestScoped.RuntimeControls = []manifest.RuntimeControl{{ID: "response_verbosity", Type: "enum", Scope: "request", RequestPath: "/text/verbosity"}}
	_, err = New(requestScoped)
	require.ErrorContains(t, err, "cannot be supplied")

	perCall := base.Clone()
	perCall.RuntimeControls = []manifest.RuntimeControl{{ID: "synthetic.mode", Type: "string", Scope: "model", RequestPath: "/mode"}}
	_, err = New(perCall)
	require.NoError(t, err)
}

func TestRegistryRejectsNonExecutablePluginPoliciesAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{ProviderID: "synthetic", Manifest: manifestValue}

	codedError := base.Clone()
	codedError.Errors = []manifest.ErrorMapping{{Class: "authentication", Codes: []string{"expired"}}}
	codedError.Manifest.Capabilities.Errors = codedError.Errors
	_, err := New(codedError)
	require.ErrorContains(t, err, "code_pointer is required")

	historyBudget := base.Clone()
	historyBudget.Images = &manifest.ImagePolicy{HistoryBudget: &manifest.ImageHistoryBudget{RequestBytes: 1024}}
	historyBudget.Manifest.Capabilities.Images = historyBudget.Images
	_, err = New(historyBudget)
	require.ErrorContains(t, err, "image history policy is unavailable")

	unencodableImages := base.Clone()
	unencodableImages.Images = &manifest.ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/webp"},
		MaxSourceBytes:     1024,
		MaxSidePixels:      512,
	}
	unencodableImages.Manifest.Capabilities.Images = unencodableImages.Images
	_, err = New(unencodableImages)
	require.ErrorContains(t, err, "no accepted output media type supported by the core image encoder")

	unsupportedOutput := base.Clone()
	unsupportedOutput.Images = &manifest.ImagePolicy{AcceptedMediaTypes: []string{"image/webp"}, MaxSourceBytes: 1024, OutputMediaType: "image/webp"}
	unsupportedOutput.Manifest.Capabilities.Images = unsupportedOutput.Images
	_, err = New(unsupportedOutput)
	require.ErrorContains(t, err, "not supported by the core image encoder")

	unacceptedOutput := base.Clone()
	unacceptedOutput.Images = &manifest.ImagePolicy{AcceptedMediaTypes: []string{"image/png"}, MaxSourceBytes: 1024, OutputMediaType: "image/jpeg"}
	unacceptedOutput.Manifest.Capabilities.Images = unacceptedOutput.Images
	_, err = New(unacceptedOutput)
	require.ErrorContains(t, err, "not declared in accepted_media_types")

	multipleInference, err := FromManifest(manifest.Manifest{
		Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"},
		Capabilities: manifest.Capabilities{
			Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
			Operations: []manifest.Operation{
				{ID: "inference", Kind: "inference", Protocol: string(ConstructionGenericJSON), Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/inference"},
				{ID: "unused-inference", Kind: "inference", Protocol: string(ConstructionGenericJSON), Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/unused"},
			},
		},
	})
	require.NoError(t, err)
	_, err = New(multipleInference)
	require.ErrorContains(t, err, `operation "unused-inference" at index 1 has no host executor`)
}

func TestRegistryValidatesOpenAIResponsesToolCodecAtActivation(t *testing.T) {
	manifestValue := &manifest.Manifest{Provider: manifest.Provider{ID: "synthetic", Name: "Synthetic"}}
	base := Registration{
		ProviderID: "synthetic", Construction: ConstructionOpenAIResponses, Manifest: manifestValue,
		Operation: &providertransport.Operation{
			ID: "inference", Key: providertransport.Key{Protocol: string(ConstructionOpenAIResponses), Transport: "sse"},
			Retry: manifest.RetryPolicy{MaxAttempts: 1, Authentication: "never", ReplayRequirement: "never"},
			ToolCodec: &manifest.ToolCodec{
				Aliases:         []manifest.ToolAlias{{Host: "view", Provider: "read_file"}},
				Parameters:      []manifest.ParameterMap{{Tool: "view", Host: "file_path", Provider: "path"}},
				Surfaces:        []string{"definitions", "prompt-references", "history-calls", "stream-events"},
				CaseFoldInbound: true,
			},
		},
	}
	_, err := New(base)
	require.NoError(t, err)

	prefix := base.Clone()
	prefix.Operation.ToolCodec.PrefixAliases = []manifest.ToolPrefixAlias{{HostPrefix: "mcp_", ProviderPrefix: "mcp__"}}
	_, err = New(prefix)
	require.ErrorContains(t, err, "prefix aliases are unavailable")

	search := base.Clone()
	search.Operation.ToolCodec.ToolSearch = "regex"
	_, err = New(search)
	require.ErrorContains(t, err, `search mode "regex" is unavailable`)

	historyResults := base.Clone()
	historyResults.Operation.ToolCodec.Surfaces = append(historyResults.Operation.ToolCodec.Surfaces, "history-results")
	_, err = New(historyResults)
	require.ErrorContains(t, err, `surface "history-results" is unavailable`)

	unknownSurface := base.Clone()
	unknownSurface.Operation.ToolCodec.Surfaces = append(unknownSurface.Operation.ToolCodec.Surfaces, "request-body")
	_, err = New(unknownSurface)
	require.ErrorContains(t, err, `surface "request-body" is unavailable`)
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

func TestRegistrationMapErrorAppliesClassSemantics(t *testing.T) {
	tests := []struct {
		class   string
		title   string
		auth    bool
		retry   bool
		context bool
	}{
		{class: "authentication", title: "Authentication required", auth: true},
		{class: "authorization", title: "Authorization denied"},
		{class: "rate-limit", title: "Rate limit reached"},
		{class: "capacity", title: "Provider capacity unavailable"},
		{class: "context-overflow", title: "Context limit exceeded", context: true},
		{class: "invalid-request", title: "Invalid provider request"},
		{class: "content-filter", title: "Content blocked"},
		{class: "server", title: "Provider server error"},
		{class: "transport", title: "Provider transport error"},
		{class: "unknown", title: "Provider error"},
	}
	for _, test := range tests {
		t.Run(test.class, func(t *testing.T) {
			registration := Registration{Errors: []manifest.ErrorMapping{{Class: test.class}}}
			providerErr := &fantasy.ProviderError{Message: "provider message"}

			require.Same(t, providerErr, registration.MapError(providerErr))
			require.Equal(t, fantasy.ProviderErrorClass(test.class), providerErr.Class)
			require.Equal(t, test.title, providerErr.Title)
			require.Equal(t, test.auth, providerErr.AuthError)
			require.Equal(t, test.retry, providerErr.IsRetryable())
			require.Equal(t, test.context, providerErr.IsContextTooLarge())
		})
	}
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

func TestRegistryAccountNamespaceAndRefresherPublishTogether(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	refresh := func(_ context.Context, refreshToken string) (*oauth.Token, error) {
		require.Equal(t, "refresh-old", refreshToken)
		return &oauth.Token{
			AccessToken:  "access-new",
			RefreshToken: "refresh-new",
			ExpiresAt:    time.Now().Add(time.Hour).Unix(),
		}, nil
	}
	registry, err := New(Registration{
		ProviderID:       "registry-account-test",
		AccountNamespace: "registry-account-namespace",
		AccountOrder:     500,
		OAuth:            &OAuthCapability{Refresh: refresh},
	})
	require.NoError(t, err)
	require.Empty(t, accounts.StoreKey("registry-account-test"))
	accounts.PublishProviders(registry.AccountRegistrations())
	t.Cleanup(func() { accounts.PublishProviders(nil) })
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
