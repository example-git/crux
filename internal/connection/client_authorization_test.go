package connection

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/lock"
	"github.com/stretchr/testify/require"
)

func newClientAuthorizationFixture(t *testing.T) (*ClientAuthorization, tls.ConnectionState) {
	t.Helper()
	setConnectionRoot(t, t.TempDir())
	server, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	_, certificate, err := Add(t.Context(), "fixture", "tcp://server.example:9443", server)
	require.NoError(t, err)
	require.NoError(t, AuthorizeClient(t.Context(), "fixture", certificate))
	_, authorization, err := ServerTLSConfigWithAuthorization(t.Context())
	require.NoError(t, err)
	peer, err := parseCertificate(certificate, x509.ExtKeyUsageClientAuth)
	require.NoError(t, err)
	return authorization, tls.ConnectionState{PeerCertificates: []*x509.Certificate{peer}, VerifiedChains: [][]*x509.Certificate{{peer}}}
}

func TestClientAuthorizationCanceledStoreRead(t *testing.T) {
	authorization, state := newClientAuthorizationFixture(t)
	require.NoError(t, authorization.Authorize(t.Context(), state))
	release, err := lock.File(t.Context(), authorization.path+".lock")
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { result <- authorization.Authorize(ctx, state) }()
	cancel()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrClientAuthorization)
	case <-time.After(time.Second):
		t.Fatal("authorization ignored cancellation while waiting for the store lock")
	}
}

func TestClientAuthorizationRequiresVerifiedExactLeaf(t *testing.T) {
	authorization, state := newClientAuthorizationFixture(t)
	other, err := NewClientIdentity("other")
	require.NoError(t, err)
	otherCertificate, err := parseCertificate(other.Certificate, x509.ExtKeyUsageClientAuth)
	require.NoError(t, err)
	for _, candidate := range []tls.ConnectionState{
		{},
		{PeerCertificates: state.PeerCertificates},
		{PeerCertificates: []*x509.Certificate{nil}, VerifiedChains: state.VerifiedChains},
		{PeerCertificates: state.PeerCertificates, VerifiedChains: [][]*x509.Certificate{{nil}}},
		{PeerCertificates: state.PeerCertificates, VerifiedChains: [][]*x509.Certificate{{otherCertificate}}},
		{PeerCertificates: []*x509.Certificate{otherCertificate}, VerifiedChains: [][]*x509.Certificate{{otherCertificate}}},
	} {
		require.ErrorIs(t, authorization.Authorize(t.Context(), candidate), ErrClientAuthorization)
	}
	require.ErrorIs(t, (*ClientAuthorization)(nil).Authorize(t.Context(), state), ErrClientAuthorization)
}

func TestClientAuthorizationCapturedPathAndServerIdentity(t *testing.T) {
	authorization, state := newClientAuthorizationFixture(t)
	original, err := os.ReadFile(authorization.path)
	require.NoError(t, err)
	other := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(other, "connections.json"), original, 0o600))
	setConnectionRoot(t, other)
	require.NoError(t, RevokeClient(t.Context(), "fixture"))
	require.NoError(t, authorization.Authorize(t.Context(), state), "environment changes cannot replace captured authority")
	require.NoError(t, os.WriteFile(authorization.path, []byte(`{"version":1,"authorized_clients":{}}`), 0o600))
	require.ErrorIs(t, authorization.Authorize(t.Context(), state), ErrClientAuthorization)
	// A different server's valid store is not authority for the old listener.
	require.NoError(t, os.Remove(filepath.Join(other, "connections.json")))
	_, err = EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(other, "connections.json"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(authorization.path, data, 0o600))
	require.ErrorIs(t, authorization.Authorize(t.Context(), state), ErrClientAuthorization)
}

func TestClientAuthorizationRejectsMalformedJSON(t *testing.T) {
	for _, data := range []string{
		`{"version":1} {"version":1}`,
		`{"version":1,"version":1}`,
		`{"authorized_clients":{"same":"a","same":"b"}}`,
		`{"connections":{"sample":{"client":{"certificate":"a","certificate":"b"}}}}`,
		`{"version":1,"VERSION":1}`,
		`{"authorized_clients":{"retained":"a"},"AUTHORIZED_CLIENTS":{"revoked":"b"}}`,
		`{"server":{"certificate":"a","Certificate":"b"}}`,
		`{"connections":{"sample":{"Address":"tcp://example:1"}}}`,
		`{"connections":{"sample":{"client":{"Private_Key":"key"}}}}`,
		`{"version":1`,
		"{\"invalid\":\"\xff\"}",
	} {
		require.ErrorIs(t, validateClientAuthorizationJSON([]byte(data)), ErrClientAuthorization)
	}
	require.NoError(t, validateClientAuthorizationJSON([]byte(`{"version":1,"authorized_clients":{},"connections":{}}`)))
	require.NoError(t, validateClientAuthorizationJSON([]byte(`{"authorized_clients":{"Mixed":"a","mixed":"b"},"connections":{"Mixed":{"name":"Mixed"},"mixed":{"name":"mixed"}}}`)), "map names remain case-sensitive user values")
}

func TestClientAuthorizationPreservesTLSVerifyConnection(t *testing.T) {
	newClientAuthorizationFixture(t)
	serverTLS, err := ServerTLSConfig(t.Context())
	require.NoError(t, err)
	var verified atomic.Bool
	serverTLS.VerifyConnection = func(tls.ConnectionState) error {
		verified.Store(true)
		return errors.New("additional verification denied")
	}
	var reachedHandler atomic.Bool
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reachedHandler.Store(true)
	}))
	srv.TLS = serverTLS
	srv.StartTLS()
	t.Cleanup(srv.Close)
	saved, exists, err := Get(t.Context(), "fixture")
	require.NoError(t, err)
	require.True(t, exists)
	clientTLS, err := ClientTLSConfig(saved)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: clientTLS}
	t.Cleanup(transport.CloseIdleConnections)
	response, err := (&http.Client{Transport: transport, Timeout: 5 * time.Second}).Get(srv.URL)
	if response != nil {
		response.Body.Close()
	}
	require.Error(t, err)
	require.True(t, verified.Load())
	require.False(t, reachedHandler.Load())
}
