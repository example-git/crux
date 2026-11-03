package webclient

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestConnectionCatalogRoutesRequireLocalOrigin(t *testing.T) {
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	t.Cleanup(func() { _ = native.Close() })
	handler := Handler(native, "127.0.0.1:9090", Startup{})
	for _, path := range []string{
		"/ui/connections",
		"/ui/connections/missing/browser",
		"/ui/connections/missing/project-sessions",
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9090"+path, nil)
		request.Header.Set("Origin", "https://attacker.invalid")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusForbidden, response.Code)
	}
	require.Empty(t, native.Backend().ListWorkspaces())
}

func TestMissingConnectionDoesNotUseLocalCatalog(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	native := server.NewServer(nil, "tcp", "127.0.0.1:9090")
	t.Cleanup(func() { _ = native.Close() })
	handler := Handler(native, "127.0.0.1:9090", Startup{})
	for _, path := range []string{
		"/ui/connections/missing/browser",
		"/ui/connections/missing/project-sessions",
	} {
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://127.0.0.1:9090"+path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		require.Equal(t, http.StatusNotFound, response.Code, response.Body.String())
	}
	require.Empty(t, native.Backend().ListWorkspaces())
}
