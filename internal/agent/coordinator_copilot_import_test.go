package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

type copilotImportTestTransport func(*http.Request) (*http.Response, error)

func (f copilotImportTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCoordinatorCopilotLogoutImportHTTPS(t *testing.T) {
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "LOCALAPPDATA": filepath.Join(root, ".config"), "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "integrated", "CRUX_DISABLE_AUTO_MEMORY": "true"}
	for key, value := range values {
		t.Setenv(key, value)
	}
	t.Setenv("COPILOT_ADVERTISE_MODE", "")
	var exchanges, requests atomic.Int32
	wantHeaders := copilot.Headers()
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/copilot_internal/v2/token" {
			exchanges.Add(1)
			require.Equal(t, "Bearer synthetic-github-import", r.Header.Get("Authorization"))
			_ = json.NewEncoder(w).Encode(map[string]any{"token": "synthetic-imported-access", "expires_at": time.Now().Add(time.Hour).Unix()})
			return
		}
		requests.Add(1)
		require.Equal(t, "/v1/chat/completions", r.URL.Path)
		require.Equal(t, "Bearer synthetic-imported-access", r.Header.Get("Authorization"))
		require.Equal(t, "user", r.Header.Get("X-Initiator"))
		for name, value := range wantHeaders {
			require.Equal(t, value, r.Header.Get(name), name)
		}
		_, _ = fmt.Fprint(w, `{"id":"fixture","object":"chat.completion","model":"fixture","choices":[{"message":{"role":"assistant","content":"imported account accepted"},"finish_reason":"stop"}]}`)
	}))
	defer host.Close()
	endpoint, err := url.Parse(host.URL)
	require.NoError(t, err)
	// Route the built-in, fixed GitHub exchange URL to a disposable TLS server.
	// All other traffic must already target this fixture; no external requests.
	routed := copilotImportTestTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "api.github.com" && r.URL.Path == "/copilot_internal/v2/token" {
			r = r.Clone(r.Context())
			u := *r.URL
			u.Scheme, u.Host = endpoint.Scheme, endpoint.Host
			r.URL = &u
		} else if r.URL.Host != endpoint.Host {
			return nil, fmt.Errorf("unexpected request outside Copilot fixture")
		}
		return host.Client().Transport.RoundTrip(r)
	})
	previousTransport, previousClient := http.DefaultTransport, http.DefaultClient
	http.DefaultTransport, http.DefaultClient = routed, &http.Client{Transport: routed}
	defer func() { http.DefaultTransport, http.DefaultClient = previousTransport, previousClient }()
	for _, directory := range []string{"config", "data", "project", ".config/github-copilot"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, directory), 0o700))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, ".config/github-copilot/apps.json"), []byte(`{"github.com:Iv1.b507a08c87ecfe98":{"oauth_token":"synthetic-github-import"}}`), 0o600))
	registry, err := providerregistry.New(registrytest.Registrations()...)
	require.NoError(t, err)
	registration, ok := registry.Lookup("copilot")
	require.True(t, ok)
	oldEntry := accounts.Entry{ID: "old", AccessToken: "synthetic-old", RefreshToken: "synthetic-old-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}
	require.NoError(t, accounts.Save(t.Context(), registration.AccountNamespace, oldEntry))
	source, err := json.Marshal(map[string]any{"providers": map[string]any{"copilot": map[string]any{"base_url": host.URL + "/v1", "owner": config.ProviderOwnerReference{Type: config.ProviderOwnerCore, Construction: providerregistry.ConstructionCopilot}, "models": []map[string]any{{"id": "fixture", "default_max_tokens": 128}}}}, "models": map[string]any{"large": map[string]any{"provider": "copilot", "model": "fixture"}, "small": map[string]any{"provider": "copilot", "model": "fixture"}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config/crux.json"), source, 0o600))
	data, err := json.Marshal(map[string]any{"providers": map[string]any{"copilot": map[string]any{"api_key": oldEntry.AccessToken, "oauth": oldEntry.Token()}}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "data/crux.json"), data, 0o600))
	store, err := config.LoadIsolated(filepath.Join(root, "project"), filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("copilot")
	require.True(t, ok)
	capture, err := store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	logout, err := store.LogoutAuthentication(t.Context(), config.ScopeGlobal, capture, owner)
	require.NoError(t, err)
	require.True(t, logout.RuntimePublished)
	loggedOut := store.RuntimeSnapshot()
	entry, marked, err := loggedOut.CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.True(t, marked)
	require.Nil(t, entry)
	otherHome := t.TempDir()
	t.Setenv("AI_CLI_DIR", filepath.Join(otherHome, "accounts"))
	t.Setenv("HOME", otherHome)
	t.Setenv("LOCALAPPDATA", otherHome)
	_, imported, err := store.ImportCopilotForOwner(t.Context(), owner)
	require.NoError(t, err)
	require.True(t, imported)
	require.Equal(t, int32(1), exchanges.Load())
	entry, marked, err = store.RuntimeSnapshot().CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.True(t, marked)
	require.NotNil(t, entry)
	require.Equal(t, "default", entry.ID)
	require.Equal(t, "synthetic-imported-access", entry.AccessToken)
	coord := &coordinator{cfg: store}
	large, _, err := coord.buildAgentModels(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false)
	require.NoError(t, err)
	response, err := large.Model.Generate(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("use the imported account")}})
	require.NoError(t, err)
	require.NotEmpty(t, response.Content)
	require.Equal(t, int32(1), requests.Load())
	entry, _, err = loggedOut.CapturedConstructionAccount(owner)
	require.NoError(t, err)
	require.Nil(t, entry)
	require.NoDirExists(t, filepath.Join(otherHome, "accounts"))
}
