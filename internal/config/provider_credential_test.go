package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestProviderOAuthLiteralConnectionAndExpressions(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "command-must-not-run")
	literal := "synthetic-$(touch " + marker + ")-$CRUX_TEST_CREDENTIAL"
	received := make(chan string, 3)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer host.Close()
	old := http.DefaultClient
	http.DefaultClient = host.Client()
	defer func() { http.DefaultClient = old }()
	resolver := NewShellVariableResolver(env.NewFromMap(map[string]string{"CRUX_TEST_CREDENTIAL": "resolved-expression"}))
	provider := ProviderConfig{ID: "fixture", Type: catalog.TypeOpenAICompat, BaseURL: host.URL, APIKey: literal, OAuthToken: &oauth.Token{AccessToken: literal}, Owner: &ProviderOwnerReference{Type: ProviderOwnerCustom, Construction: providerregistry.ConstructionOpenAICompat}}
	require.NoError(t, provider.TestConnection(t.Context(), resolver, func() error { return nil }))
	require.Equal(t, "Bearer "+literal, <-received)
	require.NoFileExists(t, marker)
	for _, token := range []*oauth.Token{nil, {AccessToken: "different-token"}} {
		provider.APIKey = "$CRUX_TEST_CREDENTIAL"
		provider.OAuthToken = token
		require.NoError(t, provider.TestConnection(t.Context(), resolver, func() error { return nil }))
		require.Equal(t, "Bearer resolved-expression", <-received)
	}
}

func TestProviderOAuthLiteralLoadReload(t *testing.T) {
	marker := ""
	literal := ""
	store, root, _ := authenticationBasisStore(t, func(root string, values map[string]string) {
		marker = filepath.Join(root, "must-not-run")
		literal = "synthetic-$(touch " + marker + ")-$UNSET"
		path := filepath.Join(root, "crux.json")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		data, err = sjson.SetBytes(data, "providers.fixture.api_key", literal)
		require.NoError(t, err)
		data, err = sjson.SetBytes(data, "providers.fixture.oauth", &oauth.Token{AccessToken: literal})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	})
	assertKey := func() {
		p, ok := store.Config().Providers.Get("fixture")
		require.True(t, ok)
		require.Equal(t, literal, p.APIKey)
		require.Empty(t, p.APIKeyTemplate)
		require.NoFileExists(t, marker)
	}
	assertKey()
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	assertKey()
	// Reload is an explicit fresh account-authority boundary, while failed reload
	// keeps the old accepted object. No account map may leak to a new generation.
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	owner, ok := store.Config().ProviderOwner("fixture")
	require.True(t, ok)
	next := runtimeAuthenticationCandidate(t, store, capture, owner, nil)
	store.setConfig(next)
	marked := store.Config()
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), []byte(`{"providers":`), 0o600))
	require.Error(t, store.ReloadFromDisk(t.Context()))
	require.Same(t, marked, store.Config())
	data, err := json.Marshal(&Config{Providers: marked.Providers, Models: marked.Models, Options: marked.Options})
	require.NoError(t, err)
	// Restore a usable provider without carrying the private in-memory authority.
	data, err = sjson.SetBytes(data, "providers.fixture.api_key", "new-literal")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "crux.json"), data, 0o600))
	require.NoError(t, store.ReloadFromDisk(t.Context()))
	require.Nil(t, store.Config().authenticationAccounts)
}

func TestProviderOAuthLiteralLoaderDiscoveryHTTPS(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "must-not-run")
	literal := "synthetic-$(touch " + marker + ")-$UNSET"
	received := make(chan string, 1)
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"main"},{"id":"small"}]}`))
	}))
	defer host.Close()
	old := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	defer func() { http.DefaultTransport = old }()
	store, _, _ := authenticationBasisStore(t, func(root string, _ map[string]string) {
		path := filepath.Join(root, "crux.json")
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		for key, value := range map[string]any{"providers.fixture.base_url": host.URL, "providers.fixture.api_key": literal, "providers.fixture.oauth": &oauth.Token{AccessToken: literal}} {
			data, err = sjson.SetBytes(data, key, value)
			require.NoError(t, err)
		}
		data, err = sjson.DeleteBytes(data, "providers.fixture.models")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))
	})
	require.Equal(t, "Bearer "+literal, <-received)
	require.NoFileExists(t, marker)
	p, ok := store.Config().Providers.Get("fixture")
	require.True(t, ok)
	require.Len(t, p.Models, 2)
	require.Equal(t, literal, p.APIKey)
	require.Empty(t, p.APIKeyTemplate)
}

func TestProviderOAuthLiteralKnownProviderAndCollection(t *testing.T) {
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, t.TempDir())
	}
	marker := filepath.Join(t.TempDir(), "must-not-run")
	literal := "synthetic-$(touch " + marker + ")-$UNSET"
	entry := accounts.Entry{ID: "selected", AccessToken: literal, RefreshToken: "synthetic-refresh"}
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, entry))
	store := NewTestStore(&Config{Options: &Options{}, Providers: csync.NewMapFrom(map[string]ProviderConfig{"codex": {
		ID: "codex", APIKey: literal, OAuthToken: entry.Token(), Owner: &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionCodex},
	}}), Models: map[SelectedModelType]SelectedModel{SelectedModelTypeLarge: {Provider: "codex", Model: "gpt-5.6"}, SelectedModelTypeSmall: {Provider: "codex", Model: "gpt-5.6"}}})
	catalogProvider := catalog.Provider{ID: "codex", Name: "Codex", Type: catalog.TypeOpenAICompat, APIEndpoint: "https://example.invalid/v1", APIKey: "$UNSET", Models: []catalog.Model{{ID: "gpt-5.6", ContextWindow: 32000, DefaultMaxTokens: 100}}}
	require.NoError(t, store.Config().configureProvidersWithMigration(t.Context(), store, env.New(), store.resolver, []catalog.Provider{catalogProvider}, nil))
	p, ok := store.Config().Providers.Get("codex")
	require.True(t, ok)
	require.Equal(t, literal, p.APIKey)
	require.Empty(t, p.APIKeyTemplate)
	require.NoFileExists(t, marker)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	require.Len(t, proposal.Credentials, 1)
	require.NotNil(t, proposal.Credentials[0].Account)
	require.Equal(t, literal, proposal.Credentials[0].Account.AccessToken)
	require.NoFileExists(t, marker)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderCodex, accounts.Entry{ID: "peer", AccessToken: "changed"}))
	_, err = store.CollectRemoteRuntime(t.Context(), 2)
	require.ErrorContains(t, err, "selected client account changed")
}
