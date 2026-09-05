package providerdiagnostics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
)

const (
	diagnosticProviderID = "diagnostic-provider"
	diagnosticPluginID   = "diagnostic.plugin"
	diagnosticVersion    = "1.0.0"
	diagnosticNamespace  = "diagnostic-accounts"
	diagnosticAccountID  = "private-account-id"
	diagnosticToken      = "private-access-token"
)

type invalidatingRuntime struct {
	store    *config.ConfigStore
	rejectAt int32
	calls    atomic.Int32
}

func (r *invalidatingRuntime) RuntimeSnapshot() config.RuntimeSnapshot {
	return r.store.RuntimeSnapshot()
}

func (r *invalidatingRuntime) ValidateActiveProviderOwner(owner providerregistry.RegistrationOwner) error {
	if r.calls.Add(1) >= r.rejectAt {
		return errors.New("exact owner changed")
	}
	return r.store.ValidateActiveProviderOwner(owner)
}

func TestRunExecutesSelectedAccountAndEveryManifestUsageOperation(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	var requestMu sync.Mutex
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestMu.Lock()
		requests = append(requests, request.Method+" "+request.URL.Path)
		requestMu.Unlock()
		if request.Header.Get("Authorization") != "Bearer "+diagnosticToken {
			t.Errorf("authorization header was not derived from the selected account")
		}
		switch request.URL.Path {
		case "/account":
			_, _ = writer.Write([]byte(`{"id":"private-account-id","display_name":"private display","private":"private-account-response"}`))
		case "/usage/setup":
			_, _ = writer.Write([]byte(`{"project":{"id":"private-project"},"private":"private-setup-response"}`))
		case "/usage/final":
			var body any
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode final usage request: %v", err)
			}
			if project := fmt.Sprint(jsonPointer(body, "project")); project != "private-project" {
				t.Errorf("final usage project = %q", project)
			}
			_, _ = writer.Write([]byte(`{"windows":[{"remaining":0.5}],"private":"private-final-response"}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)

	store, _ := diagnosticPluginStore(t, server.URL)

	report, err := Run(t.Context(), store, Request{ProviderID: diagnosticProviderID, AccountID: diagnosticAccountID})
	require.NoError(t, err)
	require.True(t, report.Valid)
	require.Equal(t, AccountResult{Loaded: true, Source: "stored"}, report.Account)
	require.Equal(t, []CheckResult{
		{Check: CheckAccount, Status: StatusPassed, Message: "authenticated account loaded"},
		{Check: CheckUsage, Status: StatusPassed, Message: "usage operations completed"},
	}, report.Checks)
	require.Len(t, report.Operations, 3)
	require.Equal(t, []OperationResult{
		{Path: "/capabilities/operations/1", Kind: "account", Status: StatusPassed, HTTPStatus: http.StatusOK, DurationMS: report.Operations[0].DurationMS, Message: "request completed"},
		{Path: "/capabilities/operations/2", Kind: "custom", Status: StatusPassed, HTTPStatus: http.StatusOK, DurationMS: report.Operations[1].DurationMS, Message: "request completed"},
		{Path: "/capabilities/operations/3", Kind: "usage", Status: StatusPassed, HTTPStatus: http.StatusOK, DurationMS: report.Operations[2].DurationMS, Message: "request completed"},
	}, report.Operations)
	requestMu.Lock()
	require.Equal(t, []string{"GET /account", "POST /usage/setup", "POST /usage/final"}, requests)
	requestMu.Unlock()

	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	for _, private := range []string{diagnosticAccountID, diagnosticToken, server.URL, "private display", "private-project", "private-account-response", "private-setup-response", "private-final-response", "private-account-metadata", "account-identity", "usage-setup", "usage-final"} {
		require.NotContains(t, string(encoded), private)
	}
}

func TestRunRejectsOwnerReplacementBeforeAnyDiagnosticRequest(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)
	store, _ := diagnosticPluginStore(t, server.URL)
	runtime := &invalidatingRuntime{store: store, rejectAt: 3}

	report, err := Run(t.Context(), runtime, Request{ProviderID: diagnosticProviderID})
	require.NoError(t, err)
	require.False(t, report.Valid)
	require.Zero(t, requests.Load())
	for _, operation := range report.Operations {
		require.Equal(t, StatusNotReached, operation.Status)
	}
}

func TestRunReportsFailedUsageSetupAndUnreachedFinalOperation(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	var finalRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/account":
			_, _ = writer.Write([]byte(`{"id":"private-account-id"}`))
		case "/usage/setup":
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = writer.Write([]byte(`{"error":"private-provider-error"}`))
		case "/usage/final":
			finalRequests.Add(1)
			writer.WriteHeader(http.StatusOK)
		}
	}))
	t.Cleanup(server.Close)
	store, _ := diagnosticPluginStore(t, server.URL)

	report, err := Run(t.Context(), store, Request{ProviderID: diagnosticProviderID})
	require.NoError(t, err)
	require.False(t, report.Valid)
	require.Zero(t, finalRequests.Load())
	require.Len(t, report.Operations, 3)
	require.Equal(t, StatusPassed, report.Operations[0].Status)
	require.Equal(t, StatusFailed, report.Operations[1].Status)
	require.Equal(t, http.StatusUnauthorized, report.Operations[1].HTTPStatus)
	require.Equal(t, StatusNotReached, report.Operations[2].Status)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "private-provider-error")
	require.NotContains(t, string(encoded), server.URL)
}

func TestRunRefreshesSelectedAccountBeforeManifestRequests(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	expiredAccess := "private-expired-access"
	refreshToken := "private-refresh-token"
	var refreshes atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		require.Equal(t, "Bearer "+diagnosticToken, request.Header.Get("Authorization"))
		switch request.URL.Path {
		case "/account":
			_, _ = writer.Write([]byte(`{"id":"private-account-id"}`))
		case "/usage/setup":
			_, _ = writer.Write([]byte(`{"project":{"id":"private-project"}}`))
		case "/usage/final":
			_, _ = writer.Write([]byte(`{"windows":[{"remaining":0.5}]}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	store, registration := diagnosticPluginStoreWithRefresh(t, server.URL, func(ctx context.Context, value string) (*oauth.Token, error) {
		require.NoError(t, ctx.Err())
		require.Equal(t, refreshToken, value)
		refreshes.Add(1)
		return &oauth.Token{AccessToken: diagnosticToken, RefreshToken: refreshToken, ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
	})
	require.NoError(t, accounts.SaveForOwner(t.Context(), diagnosticNamespace, accounts.Entry{
		ID: diagnosticAccountID, AccessToken: expiredAccess, RefreshToken: refreshToken, ExpiresAt: time.Now().Add(-time.Hour).UnixMilli(),
	}, func() error { return store.ValidateActiveProviderOwner(registration.Owner()) }))

	report, err := Run(t.Context(), store, Request{ProviderID: diagnosticProviderID, AccountID: diagnosticAccountID})
	require.NoError(t, err)
	require.True(t, report.Valid)
	require.Equal(t, int32(1), refreshes.Load())
	require.Equal(t, int32(3), requests.Load())
	stored, err := accounts.Active(t.Context(), diagnosticNamespace)
	require.NoError(t, err)
	require.Equal(t, diagnosticToken, stored.AccessToken)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), expiredAccess)
	require.NotContains(t, string(encoded), refreshToken)
	require.NotContains(t, string(encoded), diagnosticToken)
}

func TestRunValidatesExactPresetAPIKeyWithoutNetworkFallback(t *testing.T) {
	providerID := string(catalog.ProviderAlibabaSingapore)
	presetID, version, digest, ok := providerplugin.CanonicalMigratedProviderPreset(providerID)
	require.True(t, ok)
	preset := config.ProviderPresetReference{ID: presetID, Version: version, Digest: digest}

	for _, test := range []struct {
		name  string
		key   string
		valid bool
	}{
		{name: "valid", key: "sk-private-valid", valid: true},
		{name: "invalid", key: "private-invalid", valid: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := config.ProviderConfig{
				ID: providerID, Type: catalog.TypeOpenAICompat, APIKey: test.key,
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerPreset, Construction: providerregistry.ConstructionOpenAICompat},
				Preset: &preset,
			}
			cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{providerID: provider})}
			store := config.NewTestStoreWithProviderGeneration(cfg, map[string]config.ProviderPresetReference{providerID: preset})

			report, err := Run(t.Context(), store, Request{ProviderID: providerID})
			require.NoError(t, err)
			require.Equal(t, test.valid, report.Valid)
			require.Equal(t, test.valid, report.Checks[0].Status == StatusPassed)
			require.Equal(t, test.valid, report.Operations[0].Status == StatusPassed)
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), test.key)
		})
	}
}

func diagnosticPluginStore(t *testing.T, serverURL string) (*config.ConfigStore, providerregistry.Registration) {
	t.Helper()
	return diagnosticPluginStoreWithRefresh(t, serverURL, nil)
}

func diagnosticPluginStoreWithRefresh(t *testing.T, serverURL string, refresh func(context.Context, string) (*oauth.Token, error)) (*config.ConfigStore, providerregistry.Registration) {
	t.Helper()
	target, err := url.Parse(serverURL)
	require.NoError(t, err)
	credential := "diagnostic-token"
	endpoint := manifest.Endpoint{
		ID: "api", BaseURL: serverURL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}, Override: "forbidden", Credential: credential,
	}
	authorization := manifest.Template{Kind: "concat", Parts: []manifest.Template{{Kind: "literal", Value: "Bearer "}, {Kind: "credential", Ref: credential}}}
	value := manifest.Manifest{
		ManifestVersion: 1,
		ID:              diagnosticPluginID,
		Version:         diagnosticVersion,
		Provider: manifest.Provider{
			ID: diagnosticProviderID, Name: "Diagnostic Provider", AccountNamespace: diagnosticNamespace,
		},
		Capabilities: manifest.Capabilities{
			Credentials: []manifest.Credential{{ID: credential, Kind: "oauth2"}},
			Endpoints:   []manifest.Endpoint{endpoint},
			JSONTransforms: map[string]manifest.JSONPipeline{
				"setup-request": {MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/probe", Value: &manifest.Template{Kind: "literal", Value: "private-request-value"}}}},
				"usage-request": {MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/project", Value: &manifest.Template{Kind: "context", Ref: "usage.project"}}}},
			},
			Operations: []manifest.Operation{
				{ID: "inference", Kind: "inference", Protocol: string(providerregistry.ConstructionGenericJSON), Transport: "http-json", Endpoint: endpoint.ID, Method: http.MethodPost, Path: "/inference"},
				{ID: "account-identity", Kind: "account", Protocol: "generic-json", Transport: "http-json", Endpoint: endpoint.ID, Method: http.MethodGet, Path: "/account", Headers: []manifest.HeaderRule{{Operation: "set", Name: "Authorization", Value: &authorization}}},
				{ID: "usage-setup", Kind: "custom", Protocol: "generic-json", Transport: "http-json", Endpoint: endpoint.ID, Method: http.MethodPost, Path: "/usage/setup", RequestTransform: "setup-request"},
				{ID: "usage-final", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: endpoint.ID, Method: http.MethodPost, Path: "/usage/final", RequestTransform: "usage-request"},
			},
			Usage: &manifest.UsagePolicy{
				Setup:     []manifest.UsageSetup{{Operation: "usage-setup", Extract: []manifest.UsageContextExtraction{{Context: "usage.project", Pointer: "/project/id"}}}},
				Operation: "usage-final", Source: "operation", Fallback: "unavailable",
				Windows: []manifest.WindowMap{{ID: "usage", RemainingFractionPointer: "/windows/0/remaining"}},
			},
		},
	}
	registration, err := providerregistry.FromManifest(value)
	require.NoError(t, err)
	if refresh != nil {
		registration.OAuth = &providerregistry.OAuthCapability{Refresh: refresh}
	}
	_, err = providerregistry.New(registration)
	require.NoError(t, err)
	provider := config.ProviderConfig{
		ID: diagnosticProviderID, Type: catalog.TypeOpenAICompat,
		Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction, CompatibilityAdapter: registration.CompatibilityAdapter},
		Plugin: &config.ProviderPluginReference{ID: diagnosticPluginID, Version: diagnosticVersion},
	}
	cfg := &config.Config{Providers: csync.NewMapFrom(map[string]config.ProviderConfig{diagnosticProviderID: provider})}
	store := config.NewTestStoreWithRegistrations(cfg, registration)
	require.NoError(t, accounts.SaveForOwner(t.Context(), diagnosticNamespace, accounts.Entry{
		ID: diagnosticAccountID, AccessToken: diagnosticToken, Raw: json.RawMessage(`{"private":"private-account-metadata"}`),
	}, func() error { return store.ValidateActiveProviderOwner(registration.Owner()) }))
	return store, registration
}

func jsonPointer(document any, key string) any {
	value, _ := document.(map[string]any)
	return value[key]
}
