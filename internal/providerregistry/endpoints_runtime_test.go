package providerregistry

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest/manifesttest"
	"github.com/stretchr/testify/require"
)

func TestDelegatedConsumersUseManifestEndpoints(t *testing.T) {
	for _, providerID := range []string{"codex", "gemini-ag"} {
		t.Run(providerID, func(t *testing.T) {
			t.Setenv("CODEX_VERSION", "fixture")
			t.Setenv("CODEX_CLI_VERSION", "fixture")
			t.Setenv("ANTIGRAVITY_CLI_VERSION", "fixture")
			var mu sync.Mutex
			var paths []string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				paths = append(paths, r.URL.Path)
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/token":
					require.NoError(t, r.ParseForm())
					require.Equal(t, "synthetic-client", r.Form.Get("client_id"))
					require.Equal(t, "refresh-value", r.Form.Get("refresh_token"))
					_, _ = io.WriteString(w, `{"access_token":"manifest-token","expires_in":3600}`)
				case "/identity":
					require.Equal(t, "Bearer manifest-token", r.Header.Get("Authorization"))
					_, _ = io.WriteString(w, `{"email":"fixture@example.invalid"}`)
				case "/project":
					require.Equal(t, "Bearer manifest-token", r.Header.Get("Authorization"))
					_, _ = io.WriteString(w, `{"cloudaicompanionProject":"manifest-project"}`)
				case "/quota":
					require.Equal(t, "Bearer manifest-token", r.Header.Get("Authorization"))
					require.Equal(t, "https://codex-usage.example.invalid/", r.Header.Get("Origin"))
					_, _ = io.WriteString(w, `{"plan_type":"fixture","rate_limit":{"primary_window":{"used_percent":37,"limit_window_seconds":18000}}}`)
				default:
					t.Errorf("unexpected request %s", r.URL)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(server.Close)
			oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
			http.DefaultClient, http.DefaultTransport = server.Client(), server.Client().Transport
			t.Cleanup(func() { http.DefaultClient, http.DefaultTransport = oldClient, oldTransport })
			value := manifesttest.Delegated(providerID)
			u, err := url.Parse(server.URL)
			require.NoError(t, err)
			for i := range value.Capabilities.Endpoints {
				e := &value.Capabilities.Endpoints[i]
				if slices.Contains(e.AllowedSchemes, "wss") {
					continue
				}
				e.BaseURL = server.URL + "/" + e.ID
				e.AllowedHosts = []string{u.Hostname()}
			}
			registration, err := FromManifest(value, manifesttest.StaticText())
			require.NoError(t, err)
			require.NoError(t, ValidateActivation(registration))
			registry, err := New(registration)
			require.NoError(t, err)
			registration, ok := registry.Lookup(providerID)
			require.True(t, ok)
			challenge, err := registration.OAuth.PrepareCode(t.Context(), registration.OAuth.Callback.Port)
			require.NoError(t, err)
			defer challenge.Close()
			authorize, err := url.Parse(challenge.AuthorizationURL())
			require.NoError(t, err)
			require.Equal(t, u.Host, authorize.Host)
			require.Equal(t, "/authorize", authorize.Path)
			require.Equal(t, "fixture.read fixture.email", authorize.Query().Get("scope"))
			token, err := registration.OAuth.Refresh(t.Context(), "refresh-value")
			require.NoError(t, err)
			require.Equal(t, "manifest-token", token.AccessToken)
			id, _, _ := registration.Identity(t.Context(), token.AccessToken)
			require.Equal(t, "fixture@example.invalid", id)
			if providerID == "codex" {
				quota, err := registration.Quota(t.Context(), token.AccessToken)
				require.NoError(t, err)
				require.Equal(t, 37, quota.Windows[0].Percent)
				require.Equal(t, "5h", quota.Windows[0].Name)
				require.Equal(t, server.URL+"/images", registration.ImageEndpoint.BaseURL)
			} else {
				require.Equal(t, "manifest-project", registration.Gemini.ProjectForCredential(t.Context(), token.AccessToken))
			}
			mu.Lock()
			defer mu.Unlock()
			require.Contains(t, paths, "/token")
			require.Contains(t, paths, "/identity")
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			require.False(t, strings.Contains(string(encoded), "googleapis.com"))
		})
	}
}
