package providerregistry

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestPluginAccountOperationPreservesDeclaredUserAgent(t *testing.T) {
	for _, literal := range []bool{false, true} {
		t.Run(fmt.Sprint(literal), func(t *testing.T) {
			t.Setenv("PLUGIN_ACCOUNT_VERSION", "9.9.9")
			expected := "target-account-cli/1.2.3"
			userAgent := &manifest.Template{Kind: "context", Ref: "client.user_agent"}
			if literal {
				expected = "ExactPluginAgent/7.8.9 (target platform)"
				userAgent = &manifest.Template{Kind: "literal", Value: expected}
			}
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, expected, r.Header.Get("User-Agent"))
				require.Equal(t, "Bearer synthetic-token", r.Header.Get("Authorization"))
				_, _ = fmt.Fprint(w, `{"id":"selected-account","display_name":"Selected"}`)
			}))
			defer host.Close()
			original := http.DefaultTransport
			http.DefaultTransport = host.Client().Transport
			defer func() { http.DefaultTransport = original }()
			address, err := url.Parse(host.URL)
			require.NoError(t, err)
			operation := &providertransport.Operation{
				ID: "account", Kind: "account", Method: http.MethodGet, Path: "/account",
				Endpoint:       manifest.Endpoint{ID: "account", BaseURL: host.URL, AllowedSchemes: []string{"https"}, AllowedHosts: []string{address.Hostname()}},
				ClientIdentity: &manifest.ResolvedClientIdentity{Environment: "PLUGIN_ACCOUNT_VERSION", CacheKey: "plugin-account-target", FallbackVersion: "0.0.1", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "target-account-cli/{version}", ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024},
				Headers: []manifest.HeaderRule{
					{Operation: "set", Name: "User-Agent", Value: userAgent, Protected: true},
					{Operation: "set", Name: "Authorization", Value: &manifest.Template{Kind: "concat", Parts: []manifest.Template{{Kind: "literal", Value: "Bearer "}, {Kind: "credential", Ref: "account"}}}, Protected: true},
				},
			}
			ctx := oauth.ContextWithEnvironment(t.Context(), []string{"PLUGIN_ACCOUNT_VERSION=1.2.3"})
			id, display, _, err := ExecuteAccountOperation(ctx, operation, []manifest.Credential{{ID: "account", Kind: "oauth2"}}, "synthetic-token")
			require.NoError(t, err)
			require.Equal(t, "selected-account", id)
			require.Equal(t, "Selected", display)
		})
	}
}
