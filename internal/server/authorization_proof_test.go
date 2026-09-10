package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/connection"
	"github.com/stretchr/testify/require"
)

func TestAuthorizationProofThroughRegisteredMutualTLSRoute(t *testing.T) {
	hs, clients := newRemoteAuthorityTLSHarness(t)
	saved, exists, err := connection.Get(t.Context(), "retained")
	require.NoError(t, err)
	require.True(t, exists)
	saved.Address = "tcp://" + strings.TrimPrefix(hs.URL, "https://")
	before := remoteServerState(t)
	proof, err := connection.ConfirmAuthorization(t.Context(), saved)
	require.NoError(t, err)
	require.NoError(t, proof.ValidateFor(saved))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+connection.AuthorizationProofPath, nil)
	require.NoError(t, err)
	response, err := clients["retained"].Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, "no-store", response.Header.Get("Cache-Control"))
	var fields map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&fields))
	require.NoError(t, response.Body.Close())
	require.Len(t, fields, 3)
	require.Equal(t, proof.Principal, fields["principal"])
	require.Equal(t, proof.ServerFingerprint, fields["server_fingerprint"])
	require.Equal(t, before, remoteServerState(t))

	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+connection.AuthorizationProofPath, nil)
	require.NoError(t, err)
	response, err = clients["revoked"].Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.NoError(t, connection.RevokeClient(t.Context(), "revoked"))
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+connection.AuthorizationProofPath, nil)
	require.NoError(t, err)
	response, err = clients["revoked"].Do(request)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
	require.NoError(t, response.Body.Close())
	_, err = connection.ConfirmAuthorization(t.Context(), saved)
	require.NoError(t, err, "the retained principal must still work on the same server")
	request, err = http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+connection.AuthorizationProofPath, nil)
	require.NoError(t, err)
	response, err = clients["unauthorized"].Do(request)
	if response != nil {
		_ = response.Body.Close()
	}
	require.Error(t, err)
}

func TestAuthorizationProofRejectsUnauthenticatedLocalHealth(t *testing.T) {
	srv := NewServer(nil, "tcp", "127.0.0.1:0")
	t.Cleanup(srv.backend.Shutdown)
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	for _, path := range []string{"/v1/health", connection.AuthorizationProofPath} {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, hs.URL+path, nil)
		require.NoError(t, err)
		response, err := hs.Client().Do(request)
		require.NoError(t, err)
		if path == "/v1/health" {
			require.Equal(t, http.StatusOK, response.StatusCode)
		} else {
			require.Equal(t, http.StatusForbidden, response.StatusCode)
		}
		require.NoError(t, response.Body.Close())
	}
}
