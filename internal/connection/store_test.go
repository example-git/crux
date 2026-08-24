package connection

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSavedConnectionAuthenticatesWithMutualTLS(t *testing.T) {
	root := t.TempDir()
	setConnectionRoot(t, root)
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	saved, clientCode, err := Add(t.Context(), "workstation", "tcp://server.example:9443", serverCode)
	require.NoError(t, err)
	require.NoError(t, AuthorizeClient(t.Context(), "workstation", clientCode))

	serverTLS, err := ServerTLSConfig(t.Context())
	require.NoError(t, err)
	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		require.NotEmpty(t, request.TLS.PeerCertificates)
		writer.WriteHeader(http.StatusNoContent)
	}))
	testServer.TLS = serverTLS
	testServer.StartTLS()
	t.Cleanup(testServer.Close)

	clientTLS, err := ClientTLSConfig(saved)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(transport.CloseIdleConnections)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, testServer.URL, nil)
	require.NoError(t, err)
	response, err := (&http.Client{Transport: transport}).Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusNoContent, response.StatusCode)
	require.NoError(t, response.Body.Close())

	reloaded, exists, err := Get(t.Context(), "workstation")
	require.NoError(t, err)
	require.True(t, exists)
	require.Equal(t, saved, reloaded)
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(root, "connections.json"))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
}

func TestServerRejectsUnauthorizedClientIdentity(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, authorizedCode, err := Add(t.Context(), "authorized", "tcp://server.example:9443", serverCode)
	require.NoError(t, err)
	require.NoError(t, AuthorizeClient(t.Context(), "authorized", authorizedCode))
	unauthorized, _, err := Add(t.Context(), "unauthorized", "tcp://server.example:9443", serverCode)
	require.NoError(t, err)

	serverTLS, err := ServerTLSConfig(t.Context())
	require.NoError(t, err)
	testServer := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	testServer.TLS = serverTLS
	testServer.StartTLS()
	t.Cleanup(testServer.Close)

	clientTLS, err := ClientTLSConfig(unauthorized)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(transport.CloseIdleConnections)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, testServer.URL, nil)
	require.NoError(t, err)
	response, err := (&http.Client{Transport: transport}).Do(request)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.Error(t, err)
}

func TestConnectionStoreRejectsInvalidAndDuplicateRecords(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, _, err := Add(t.Context(), "invalid", "tcp://server.example:9443", "not-a-certificate")
	require.ErrorContains(t, err, "invalid server pairing code")

	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, _, err = Add(t.Context(), "saved", "tcp://server.example:9443", serverCode)
	require.NoError(t, err)
	_, _, err = Add(t.Context(), "saved", "tcp://other.example:9443", serverCode)
	require.ErrorContains(t, err, "connection already exists")
}

func TestConnectionStoreRestoresPreviousFileWhenReplacementFails(t *testing.T) {
	root := t.TempDir()
	setConnectionRoot(t, root)
	serverCode, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, clientCode, err := Add(t.Context(), "pending", "tcp://server.example:9443", serverCode)
	require.NoError(t, err)
	storeFile := filepath.Join(root, "connections.json")
	original, err := os.ReadFile(storeFile)
	require.NoError(t, err)

	originalRename := renameStoreFile
	calls := 0
	renameStoreFile = func(oldPath, newPath string) error {
		calls++
		switch calls {
		case 1, 3:
			return errors.New("injected replacement failure")
		default:
			return os.Rename(oldPath, newPath)
		}
	}
	t.Cleanup(func() { renameStoreFile = originalRename })

	err = AuthorizeClient(t.Context(), "pending", clientCode)
	require.ErrorContains(t, err, "injected replacement failure")
	actual, readErr := os.ReadFile(storeFile)
	require.NoError(t, readErr)
	require.Equal(t, original, actual)
}

func setConnectionRoot(t *testing.T, root string) {
	t.Helper()
	t.Setenv("CRUX_GLOBAL_DATA", root)
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
}
