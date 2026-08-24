package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/example-git/crux/internal/connection"
	"github.com/stretchr/testify/require"
)

func TestAuthenticatedClientUsesSavedMutualTLSIdentity(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	saved, clientCode, err := connection.Add(t.Context(), "remote", "tcp://127.0.0.1:1", serverCode)
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "remote", clientCode))
	serverTLS, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)

	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.Equal(t, "/v1/health", request.URL.Path)
		require.NotEmpty(t, request.TLS.PeerCertificates)
		writer.WriteHeader(http.StatusOK)
	}))
	testServer.TLS = serverTLS
	testServer.StartTLS()
	t.Cleanup(testServer.Close)
	serverURL, err := url.Parse(testServer.URL)
	require.NoError(t, err)
	saved.Address = "tcp://" + serverURL.Host

	client, err := NewAuthenticatedClient(t.TempDir(), saved)
	require.NoError(t, err)
	require.NoError(t, client.Health(t.Context()))
}
