package providerdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	oauthusage "github.com/example-git/crux/internal/oauth/usage"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type namespaceDiagnosticFixture struct {
	store        *config.ConfigStore
	proposal     config.RemoteRuntimeProposal
	registration providerregistry.Registration
	provider     config.ProviderConfig
	token        *oauth.Token
	accountRoot  string
	ambientRoot  string
	marker       []byte
}

func namespaceDiagnosticStore(t *testing.T, server *httptest.Server, accepted, native bool, version ...string) namespaceDiagnosticFixture {
	t.Helper()
	oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient, http.DefaultTransport = server.Client(), server.Client().Transport
	if accepted && !native {
		transport := http.DefaultTransport
		http.DefaultTransport = namespaceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
			// These keys were absent from the admitted credential environment.
			// Check the actual request context before it reaches disposable TLS.
			for _, name := range []string{"DIAGNOSTIC_CAPTURED", "DIAGNOSTIC_ABSENT"} {
				_, present := oauth.LookupEnvironment(request.Context(), name)
				assert.False(t, present, "%s fell back to ambient environment", name)
			}
			return transport.RoundTrip(request)
		})
	}
	t.Cleanup(func() { http.DefaultClient, http.DefaultTransport = oldClient, oldTransport })
	root := t.TempDir()
	f := namespaceDiagnosticFixture{accountRoot: filepath.Join(root, "captured-accounts"), ambientRoot: filepath.Join(root, "ambient-accounts"), marker: []byte("not an account directory")}
	// A directory lookup here would fail. The marker and missing ambient root
	// prove that diagnostics did not need account storage to reach HTTPS.
	require.NoError(t, os.WriteFile(f.accountRoot, f.marker, 0600))
	t.Setenv("AI_CLI_DIR", f.accountRoot)
	t.Setenv("DIAGNOSTIC_CAPTURED", "accepted-environment")
	t.Setenv("CODEX_CLI_VERSION", "1.2.3")
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "captured-diagnostic")
	t.Setenv("TERM_PROGRAM", "CapturedTerminal")
	t.Setenv("DIAGNOSTIC_ABSENT", "")
	require.NoError(t, os.Unsetenv("DIAGNOSTIC_ABSENT"))
	source := filepath.Join(root, "fixture.plugin")
	require.NoError(t, os.CopyFS(source, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	var declaration manifest.Manifest
	require.NoError(t, json.Unmarshal(data, &declaration))
	if len(version) != 0 {
		declaration.Version = version[0]
	}
	declaration.Provider.AccountNamespace = ""
	declaration.Capabilities.Instructions = nil
	require.NoError(t, os.RemoveAll(filepath.Join(source, "instructions")))
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	for index := range declaration.Capabilities.Endpoints {
		endpoint := &declaration.Capabilities.Endpoints[index]
		if endpoint.ID == "api" {
			endpoint.BaseURL, endpoint.AllowedHosts = server.URL, []string{target.Hostname()}
		}
	}
	declaration.Capabilities.Operations = append(declaration.Capabilities.Operations,
		manifest.Operation{ID: "diagnostic-setup", Kind: "custom", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/usage/setup"},
		manifest.Operation{ID: "diagnostic-usage", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: http.MethodPost, Path: "/usage/final"})
	declaration.Capabilities.Usage = &manifest.UsagePolicy{Source: "operation", Operation: "diagnostic-usage", Fallback: "unavailable", Setup: []manifest.UsageSetup{{Operation: "diagnostic-setup"}}, Windows: []manifest.WindowMap{{ID: "usage", RemainingFractionPointer: "/remaining"}}}
	if native {
		declaration.ID = "example.native-diagnostics"
		declaration.Provider.ID = "codex"
		declaration.Provider.AccountNamespace = "codex"
		declaration.Capabilities = manifest.Capabilities{
			Credentials: declaration.Capabilities.Credentials, OAuth: declaration.Capabilities.OAuth,
			Endpoints: declaration.Capabilities.Endpoints,
			Compatibility: &manifest.CompatibilityAdapter{ID: string(providerregistry.ConstructionCodex), Delegates: []string{"construction", "usage"}, Inventory: []manifest.CompatibilityInventoryItem{
				{Delegate: "construction", Classification: "private-stateful", Behavior: "Native Codex request construction"},
				{Delegate: "usage", Classification: "finite-core-primitive", Behavior: "Native Codex quota request", Primitive: "codex-quota"},
			}},
			Operations: []manifest.Operation{{ID: "responses", Kind: "inference", Protocol: "openai-responses", Transport: "websocket-json", Endpoint: "api", Method: http.MethodPost, Path: "/"}},
		}
		for index := range declaration.Capabilities.Endpoints {
			endpoint := &declaration.Capabilities.Endpoints[index]
			if endpoint.ID == "api" {
				endpoint.BaseURL, endpoint.AllowedSchemes, endpoint.AllowedHosts = "wss://native.invalid/responses", []string{"wss"}, []string{"native.invalid"}
			}
		}
		for index := range declaration.Models {
			declaration.Models[index].Reasoning, declaration.Models[index].DefaultOptions = nil, nil
			declaration.Models[index].Modalities.Input = []string{"text"}
		}
	}
	data, err = json.Marshal(declaration)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0600))
	f.registration, err = providerregistry.FromManifest(declaration)
	require.NoError(t, err)
	_, err = providerregistry.New(f.registration)
	require.NoError(t, err)
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	t.Cleanup(manager.Close)
	diagnosis, err := manager.Diagnose(t.Context(), providerplugin.DiagnoseRequest{Source: source})
	require.NoError(t, err)
	require.True(t, diagnosis.Valid, "%+v", diagnosis.Diagnostics)
	installed, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source})
	require.NoError(t, err)
	status := installed.Plugins[0]
	installed, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{Digest: status.Digest, Trusted: true})
	require.NoError(t, err)
	bundles, err := manager.ExportRegisteredBundles(installed.Revision, map[string]string{status.ID: status.Digest})
	require.NoError(t, err)
	bundle, err := providerplugin.ValidateDetachedBundle(bundles[0])
	require.NoError(t, err)
	metadata, err := bundle.Catalog()
	require.NoError(t, err)
	f.token = &oauth.Token{AccessToken: "selected-$LITERAL-access", RefreshToken: "selected-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "saved-client", ClientSecret: "saved-client-secret", AuthURL: "https://auth.invalid/authorize", TokenURL: "https://auth.invalid/token", AuthStyle: 2}}
	f.provider = config.ProviderConfig{ID: declaration.Provider.ID, Name: metadata.Name, Type: metadata.Type, BaseURL: metadata.APIEndpoint, Models: metadata.Models, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: f.registration.Construction, CompatibilityAdapter: f.registration.CompatibilityAdapter}, Plugin: &config.ProviderPluginReference{ID: declaration.ID, Version: declaration.Version}, Configuration: map[string]any{"oauth_client_id": "captured-client"}}
	f.proposal = config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1, Bundles: bundles, Providers: []config.RemoteProviderDefinition{{Config: f.provider, BundleDigest: bundle.Digest()}}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: {Provider: f.provider.ID, Model: f.provider.Models[0].ID}, config.SelectedModelTypeSmall: {Provider: f.provider.ID, Model: f.provider.Models[0].ID}}, Credentials: []config.RemoteCredentialBinding{{Owner: f.registration.Owner(), Generation: 1, OAuthToken: f.token}}}
	if native {
		f.proposal.Providers[0].NativeIdentity = &config.NativeIdentity{UserAgent: "accepted-diagnostic/7.8.9 (CapturedOS 1; fixture) CapturedTerminal", Originator: "accepted-diagnostic", Version: "7.8.9"}
		f.proposal.Credentials[0].OAuthToken = nil
		f.proposal.Credentials[0].Account = &accounts.Entry{ID: "selected-native-account", AccessToken: f.token.AccessToken, RefreshToken: f.token.RefreshToken, ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	}
	sealNamespaceDiagnostic(t, &f.proposal)
	if accepted {
		f.store, err = config.CompileRemoteRuntime(root, filepath.Join(root, "receiver"), false, f.proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"HOME": root, "AI_CLI_DIR": f.ambientRoot, "CODEX_CLI_VERSION": "9.9.9"}))
		require.NoError(t, err)
	} else {
		provider := f.provider
		provider.OAuthToken, provider.APIKey = f.token, f.token.AccessToken
		f.store = config.NewTestStoreWithRegistrations(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider})}, f.registration)
	}
	t.Setenv("AI_CLI_DIR", f.ambientRoot)
	t.Setenv("DIAGNOSTIC_CAPTURED", "hostile-environment")
	t.Setenv("DIAGNOSTIC_ABSENT", "hostile-presence")
	t.Setenv("CODEX_CLI_VERSION", "9.9.9")
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "hostile-diagnostic")
	t.Setenv("TERM_PROGRAM", "HostileTerminal")
	t.Cleanup(func() { f.unchangedAccounts(t) })
	return f
}

func sealNamespaceDiagnostic(t *testing.T, proposal *config.RemoteRuntimeProposal) {
	t.Helper()
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(*proposal)
	require.NoError(t, err)
}

func (f namespaceDiagnosticFixture) unchangedAccounts(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(f.accountRoot)
	require.NoError(t, err)
	require.Equal(t, f.marker, data)
	require.NoDirExists(t, f.ambientRoot)
}

func TestNamespaceDiagnosticsConfiguredAndAcceptedHTTPS(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		t.Run(map[bool]string{false: "configured", true: "accepted"}[accepted], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.NotNil(t, r.TLS)
				assert.Equal(t, "Bearer selected-$LITERAL-access", r.Header.Get("Authorization"))
				assert.Contains(t, []string{"/usage/setup", "/usage/final"}, r.URL.Path)
				_, _ = w.Write([]byte(`{"remaining":0.75}`))
			}))
			t.Cleanup(server.Close)
			f := namespaceDiagnosticStore(t, server, accepted, false)
			require.True(t, f.token.IsExpired(), "diagnostics preserve configured-token behavior without a new expiry veto")
			report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
			require.NoError(t, err)
			require.True(t, report.Valid, "%+v", report)
			require.EqualValues(t, 2, calls.Load())
			require.Equal(t, []CheckResult{{Check: CheckUsage, Status: StatusPassed, Message: "usage operations completed"}}, report.Checks)
			require.Len(t, report.Operations, 2)
			for _, result := range report.Operations {
				require.Equal(t, StatusPassed, result.Status)
				require.Equal(t, http.StatusOK, result.HTTPStatus)
			}
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			for _, private := range []string{f.token.AccessToken, f.token.RefreshToken, f.token.Client.ClientSecret, server.URL, f.accountRoot} {
				require.NotContains(t, string(encoded), private)
			}
		})
	}
}

func TestNamespaceDiagnosticsRejectAccountSelectionAndMissingTokenBeforeHTTPS(t *testing.T) {
	for _, accepted := range []bool{false, true} {
		for _, mode := range []string{"account-id", "account-check", "missing-token", "canceled"} {
			t.Run(map[bool]string{false: "configured", true: "accepted"}[accepted]+"-"+mode, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
				t.Cleanup(server.Close)
				f := namespaceDiagnosticStore(t, server, accepted, false)
				request := Request{ProviderID: f.provider.ID}
				ctx := t.Context()
				switch mode {
				case "account-id":
					request.AccountID = "must-not-be-synthesized"
				case "account-check":
					request.Checks = []Check{CheckAccount}
				case "missing-token":
					if accepted {
						f.proposal.Revision, f.proposal.Credentials[0].Generation = 2, 2
						f.proposal.Credentials[0].OAuthToken = nil
						f.proposal.Credentials[0].Unavailable = true
						sealNamespaceDiagnostic(t, &f.proposal)
						_, err := f.store.ReplaceRemoteRuntime(t.Context(), f.proposal, strings.Repeat("a", 64), 1)
						require.NoError(t, err)
					} else {
						provider, _ := f.store.Config().Providers.Get(f.provider.ID)
						provider.OAuthToken = nil
						f.store.Config().Providers.Set(f.provider.ID, provider)
					}
				case "canceled":
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(ctx)
					cancel()
				}
				report, err := Run(ctx, f.store, request)
				if mode == "account-id" || mode == "account-check" {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
					require.False(t, report.Valid)
				}
				require.Zero(t, calls.Load())
			})
		}
	}
}

type namespaceRoundTripFunc func(*http.Request) (*http.Response, error)

func (f namespaceRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func redirectNamespaceNative(t *testing.T, server *httptest.Server) {
	t.Helper()
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	transport := server.Client().Transport
	http.DefaultClient = &http.Client{Transport: namespaceRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.Host != "chatgpt.com" {
			return nil, errors.New("unexpected diagnostic network destination")
		}
		copy := request.Clone(request.Context())
		value := *request.URL
		copy.URL = &value
		copy.URL.Scheme, copy.URL.Host = target.Scheme, target.Host
		return transport.RoundTrip(copy)
	})}
}

func TestNamespaceDiagnosticsQuotaCredentialAndCapturedEnvironment(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "Bearer selected-$LITERAL-access", r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"remaining":0.5}`))
	}))
	t.Cleanup(server.Close)
	f := namespaceDiagnosticStore(t, server, false, false)
	registration := f.registration.Clone()
	registration.QuotaCredential = providerregistry.QuotaCredentialAccessToken
	quota := registration.Quota
	registration.Quota = func(ctx context.Context, token string) (*oauthusage.Usage, error) {
		value, present := oauth.LookupEnvironment(ctx, "DIAGNOSTIC_CAPTURED")
		require.True(t, present)
		require.Equal(t, "quota-captured", value)
		_, present = oauth.LookupEnvironment(ctx, "DIAGNOSTIC_ABSENT")
		require.False(t, present)
		return quota(ctx, token)
	}
	_, err := providerregistry.New(registration)
	require.NoError(t, err)
	t.Setenv("DIAGNOSTIC_CAPTURED", "quota-captured")
	require.NoError(t, os.Unsetenv("DIAGNOSTIC_ABSENT"))
	t.Setenv("AI_CLI_DIR", f.accountRoot)
	provider, _ := f.store.Config().Providers.Get(f.provider.ID)
	f.store = config.NewTestStoreWithRegistrations(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider})}, registration)
	t.Setenv("DIAGNOSTIC_CAPTURED", "hostile-after-capture")
	t.Setenv("DIAGNOSTIC_ABSENT", "must-not-fallback")
	t.Setenv("AI_CLI_DIR", f.ambientRoot)
	report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
	require.NoError(t, err)
	require.True(t, report.Valid, "%+v", report)
	require.EqualValues(t, 2, calls.Load())
}

type switchingDiagnosticRuntime struct {
	current atomic.Pointer[config.ConfigStore]
}

func (r *switchingDiagnosticRuntime) RuntimeSnapshot() config.RuntimeSnapshot {
	return r.current.Load().RuntimeSnapshot()
}

func (r *switchingDiagnosticRuntime) ValidateActiveProviderOwner(owner providerregistry.RegistrationOwner) error {
	return r.current.Load().ValidateActiveProviderOwner(owner)
}

func TestNamespaceDiagnosticsRejectsLocalTokenEnvironmentAndOwnerDrift(t *testing.T) {
	for _, mode := range []string{"token-client", "environment", "owner"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			runtime := &switchingDiagnosticRuntime{}
			var replacement *config.ConfigStore
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/usage/setup", r.URL.Path)
				runtime.current.Store(replacement)
				_, _ = w.Write([]byte(`{"remaining":0.5}`))
			}))
			t.Cleanup(server.Close)
			f := namespaceDiagnosticStore(t, server, false, false)
			provider, _ := f.store.Config().Providers.Get(f.provider.ID)
			if mode == "token-client" {
				token, client := *provider.OAuthToken, *provider.OAuthToken.Client
				client.ClientSecret = "different-client-registration"
				token.Client, provider.OAuthToken = &client, &token
			}
			if mode == "owner" {
				plugin := *provider.Plugin
				plugin.Version = "2.0.0"
				provider.Plugin = &plugin
			}
			// Recreate the originally captured environment unless this case is
			// specifically testing an accepted environment replacement.
			if mode != "environment" {
				t.Setenv("AI_CLI_DIR", f.accountRoot)
				t.Setenv("DIAGNOSTIC_CAPTURED", "accepted-environment")
				require.NoError(t, os.Unsetenv("DIAGNOSTIC_ABSENT"))
				t.Setenv("CODEX_CLI_VERSION", "1.2.3")
				t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "captured-diagnostic")
				t.Setenv("TERM_PROGRAM", "CapturedTerminal")
			}
			replacement = config.NewTestStoreWithRegistrations(&config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{provider.ID: provider})}, f.registration)
			runtime.current.Store(f.store)
			report, err := Run(t.Context(), runtime, Request{ProviderID: f.provider.ID})
			require.NoError(t, err)
			require.False(t, report.Valid)
			require.EqualValues(t, 1, calls.Load(), "drift must stop before the final usage operation")
		})
	}
}

func TestNamespaceDiagnosticsAcceptedNativeIdentitySurvivesSameOwnerReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	headers := make(chan http.Header, 2)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/backend-api/wham/usage", r.URL.Path)
		headers <- r.Header.Clone()
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"plan_type":"private-plan","rate_limit":{"primary_window":{"used_percent":17,"limit_window_seconds":18000}}}`))
	}))
	t.Cleanup(server.Close)
	f := namespaceDiagnosticStore(t, server, true, true)
	redirectNamespaceNative(t, server)
	type result struct {
		report Report
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
		done <- result{report, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("accepted native quota did not reach HTTPS")
	}
	data, err := json.Marshal(f.proposal)
	require.NoError(t, err)
	var next config.RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &next))
	next.Revision, next.Credentials[0].Generation = 2, 2
	next.Credentials[0].Account.AccessToken = "next-accepted-access"
	next.Providers[0].NativeIdentity = &config.NativeIdentity{UserAgent: "next-diagnostic/2.3.4 (NextOS 1; fixture) NextTerminal", Originator: "next-diagnostic", Version: "2.3.4"}
	sealNamespaceDiagnostic(t, &next)
	_, err = f.store.ReplaceRemoteRuntime(t.Context(), next, strings.Repeat("a", 64), 1)
	close(release)
	require.NoError(t, err)
	first := <-done
	require.NoError(t, first.err)
	require.True(t, first.report.Valid, "%+v", first.report)
	firstHeader := <-headers
	require.Equal(t, "Bearer "+f.token.AccessToken, firstHeader.Get("Authorization"))
	require.Equal(t, f.proposal.Providers[0].NativeIdentity.UserAgent, firstHeader.Get("User-Agent"))
	report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
	require.NoError(t, err)
	require.True(t, report.Valid, "%+v", report)
	secondHeader := <-headers
	require.Equal(t, "Bearer next-accepted-access", secondHeader.Get("Authorization"))
	require.Equal(t, next.Providers[0].NativeIdentity.UserAgent, secondHeader.Get("User-Agent"))
	require.EqualValues(t, 2, calls.Load())
}

func TestNamespaceDiagnosticsAcceptedTokenSurvivesSameOwnerReplacement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	headers := make(chan http.Header, 4)
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotNil(t, r.TLS)
		headers <- r.Header.Clone()
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = w.Write([]byte(`{"remaining":0.75}`))
	}))
	t.Cleanup(server.Close)
	f := namespaceDiagnosticStore(t, server, true, false)
	type result struct {
		report Report
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
		done <- result{report, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("accepted quota did not reach HTTPS")
	}
	data, err := json.Marshal(f.proposal)
	require.NoError(t, err)
	var next config.RemoteRuntimeProposal
	require.NoError(t, json.Unmarshal(data, &next))
	next.Revision, next.Credentials[0].Generation = 2, 2
	next.Credentials[0].OAuthToken.AccessToken = "next-accepted-access"
	next.Credentials[0].OAuthToken.Client.ClientSecret = "next-accepted-client-secret"
	sealNamespaceDiagnostic(t, &next)
	_, err = f.store.ReplaceRemoteRuntime(t.Context(), next, strings.Repeat("a", 64), 1)
	close(release)
	require.NoError(t, err)
	first := <-done
	require.NoError(t, first.err)
	require.True(t, first.report.Valid, "%+v", first.report)
	for range 2 {
		require.Equal(t, "Bearer "+f.token.AccessToken, (<-headers).Get("Authorization"))
	}
	report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
	require.NoError(t, err)
	require.True(t, report.Valid, "%+v", report)
	for range 2 {
		require.Equal(t, "Bearer next-accepted-access", (<-headers).Get("Authorization"))
	}
	require.EqualValues(t, 4, calls.Load())
}

func TestNamespaceDiagnosticsAcceptedExactOwnerReplacementStopsAfterSetup(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.NotNil(t, r.TLS)
		assert.Equal(t, "/usage/setup", r.URL.Path)
		calls.Add(1)
		close(entered)
		<-release
		_, _ = w.Write([]byte(`{"remaining":0.75}`))
	}))
	t.Cleanup(server.Close)
	f := namespaceDiagnosticStore(t, server, true, false)
	replacement := namespaceDiagnosticStore(t, server, true, false, "2.0.0")
	require.NotEqual(t, f.registration.Owner(), replacement.registration.Owner())
	type result struct {
		report Report
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := Run(t.Context(), f.store, Request{ProviderID: f.provider.ID})
		done <- result{report, err}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("accepted quota did not reach HTTPS")
	}
	next := replacement.proposal
	next.Revision, next.Credentials[0].Generation = 2, 2
	sealNamespaceDiagnostic(t, &next)
	_, err := f.store.ReplaceRemoteRuntime(t.Context(), next, strings.Repeat("a", 64), 1)
	close(release)
	require.NoError(t, err)
	first := <-done
	require.NoError(t, first.err)
	require.False(t, first.report.Valid)
	require.EqualValues(t, 1, calls.Load(), "owner replacement must prevent the final usage operation")
	require.Equal(t, StatusNotReached, first.report.Operations[1].Status)
}

func TestNamespaceDiagnosticsAcceptedAccountNeverFallsBackToReceiverStorage(t *testing.T) {
	for _, mode := range []string{"unselected-native-without-forwarded-account", "different-account-selection"} {
		t.Run(mode, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				_, _ = w.Write([]byte(`{"plan_type":"receiver-plan","rate_limit":{"primary_window":{"used_percent":17,"limit_window_seconds":18000}}}`))
			}))
			t.Cleanup(server.Close)
			f := namespaceDiagnosticStore(t, server, true, true)
			request := Request{ProviderID: f.provider.ID}
			if mode == "unselected-native-without-forwarded-account" {
				selected := namespaceDiagnosticStore(t, server, true, false)
				next := f.proposal
				next.Revision, next.Credentials[0].Generation = 2, 2
				next.Credentials[0].Account = nil
				// Native construction requires an OAuth account when selected for
				// inference. Keep Codex admitted but select the finite provider's
				// models; diagnostics still explicitly targets the Codex owner.
				next.Bundles = append(next.Bundles, selected.proposal.Bundles...)
				next.Providers = append(next.Providers, selected.proposal.Providers...)
				next.Credentials = append(next.Credentials, selected.proposal.Credentials...)
				next.Models = selected.proposal.Models
				sealNamespaceDiagnostic(t, &next)
				_, err := f.store.ReplaceRemoteRuntime(t.Context(), next, strings.Repeat("a", 64), 1)
				require.NoError(t, err)
			} else {
				request.AccountID = "receiver-selected-account"
			}
			redirectNamespaceNative(t, server)
			// A real receiver account would satisfy the old Active/List fallback.
			// Its presence must never replace the admitted credential selection.
			t.Setenv("AI_CLI_DIR", t.TempDir())
			require.NoError(t, accounts.SaveForOwner(t.Context(), "codex", accounts.Entry{
				ID: "receiver-selected-account", AccessToken: "receiver-private-access", RefreshToken: "receiver-private-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(),
			}, func() error { return f.store.ValidateActiveProviderOwner(f.registration.Owner()) }))
			path, err := accounts.Path()
			require.NoError(t, err)
			before, err := os.ReadFile(path)
			require.NoError(t, err)
			info, err := os.Stat(path)
			require.NoError(t, err)
			report, err := Run(t.Context(), f.store, request)
			require.NoError(t, err)
			require.False(t, report.Valid)
			require.False(t, report.Account.Loaded)
			require.Equal(t, CheckAccount, report.Checks[0].Check)
			require.Equal(t, StatusFailed, report.Checks[0].Status)
			require.Zero(t, calls.Load())
			after, err := os.ReadFile(path)
			require.NoError(t, err)
			afterInfo, err := os.Stat(path)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Equal(t, info.ModTime(), afterInfo.ModTime())
			require.Equal(t, info.Mode(), afterInfo.Mode())
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "receiver-private")
		})
	}
}

func TestNamespaceDiagnosticsReports401AndCancelsActualHTTPS(t *testing.T) {
	for _, test := range []struct {
		name     string
		mode     string
		accepted bool
	}{
		{name: "configured-unauthorized", mode: "unauthorized"},
		{name: "configured-cancel", mode: "cancel"},
		{name: "accepted-unauthorized", mode: "unauthorized", accepted: true},
		{name: "accepted-cancel", mode: "cancel", accepted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				assert.Equal(t, "/usage/setup", r.URL.Path)
				if test.mode == "unauthorized" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				close(entered)
				<-r.Context().Done()
				close(canceled)
			}))
			t.Cleanup(server.Close)
			f := namespaceDiagnosticStore(t, server, test.accepted, false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan Report, 1)
			go func() {
				report, err := Run(ctx, f.store, Request{ProviderID: f.provider.ID})
				assert.NoError(t, err)
				done <- report
			}()
			if test.mode == "cancel" {
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("quota request did not reach HTTPS")
				}
				cancel()
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("cancellation did not reach HTTPS")
				}
			}
			report := <-done
			require.False(t, report.Valid)
			require.Equal(t, StatusFailed, report.Checks[0].Status)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}
