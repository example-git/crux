package config

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func authenticationCOWOAuthStore(t *testing.T, providerID string) (*ConfigStore, providerregistry.RegistrationOwner, accounts.Entry) {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfilePluginCompat)}
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	for _, dir := range []string{"config", "data", "project"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0o700))
	}
	registry, err := providerregistry.New(registrytest.Registrations()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup(providerID)
	require.True(t, ok)
	if registration.Manifest != nil {
		require.NoError(t, registrytest.Install(t.Context(), values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"], *registration.Manifest))
	}
	entry := accounts.Entry{ID: "selected", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli(), Raw: json.RawMessage(`{"account_id":"retained-account"}`)}
	credentials := map[string]any{}
	if providerID == "codex" {
		require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, entry))
		credentials = map[string]any{"api_key": entry.AccessToken, "oauth": entry.Token()}
	}
	source, err := json.Marshal(map[string]any{"providers": map[string]any{providerID: map[string]any{"owner": providerOwnerReferenceForRegistration(registration), "models": []map[string]any{{"id": "gpt-5.6"}}}}, "models": map[string]any{"large": map[string]any{"provider": providerID, "model": "gpt-5.6"}, "small": map[string]any{"provider": providerID, "model": "gpt-5.6"}}})
	require.NoError(t, err)
	if registration.Manifest != nil {
		source, err = sjson.SetBytes(source, "providers."+providerID+".plugin", ProviderPluginReference{ID: registration.Manifest.ID, Version: registration.Manifest.Version})
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), source, 0o600))
	data, err := json.Marshal(map[string]any{"providers": map[string]any{providerID: credentials}, "foreign": json.RawMessage(`9007199254740993`)})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "data", "crux.json"), data, 0o600))
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(filepath.Join(root, "data"), alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	store, err := LoadIsolated(filepath.Join(root, "project"), alias, false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner(providerID)
	require.True(t, ok)
	if providerID == "codex" {
		active, err := accounts.Active(t.Context(), owner.AccountNamespace)
		require.NoError(t, err)
		require.NotNil(t, active)
		entry = *active
	}
	return store, owner, entry
}

func authenticationCOWHTTPS(t *testing.T, expected string) (func(context.Context, string) (*oauth.Token, error), *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var got string
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != expected {
			t.Errorf("unexpected synthetic exchange input")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(selectedRefreshToken())
	}))
	t.Cleanup(server.Close)
	return func(ctx context.Context, token string) (*oauth.Token, error) {
		data, err := json.Marshal(token)
		if err != nil {
			return nil, err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, bytes.NewReader(data))
		if err != nil {
			return nil, err
		}
		response, err := server.Client().Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		var result oauth.Token
		err = json.NewDecoder(response.Body).Decode(&result)
		return &result, err
	}, &calls
}

func TestAuthenticationBasisSelectedRefreshAndRemovalAliases(t *testing.T) {
	store, owner, entry := authenticationCOWOAuthStore(t, "codex")
	before, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	marked := store.Config().cloneForWrite()
	require.NoError(t, before.finalizeRuntimeAuthenticationAccounts(marked, owner, &entry))
	store.setConfig(marked)
	oldAuthority := marked.authenticationAccounts
	exchange, calls := authenticationCOWHTTPS(t, entry.RefreshToken)
	store.exchangeToken = func(ctx context.Context, _ string, refresh string) (*oauth.Token, error) {
		return exchange(ctx, refresh)
	}
	fresh, err := store.RefreshSelectedOAuthAccount(t.Context(), ScopeGlobal, owner, entry, true)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	captured, retained, err := store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.True(t, retained)
	require.Equal(t, fresh, captured)
	require.NotSame(t, oldAuthority, store.Config().authenticationAccounts)
	require.Equal(t, entry, *oldAuthority.entries[owner])
	capture, layers := authenticationBasisCapture(t, store, store.baseEnvironment)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
	authority := store.Config().authenticationAccounts
	require.NoError(t, store.SetCompactMode(ScopeGlobal, true))
	require.Same(t, authority, store.Config().authenticationAccounts)
	require.NoError(t, store.RemoveProviderCredentials(ScopeGlobal, owner))
	provider, _ := store.Config().Providers.Get(owner.ProviderID)
	require.Empty(t, provider.APIKey)
	require.Nil(t, provider.OAuthToken)
	require.Same(t, authority, store.Config().authenticationAccounts, "credential removal must not invent another selected account")
	capture, layers = authenticationBasisCapture(t, store, store.baseEnvironment)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
}

func TestAuthenticationBasisCopilotImportAlias(t *testing.T) {
	store, owner, _ := authenticationCOWOAuthStore(t, "copilot")
	exchange, calls := authenticationCOWHTTPS(t, "synthetic-import")
	registrations := store.providerRegistry.Registrations()
	for i := range registrations {
		if registrations[i].ProviderID == owner.ProviderID {
			registrations[i].OAuth.Import = func(ctx context.Context) (*oauth.Token, bool, error) {
				token, err := exchange(ctx, "synthetic-import")
				return token, err == nil, err
			}
		}
	}
	registry, err := providerregistry.New(registrations...)
	require.NoError(t, err)
	next := store.Config().cloneForWrite()
	scan := cloneProviderScan(*next.providerScan)
	scan.Registry = registry
	next.bindProviderScan(scan)
	store.providerRegistry = registry
	store.setConfig(next)
	token, imported, err := store.ImportCopilotForOwner(t.Context(), owner)
	require.NoError(t, err)
	require.True(t, imported)
	require.Equal(t, int32(1), calls.Load())
	active, err := accounts.Active(t.Context(), owner.AccountNamespace)
	require.NoError(t, err)
	require.NotNil(t, active)
	require.Equal(t, token.AccessToken, active.AccessToken)
	capture, layers := authenticationBasisCapture(t, store, store.baseEnvironment)
	require.NoError(t, capture.validateConfigBasis(layers, ""))
}
