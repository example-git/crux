package config

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

type authenticationCandidateFixture struct {
	store        *ConfigStore
	root, marker string
	before       AuthenticationCapture
	owner        providerregistry.RegistrationOwner
}

func newAuthenticationCandidateFixture(t *testing.T, id string, configured, disabled bool, tokenEndpoint string) authenticationCandidateFixture {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"),
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"),
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileIntegrated),
		"CRUX_DISABLE_AUTO_MEMORY": "true",
		"CAPTURED_HEADER":          "accepted-header", "PATH": os.Getenv("PATH"),
	}
	marker := filepath.Join(root, "header-count")
	provider := ProviderConfig{ID: id, Disable: disabled,
		BaseURL: "https://api.example.invalid/custom", SystemPromptPrefix: "preserve prompt", ToolingInstructions: "crux",
		ExtraHeaders:    map[string]string{"X-Captured": "$CAPTURED_HEADER", "X-Once": fmt.Sprintf("$(printf x >> '%s'; printf command-header)", filepath.ToSlash(marker)), "X-Empty": "${ABSENT_CANDIDATE_HEADER:-}"},
		ExtraBody:       map[string]any{"precise": json.Number("37"), "nested": map[string]any{"keep": false}},
		ProviderOptions: map[string]any{"literal": "$DO_NOT_EXPAND"}, FlatRate: true,
		Models: []catalog.Model{{ID: "user-model"}, {ID: "example-reasoner", Name: "User Reasoner"}, {ID: "user-model", Name: "duplicate"}},
	}
	if id == "example-responses" {
		values["CRUX_PROVIDER_PROFILE"] = string(ProviderProfilePluginNative)
		values["CRUX_PROVIDER_PLUGINS"] = "example-responses"
		provider.Plugin = &ProviderPluginReference{ID: "example.responses-oauth"}
		provider.Configuration = map[string]any{"oauth_client_id": "captured-client"}
		bundle := filepath.Join(root, "responses.plugin")
		require.NoError(t, os.CopyFS(bundle, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
		if tokenEndpoint != "" {
			data, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
			require.NoError(t, err)
			var declaration manifest.Manifest
			require.NoError(t, json.Unmarshal(data, &declaration))
			target, err := url.Parse(tokenEndpoint)
			require.NoError(t, err)
			for i := range declaration.Capabilities.Endpoints {
				endpoint := &declaration.Capabilities.Endpoints[i]
				if endpoint.ID == "token" {
					endpoint.BaseURL, endpoint.AllowedHosts = tokenEndpoint, []string{target.Hostname()}
				}
			}
			data, err = json.Marshal(declaration)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0o600))
		}
		installTrustedProviderBundle(t, values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"], bundle)
	}
	if configured {
		provider.APIKey = "synthetic-previous-access"
	}
	providers := map[string]ProviderConfig{"unrelated": {
		ID: "unrelated", Type: catalog.TypeOpenAICompat, APIKey: "synthetic-unrelated-key", BaseURL: "https://unrelated.example.invalid/v1",
		Models: []catalog.Model{{ID: "main", DefaultMaxTokens: 100}, {ID: "small", DefaultMaxTokens: 50}},
	}}
	if id != "codex" { // codex exercises a genuinely absent raw target.
		providers[id] = provider
	}
	document := &Config{Providers: csync.NewMapFrom(providers), Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: "unrelated", Model: "main", ProviderOptions: map[string]any{"nested": map[string]any{"keep": false}}},
		SelectedModelTypeSmall: {Provider: "unrelated", Model: "small", MaxTokens: 37},
	}}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), data, 0o600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
	require.NoError(t, err)
	_, present := store.Config().Providers.Get(id)
	require.Equal(t, configured || disabled, present)
	if id != "codex" {
		count, err := os.ReadFile(marker)
		require.NoError(t, err)
		require.Equal(t, "x", string(count), "the normal loader already evaluated the raw header once")
		require.NoError(t, os.Remove(marker))
	}
	// An existing custom agent must survive candidate construction unchanged.
	next := store.Config().cloneForWrite()
	next.Agents = maps.Clone(next.Agents)
	next.Agents["retained-agent"] = Agent{ID: "retained-agent", Instructions: "private accepted instructions", AllowedTools: []string{"view"}, Model: SelectedModelTypeSmall}
	store.setConfig(next)
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	owner, ok := before.runtime.ProviderOwner(id)
	require.True(t, ok)
	return authenticationCandidateFixture{store: store, root: root, marker: marker, before: before, owner: owner}
}

func assertAuthenticationCandidatePreservesSiblings(t *testing.T, before, after *Config, id string) {
	t.Helper()
	want, got := before.cloneForWrite(), after.cloneForWrite()
	want.Providers.Del(id)
	got.Providers.Del(id)
	got.authenticationRevocations = want.authenticationRevocations
	require.Equal(t, mustMarshalConfig(want), mustMarshalConfig(got))
	require.Equal(t, before.Models, after.Models)
	require.Equal(t, before.Agents, after.Agents)
	require.Equal(t, before.authenticationBasis, after.authenticationBasis)
	require.Same(t, before.providerScan, after.providerScan)
}

func TestAuthenticationCandidateConfiguredPreservesAcceptedFields(t *testing.T) {
	for _, target := range []struct {
		id       string
		disabled bool
	}{{"copilot", false}, {"example-responses", false}, {"example-responses", true}} {
		t.Run(fmt.Sprintf("%s/disabled=%t", target.id, target.disabled), func(t *testing.T) {
			id := target.id
			fixture := newAuthenticationCandidateFixture(t, id, true, target.disabled, "")
			// Capture complete target selections, including an explicitly disabled
			// provider. Authentication maintenance must not enable or replace them.
			accepted := fixture.store.Config().cloneForWrite()
			accepted.Models[SelectedModelTypeLarge] = SelectedModel{Provider: id, Model: "user-model", MaxTokens: 71, ProviderOptions: map[string]any{"literal.control": false}}
			accepted.Models[SelectedModelTypeSmall] = SelectedModel{Provider: id, Model: "example-reasoner", MaxTokens: 29}
			fixture.store.setConfig(accepted)
			var err error
			fixture.before, err = fixture.store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			before := fixture.before.runtime.Config()
			diskBefore, err := os.ReadFile(filepath.Join(fixture.root, "crux.json"))
			require.NoError(t, err)
			original, _ := before.Providers.Get(id)
			token := &oauth.Token{AccessToken: "literal-$(must-not-execute)-$TOKEN", RefreshToken: "synthetic-refresh", ExpiresIn: 3600}
			fixture.before.runtime.resolver = authenticationCandidateForbiddenResolver{}
			prepared, err := prepareAuthenticationProvider(t.Context(), fixture.before, fixture.owner, token)
			require.NoError(t, err)
			want := cloneProviderConfig(original)
			registration, err := authenticationRegistration(fixture.before, fixture.owner)
			require.NoError(t, err)
			applyOAuthTokenToProvider(&want, cloneOAuthToken(token), registration)
			want.APIKeyTemplate = ""
			require.Equal(t, want, prepared)
			if id == "copilot" {
				for name, value := range copilot.Headers() {
					require.Equal(t, value, prepared.ExtraHeaders[name])
				}
			}
			candidate, err := authenticationConfigCandidate(fixture.before, fixture.owner, &prepared)
			require.NoError(t, err)
			assertAuthenticationCandidatePreservesSiblings(t, before, candidate, id)
			require.NoFileExists(t, fixture.marker, "configured headers must not be re-expanded")
			require.Same(t, before, fixture.store.Config())
			require.True(t, fixture.before.runtime.SamePublication(fixture.store.RuntimeSnapshot()))
			token.AccessToken = "caller mutation"
			prepared.ExtraHeaders["X-Captured"] = "caller mutation"
			actual, _ := candidate.Providers.Get(id)
			require.Equal(t, want, actual)
			current, _ := before.Providers.Get(id)
			require.Equal(t, original, current)
			diskAfter, err := os.ReadFile(filepath.Join(fixture.root, "crux.json"))
			require.NoError(t, err)
			require.Equal(t, diskBefore, diskAfter)
		})
	}
}

type authenticationCandidateForbiddenResolver struct{}

func (authenticationCandidateForbiddenResolver) ResolveValue(string) (string, error) {
	panic("candidate resolved an accepted value")
}

func TestAuthenticationCandidateReconstructsUnconfiguredPlugin(t *testing.T) {
	fixture := newAuthenticationCandidateFixture(t, "example-responses", false, false, "")
	t.Setenv("CAPTURED_HEADER", "wrong-live-value")
	before := fixture.before.runtime.Config()
	registration, err := authenticationRegistration(fixture.before, fixture.owner)
	require.NoError(t, err)
	require.Equal(t, fixture.owner, registration.Owner())
	require.NotNil(t, registration.OAuth.Refresh)
	require.NoFileExists(t, fixture.marker, "pure registration lookup must not run headers")
	prepared, err := prepareAuthenticationProvider(t.Context(), fixture.before, fixture.owner, &oauth.Token{AccessToken: "synthetic-selected"})
	require.NoError(t, err)
	require.Equal(t, "synthetic-selected", prepared.APIKey)
	require.Empty(t, prepared.APIKeyTemplate)
	require.Equal(t, "https://api.example.invalid/custom", prepared.BaseURL)
	require.Equal(t, "preserve prompt", prepared.SystemPromptPrefix)
	require.Equal(t, map[string]any{"oauth_client_id": "captured-client"}, prepared.Configuration)
	require.Equal(t, json.Number("37"), prepared.ExtraBody["precise"])
	require.Equal(t, "$DO_NOT_EXPAND", prepared.ProviderOptions["literal"])
	require.Equal(t, []string{"user-model", "example-reasoner", "example-small"}, []string{prepared.Models[0].ID, prepared.Models[1].ID, prepared.Models[2].ID})
	require.Equal(t, "user-model", prepared.Models[0].Name)
	require.Equal(t, "User Reasoner", prepared.Models[1].Name)
	require.Equal(t, "accepted-header", prepared.ExtraHeaders["X-Captured"])
	require.Equal(t, "command-header", prepared.ExtraHeaders["X-Once"])
	require.NotContains(t, prepared.ExtraHeaders, "X-Empty")
	count, err := os.ReadFile(fixture.marker)
	require.NoError(t, err)
	require.Equal(t, "x", string(count))
	candidate, err := authenticationConfigCandidate(fixture.before, fixture.owner, &prepared)
	require.NoError(t, err)
	assertAuthenticationCandidatePreservesSiblings(t, before, candidate, prepared.ID)
	count, err = os.ReadFile(fixture.marker)
	require.NoError(t, err)
	require.Equal(t, "x", string(count), "candidate assembly must not run headers again")
	_, exists := before.Providers.Get(prepared.ID)
	require.False(t, exists)
}

func TestAuthenticationCandidateUsesExactCapturedBasisAndCatalogHeaders(t *testing.T) {
	fixture := newAuthenticationCandidateFixture(t, "example-responses", false, false, "")
	before := fixture.before
	before.runtime.config = before.runtime.config.cloneForWrite()
	basis := before.runtime.config.authenticationBasis.clone()
	// The general loader's JSON merger has a separate precision limitation.
	// This check starts from an exact accepted basis and verifies this helper
	// does not introduce a second float conversion while recovering the target.
	require.Contains(t, string(basis.configured), `"precise":37`)
	basis.configured = bytes.Replace(basis.configured, []byte(`"precise":37`), []byte(`"precise":9007199254740993`), 1)
	before.runtime.config.authenticationBasis = basis
	scan := cloneProviderScan(*before.runtime.config.providerScan)
	for i := range scan.Providers {
		if string(scan.Providers[i].ID) == fixture.owner.ProviderID {
			scan.Providers[i].DefaultHeaders = map[string]string{
				"X-Catalog": "$CAPTURED_HEADER", "X-Captured": "catalog-overridden", "X-Catalog-Empty": "${ABSENT_CANDIDATE_HEADER:-}",
			}
		}
	}
	before.runtime.config.providerScan = &scan
	prepared, err := prepareAuthenticationProvider(t.Context(), before, fixture.owner, &oauth.Token{AccessToken: "synthetic-selected"})
	require.NoError(t, err)
	require.Equal(t, json.Number("9007199254740993"), prepared.ExtraBody["precise"])
	require.Equal(t, "accepted-header", prepared.ExtraHeaders["X-Catalog"])
	require.Equal(t, "accepted-header", prepared.ExtraHeaders["X-Captured"], "user input overrides the catalog header before expansion")
	require.NotContains(t, prepared.ExtraHeaders, "X-Catalog-Empty")
	for _, known := range scan.Providers {
		if string(known.ID) == fixture.owner.ProviderID {
			require.Equal(t, "$CAPTURED_HEADER", known.DefaultHeaders["X-Catalog"], "captured catalog is immutable")
		}
	}
}

func TestAuthenticationCandidateCompletesOnlyMissingAcceptedOwner(t *testing.T) {
	for _, id := range []string{"copilot", "example-responses"} {
		t.Run(id, func(t *testing.T) {
			fixture := newAuthenticationCandidateFixture(t, id, true, false, "")
			legacy := fixture.store.Config().cloneForWrite()
			provider, _ := legacy.Providers.Get(id)
			provider.Owner = nil
			legacy.Providers.Set(id, provider)
			fixture.store.setConfig(legacy)
			before, err := fixture.store.CaptureAuthentication(t.Context())
			require.NoError(t, err)
			current, ok := before.runtime.ProviderOwner(id)
			require.True(t, ok, "the store supports this accepted legacy owner")
			require.Equal(t, fixture.owner, current)
			before.runtime.resolver = authenticationCandidateForbiddenResolver{}
			prepared, err := prepareAuthenticationProvider(t.Context(), before, fixture.owner, &oauth.Token{AccessToken: "synthetic-selected"})
			require.NoError(t, err)
			registration, ok := before.runtime.ProviderRegistrationFor(id, prepared)
			require.True(t, ok)
			require.Equal(t, fixture.owner, registration.Owner())
			actual, _ := legacy.Providers.Get(id)
			require.Nil(t, actual.Owner, "completion must not mutate the accepted provider")
			require.NoFileExists(t, fixture.marker)
		})
	}
}

func TestAuthenticationRegistrationUsesCapturedPluginBindings(t *testing.T) {
	var calls atomic.Int32
	var clientID string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.NoError(t, r.ParseForm())
		clientID = r.Form.Get("client_id")
		require.Equal(t, "synthetic-refresh", r.Form.Get("refresh_token"))
		_, _ = fmt.Fprint(w, `{"access_token":"synthetic-selected","expires_in":3600}`)
	}))
	t.Cleanup(server.Close)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = oldTransport })
	fixture := newAuthenticationCandidateFixture(t, "example-responses", false, false, server.URL+"/token")
	bound, err := authenticationRegistration(fixture.before, fixture.owner)
	require.NoError(t, err)
	require.Zero(t, calls.Load())
	require.NoFileExists(t, fixture.marker)
	token, err := bound.OAuth.Refresh(t.Context(), "synthetic-refresh")
	require.NoError(t, err)
	require.Equal(t, "captured-client", clientID)
	require.Equal(t, int32(1), calls.Load())
	_, err = prepareAuthenticationProvider(t.Context(), fixture.before, fixture.owner, token)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load(), "usable selected token needs no verification or refresh during preparation")
}

func TestAuthenticationCandidateLogoutPreservesAbsentAndDisabledTargets(t *testing.T) {
	for _, target := range []struct {
		id                   string
		configured, disabled bool
	}{{"codex", false, false}, {"copilot", true, false}, {"copilot", false, true}} {
		t.Run(fmt.Sprintf("%s/configured=%t/disabled=%t", target.id, target.configured, target.disabled), func(t *testing.T) {
			fixture := newAuthenticationCandidateFixture(t, target.id, target.configured, target.disabled, "")
			candidate, err := authenticationConfigCandidate(fixture.before, fixture.owner, nil)
			require.NoError(t, err)
			assertAuthenticationCandidatePreservesSiblings(t, fixture.before.runtime.config, candidate, target.id)
			provider, exists := candidate.Providers.Get(target.id)
			require.Equal(t, target.configured || target.disabled, exists)
			if exists {
				require.Empty(t, provider.APIKey)
				require.Empty(t, provider.APIKeyTemplate)
				require.Nil(t, provider.OAuthToken)
				require.Equal(t, target.disabled, provider.Disable)
				require.ErrorIs(t, (RuntimeSnapshot{config: candidate, registry: fixture.before.runtime.registry}).AuthenticationRevocation(target.id), ErrAuthenticationRevoked)
			} else {
				require.Empty(t, candidate.authenticationRevocations)
				prepared, err := prepareAuthenticationProvider(t.Context(), fixture.before, fixture.owner, &oauth.Token{AccessToken: "synthetic-first"})
				require.NoError(t, err)
				require.Equal(t, target.id, prepared.ID)
				require.NotEmpty(t, prepared.Models)
			}
		})
	}
}

func TestAuthenticationCandidateRejectsInvalidPreparationBeforeEffects(t *testing.T) {
	fixture := newAuthenticationCandidateFixture(t, "example-responses", false, false, "")
	for _, test := range []string{"empty-token", "wrong-owner", "uncaptured-owner", "detached", "missing-registry", "missing-catalog", "duplicate-catalog", "missing-basis", "conflicting-raw-owner", "invalid-configuration"} {
		t.Run(test, func(t *testing.T) {
			before := fixture.before
			before.runtime.config = before.runtime.config.cloneForWrite()
			owner := fixture.owner
			token := &oauth.Token{AccessToken: "synthetic-next"}
			switch test {
			case "empty-token":
				token.AccessToken = ""
			case "wrong-owner":
				owner.AccountNamespace = "wrong-owner"
			case "uncaptured-owner":
				before.owners = nil
			case "detached":
				before.runtime.clientRuntime = &clientRuntimeState{}
			case "missing-registry":
				before.runtime.registry = nil
			case "missing-basis":
				before.runtime.config.authenticationBasis = nil
			case "missing-catalog", "duplicate-catalog":
				scan := cloneProviderScan(*before.runtime.config.providerScan)
				for _, known := range scan.Providers {
					if string(known.ID) == owner.ProviderID && test == "duplicate-catalog" {
						scan.Providers = append(scan.Providers, known)
						break
					}
				}
				if test == "missing-catalog" {
					scan.Providers = slices.DeleteFunc(scan.Providers, func(p catalog.Provider) bool { return string(p.ID) == owner.ProviderID })
				}
				before.runtime.config.providerScan = &scan
			case "conflicting-raw-owner", "invalid-configuration":
				basis := before.runtime.config.authenticationBasis.clone()
				var raw map[string]any
				require.NoError(t, json.Unmarshal(basis.configured, &raw))
				provider := raw["providers"].(map[string]any)[owner.ProviderID].(map[string]any)
				if test == "conflicting-raw-owner" {
					provider["owner"] = map[string]any{"type": "custom", "construction": "openai-compat"}
				} else {
					provider["configuration"] = map[string]any{"oauth_client_id": "synthetic-private-value", "secret": "must-not-leak"}
				}
				var err error
				basis.configured, err = json.Marshal(raw)
				require.NoError(t, err)
				before.runtime.config.authenticationBasis = basis
			}
			_, err := prepareAuthenticationProvider(t.Context(), before, owner, token)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "must-not-leak")
			require.NotContains(t, err.Error(), "synthetic-private-value")
			require.NoFileExists(t, fixture.marker)
			require.True(t, fixture.before.runtime.SamePublication(fixture.store.RuntimeSnapshot()))
		})
	}
}

func TestAuthenticationCandidateRejectsInvalidPreparedCredentials(t *testing.T) {
	fixture := newAuthenticationCandidateFixture(t, "copilot", true, false, "")
	prepared, err := prepareAuthenticationProvider(t.Context(), fixture.before, fixture.owner, &oauth.Token{AccessToken: "synthetic-selected"})
	require.NoError(t, err)
	for _, invalid := range []string{"nil-token", "empty-token", "different-key", "old-template", "changed-owner"} {
		t.Run(invalid, func(t *testing.T) {
			provider := cloneProviderConfig(prepared)
			switch invalid {
			case "nil-token":
				provider.OAuthToken = nil
			case "empty-token":
				provider.OAuthToken.AccessToken, provider.APIKey = "", ""
			case "different-key":
				provider.APIKey = "synthetic-unrelated"
			case "old-template":
				provider.APIKeyTemplate = "$OLD_ACCESS_TOKEN"
			case "changed-owner":
				provider.Owner.Type = ProviderOwnerCustom
			}
			_, err := authenticationConfigCandidate(fixture.before, fixture.owner, &provider)
			require.Error(t, err)
			require.True(t, fixture.before.runtime.SamePublication(fixture.store.RuntimeSnapshot()))
		})
	}
}

func TestAuthenticationCandidateHeaderCancellationAndSafeFailure(t *testing.T) {
	fixture := newAuthenticationCandidateFixture(t, "example-responses", false, false, "")
	for _, canceled := range []bool{false, true} {
		t.Run(fmt.Sprint(canceled), func(t *testing.T) {
			before := fixture.before
			entered := make(chan struct{})
			before.runtime.resolver = NewShellVariableResolver(env.NewFromMap(nil), WithExpander(func(ctx context.Context, _ string, _ []string) (string, error) {
				if !canceled {
					return "", errors.New("synthetic-private-error-payload")
				}
				close(entered)
				<-ctx.Done()
				return "", ctx.Err()
			}))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := prepareAuthenticationProvider(ctx, before, fixture.owner, &oauth.Token{AccessToken: "synthetic-next"})
				result <- err
			}()
			if canceled {
				<-entered
				cancel()
			}
			select {
			case err := <-result:
				require.Error(t, err)
				require.NotContains(t, err.Error(), "synthetic-private-error-payload")
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("header preparation did not respect cancellation")
			}
			require.NoFileExists(t, fixture.marker)
		})
	}
}
