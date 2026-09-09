package config

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/foundation/catalog"
	"github.com/example-git/crux/internal/csync"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/gemini"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type selectedGeminiIdentityTransport func(*http.Request) (*http.Response, error)

func (f selectedGeminiIdentityTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func selectedGeminiIdentityStore(t *testing.T, project string) (*ConfigStore, providerregistry.RegistrationOwner, accounts.Entry, map[string]string) {
	t.Helper()
	root := t.TempDir()
	t.Setenv("AI_CLI_DIR", root)
	values := map[string]string{
		"AI_CLI_DIR": root, "HOME": root, "USERPROFILE": root,
		"GEMINI_PROJECT_ID": project, "GEMINI_OAUTH_CLIENT_ID": "captured-gemini-client",
		"GEMINI_OAUTH_CLIENT_SECRET": "captured-gemini-secret", "ANTIGRAVITY_CLI_VERSION": "captured-version",
	}
	expired := &oauth.Token{AccessToken: "selected-old-access", RefreshToken: "selected-old-refresh", ExpiresIn: 3600, ExpiresAt: time.Now().Add(-time.Hour).Unix()}
	entry := accounts.FromToken("selected-gemini", "Selected Gemini", expired, nil)
	require.NoError(t, accounts.Save(t.Context(), accounts.ProviderGemini, entry))
	provider := ProviderConfig{ID: gemini.ID, Name: "Selected Gemini", APIKey: expired.AccessToken, OAuthToken: expired,
		BaseURL: gemini.APIEndpoint, Type: catalog.TypeOpenAICompat,
		Owner:  &ProviderOwnerReference{Type: ProviderOwnerCore, Construction: providerregistry.ConstructionGeminiAntigravity},
		Models: []catalog.Model{{ID: "fixture", Name: "Fixture"}},
	}
	cfg := &Config{Providers: csync.NewMap[string, ProviderConfig](), Models: map[SelectedModelType]SelectedModel{
		SelectedModelTypeLarge: {Provider: gemini.ID, Model: "fixture"}, SelectedModelTypeSmall: {Provider: gemini.ID, Model: "fixture"},
	}}
	cfg.setDefaults(root, filepath.Join(root, "data"))
	cfg.Providers.Set(gemini.ID, provider)
	store := NewTestStore(cfg)
	store.workingDir, store.globalDataPath = root, filepath.Join(root, "config.json")
	store.baseEnvironment = env.NewFromMap(values)
	store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
	store.resolver = IdentityResolver()
	data, err := json.Marshal(map[string]any{"providers": map[string]any{gemini.ID: provider}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.globalDataPath, data, 0o600))
	owner, ok := store.RuntimeSnapshot().ProviderOwner(gemini.ID)
	require.True(t, ok)
	return store, owner, entry, values
}

func TestSelectedGeminiRefreshUsesCapturedEnvironmentAndPersists(t *testing.T) {
	for _, name := range []string{"nonempty-project", "blank-project", "changed-project", "changed-client-id", "changed-version", "missing-client-credentials", "project-changed-during-exchange"} {
		t.Run(name, func(t *testing.T) {
			project := "captured-project"
			if name == "blank-project" || name == "missing-client-credentials" {
				project = ""
			}
			store, owner, original, values := selectedGeminiIdentityStore(t, project)
			if name == "missing-client-credentials" {
				delete(values, "GEMINI_OAUTH_CLIENT_ID")
				delete(values, "GEMINI_OAUTH_CLIENT_SECRET")
				store.baseEnvironment = env.NewFromMap(values)
				store.effectiveEnvironment = cloneEnvironment(store.baseEnvironment)
			}
			admitted := store.RuntimeSnapshot()
			definition, definitionOwner, err := admitted.ClientProviderDefinition(gemini.ID)
			require.NoError(t, err)
			require.Equal(t, owner, definitionOwner)
			require.NotNil(t, definition.GeminiProjectID)
			require.Equal(t, project, *definition.GeminiProjectID)
			require.NotNil(t, definition.NativeIdentity)
			require.Contains(t, definition.NativeIdentity.UserAgent, "antigravity/cli/captured-version ")
			before, err := os.ReadFile(store.globalDataPath)
			require.NoError(t, err)
			changeCaptured := func(key string) {
				changed := maps.Clone(values)
				changed[key] = "changed-captured-value"
				store.writeMu.Lock()
				defer store.writeMu.Unlock()
				store.configMu.Lock()
				defer store.configMu.Unlock()
				store.effectiveEnvironment = env.NewFromMap(changed)
				store.publishConfigLocked(store.config.cloneForWrite())
			}
			var exchanges, metadata atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				assert.NotNil(t, r.TLS)
				assert.Equal(t, http.MethodPost, r.Method)
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/token":
					exchanges.Add(1)
					require.NoError(t, r.ParseForm())
					assert.Equal(t, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {original.RefreshToken}, "client_id": {"captured-gemini-client"}, "client_secret": {"captured-gemini-secret"}}, r.PostForm)
					assert.True(t, strings.HasPrefix(r.Header.Get("User-Agent"), "Go-http-client/"))
					if name == "project-changed-during-exchange" {
						changeCaptured("GEMINI_PROJECT_ID")
					}
					_, _ = io.WriteString(w, `{"access_token":"selected-fresh-access","refresh_token":"selected-rotated-refresh","expires_in":3600}`)
				case "/v1internal:loadCodeAssist":
					metadata.Add(1)
					assert.Equal(t, "Bearer selected-fresh-access", r.Header.Get("Authorization"))
					assert.Equal(t, definition.NativeIdentity.UserAgent, r.Header.Get("User-Agent"))
					_, _ = io.WriteString(w, `{"cloudaicompanionProject":"credential-project"}`)
				default:
					t.Errorf("unexpected HTTPS path %s", r.URL.Path)
					http.Error(w, "unexpected request", http.StatusBadRequest)
				}
			}))
			defer host.Close()
			target, err := url.Parse(host.URL)
			require.NoError(t, err)
			previous := http.DefaultClient
			http.DefaultClient = &http.Client{Transport: selectedGeminiIdentityTransport(func(r *http.Request) (*http.Response, error) {
				if !(r.URL.Scheme == "https" && (r.URL.Host == "oauth2.googleapis.com" && r.URL.Path == "/token" || r.URL.Host == "daily-cloudcode-pa.googleapis.com" && r.URL.Path == "/v1internal:loadCodeAssist")) {
					return nil, fmt.Errorf("unexpected outbound request %s", r.URL.Redacted())
				}
				copy := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = target.Scheme, target.Host
				copy.URL, copy.Host = &address, target.Host
				return host.Client().Transport.RoundTrip(copy)
			})}
			defer func() { http.DefaultClient = previous }()
			for key, value := range map[string]string{"GEMINI_PROJECT_ID": "ambient-project", "GEMINI_OAUTH_CLIENT_ID": "ambient-client", "GEMINI_OAUTH_CLIENT_SECRET": "ambient-secret", "ANTIGRAVITY_CLI_VERSION": "ambient-version"} {
				t.Setenv(key, value)
			}
			rejected := strings.HasPrefix(name, "changed-") || name == "missing-client-credentials"
			if strings.HasPrefix(name, "changed-") {
				key := map[string]string{"changed-project": "GEMINI_PROJECT_ID", "changed-client-id": "GEMINI_OAUTH_CLIENT_ID", "changed-version": "ANTIGRAVITY_CLI_VERSION"}[name]
				changeCaptured(key)
			}
			fresh, err := store.RefreshSelectedOAuthAccountForRuntime(t.Context(), ScopeGlobal, owner, original, true, admitted)
			if rejected {
				if name == "missing-client-credentials" {
					require.ErrorContains(t, err, "Gemini OAuth client credentials are not configured")
				} else {
					require.ErrorContains(t, err, "changed")
				}
				require.Nil(t, fresh)
				require.Zero(t, exchanges.Load())
			} else if name == "project-changed-during-exchange" {
				require.ErrorContains(t, err, "account token saved; provider config was not updated")
				require.NotNil(t, fresh)
				require.Equal(t, "selected-rotated-refresh", fresh.RefreshToken)
				require.EqualValues(t, 1, exchanges.Load())
			} else {
				require.NoError(t, err)
				require.NotNil(t, fresh)
				require.Equal(t, "selected-fresh-access", fresh.AccessToken)
				require.Equal(t, "selected-rotated-refresh", fresh.RefreshToken)
				require.EqualValues(t, 1, exchanges.Load())
				bound, err := useragent.ContextWithGeminiIdentity(t.Context(), *definition.NativeIdentity)
				require.NoError(t, err)
				require.Equal(t, "credential-project", gemini.ProjectForCredential(bound, fresh.AccessToken))
				require.EqualValues(t, 1, metadata.Load())
			}
			stored, readErr := accounts.Active(t.Context(), owner.AccountNamespace)
			require.NoError(t, readErr)
			require.NotNil(t, stored)
			provider, ok := store.Config().Providers.Get(gemini.ID)
			require.True(t, ok)
			after, readErr := os.ReadFile(store.globalDataPath)
			require.NoError(t, readErr)
			if rejected || name == "project-changed-during-exchange" {
				require.Equal(t, before, after)
				require.Equal(t, original.AccessToken, provider.APIKey)
				require.Zero(t, metadata.Load())
				if rejected {
					require.Equal(t, accounts.CredentialID(original), accounts.CredentialID(*stored))
				} else {
					require.Equal(t, accounts.CredentialID(*fresh), accounts.CredentialID(*stored))
				}
			} else {
				require.Equal(t, accounts.CredentialID(*fresh), accounts.CredentialID(*stored))
				require.Equal(t, fresh.Token(), provider.OAuthToken)
				require.Equal(t, fresh.AccessToken, provider.APIKey)
				require.Equal(t, fresh.AccessToken, gjson.GetBytes(after, "providers.gemini-ag.api_key").String())
				require.Equal(t, fresh.RefreshToken, gjson.GetBytes(after, "providers.gemini-ag.oauth.refresh_token").String())
			}
		})
	}
}
