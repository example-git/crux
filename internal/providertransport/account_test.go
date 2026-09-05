package providertransport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestAccountIdentityUsesDeclaredOperationAndCredential(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/account", request.URL.Path)
		require.Equal(t, "Bearer access-token", request.Header.Get("Authorization"))
		_, _ = io.WriteString(response, `{"account":{"id":"account-1","name":"Example User"}}`)
	}))
	defer server.Close()
	operation := &Operation{
		ID: "account", Kind: "account", Method: http.MethodGet, Path: "/account",
		Endpoint: manifest.Endpoint{BaseURL: server.URL},
		Headers: []manifest.HeaderRule{{
			Operation: "set", Name: "Authorization",
			Value: &manifest.Template{Kind: "concat", Parts: []manifest.Template{
				{Kind: "literal", Value: "Bearer "}, {Kind: "credential", Ref: "account"},
			}},
		}},
		ResponseTransform: &manifest.JSONPipeline{MaxOperations: 2, Operations: []manifest.JSONOperation{
			{Operation: "copy", Path: "/id", From: "/account/id"},
			{Operation: "copy", Path: "/display_name", From: "/account/name"},
		}},
	}
	identity := AccountIdentity(operation, []manifest.Credential{{ID: "account", Kind: "oauth2"}})
	id, display, raw := identity(t.Context(), "access-token")
	require.Equal(t, "account-1", id)
	require.Equal(t, "Example User", display)
	require.Contains(t, string(raw), `"id":"account-1"`)
}
