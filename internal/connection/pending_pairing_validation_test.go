package connection

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type pendingPairingValidationFixture struct {
	ctx          context.Context
	clientPath   string
	serverPath   string
	serverBefore []byte
	retained     pendingPairing
	proofs       atomic.Int32
}

// The relay discards a real successful setup response, rather than fabricating
// a grant or staging a pending record directly. Recovery then uses the daemon
// proof endpoint after setup has released the same pinned address.
func newPendingPairingValidationFixture(t *testing.T) *pendingPairingValidationFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	root := t.TempDir()
	for _, key := range []string{"HOME", "AI_CLI_DIR", "XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "CRUX_GLOBAL_CONFIG"} {
		t.Setenv(key, filepath.Join(root, key))
	}
	serverRoot, clientRoot := filepath.Join(root, "server"), filepath.Join(root, "client")
	setConnectionRoot(t, serverRoot)
	serverCode, err := EnsureServerIdentity(ctx)
	require.NoError(t, err)
	f := &pendingPairingValidationFixture{ctx: ctx, serverPath: storePath()}
	serverData, err := loadStoreAt(ctx, f.serverPath)
	require.NoError(t, err)
	certificate, privateKey, err := parseIdentity(*serverData.Server, x509.ExtKeyUsageServerAuth)
	require.NoError(t, err)
	relay := httptest.NewUnstartedServer(nil)
	t.Cleanup(relay.Close)
	address := "tcp://" + relay.Listener.Addr().String()
	enrollment, err := StartEnrollment(ctx, "tcp://127.0.0.1:0", address, time.Minute, approveEnrollmentForTest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	setupURL, err := url.Parse("https://" + enrollment.listener.Addr().String() + enrollmentPath)
	require.NoError(t, err)
	transport := &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS13,
		// Both listeners present this fixture's exact server identity. The
		// forwarding hop checks that pin independently of ambient trust roots.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 || !bytes.Equal(state.PeerCertificates[0].Raw, certificate.Raw) {
				return errors.New("fixture setup certificate changed")
			}
			return nil
		},
	}}
	t.Cleanup(transport.CloseIdleConnections)
	dropped := make(chan error, 1)
	var posts atomic.Int32
	relay.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != enrollmentPath {
			http.NotFound(w, r)
			return
		}
		posts.Add(1)
		forward := r.Clone(r.Context())
		forward.URL = setupURL
		forward.RequestURI = ""
		response, err := transport.RoundTrip(forward)
		if err != nil {
			dropped <- err
			http.Error(w, "fixture setup forwarding failed", http.StatusBadGateway)
			return
		}
		body, readErr := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if readErr != nil || response.StatusCode != http.StatusCreated || !validEnrollmentReply(body) {
			dropped <- errors.New("real setup did not confirm authorization")
			http.Error(w, "fixture setup did not authorize", http.StatusBadGateway)
			return
		}
		// Close without writing status/body to Pair: the authorization result
		// was received by the relay, but the client cannot observe that result.
		connection, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			err = connection.Close()
		}
		dropped <- err
	})
	relay.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{certificate.Raw}, PrivateKey: privateKey}}}
	relay.StartTLS()
	setConnectionRoot(t, clientRoot)
	f.clientPath = storePath()
	_, pairErr := Pair(ctx, "pending-client", enrollment.SetupCode())
	var pendingErr *PairingPendingError
	require.ErrorAs(t, pairErr, &pendingErr)
	require.ErrorContains(t, pairErr, "enroll client")
	select {
	case err := <-dropped:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal("relay did not observe and discard the real authorization receipt")
	}
	require.Equal(t, int32(1), posts.Load())
	result, err := enrollment.Wait(ctx)
	require.NoError(t, err)
	require.Equal(t, "pending-client", result.Name)
	listed, err := ListPendingPairings(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	require.Equal(t, pendingErr.OperationID, listed[0].OperationID)
	require.Equal(t, result.Fingerprint, listed[0].ClientFingerprint)
	// Read the serialized sidecar anew; no retained client process object is
	// supplied to RecoverPairing, which must independently load this identity.
	disk, image, err := readPendingPairings(f.clientPath)
	require.NoError(t, err)
	f.retained = disk.Entries[pendingErr.OperationID]
	require.NotEmpty(t, f.retained.Connection.Client.PrivateKey)
	require.Equal(t, serverCode, f.retained.Connection.ServerCertificate)
	require.Equal(t, address, f.retained.Connection.Address)
	require.Equal(t, os.FileMode(0o600), image.info.Mode().Perm())
	_, exists, err := Get(ctx, f.retained.Connection.Name)
	require.NoError(t, err)
	require.False(t, exists)
	serverData, err = loadStoreAt(ctx, f.serverPath)
	require.NoError(t, err)
	require.Len(t, serverData.AuthorizedClients, 1)
	require.Equal(t, f.retained.Connection.Client.Certificate, serverData.AuthorizedClients[result.Name])
	f.serverBefore, err = os.ReadFile(f.serverPath)
	require.NoError(t, err)
	_, err = Pair(ctx, f.retained.Connection.Name, enrollment.SetupCode())
	require.ErrorContains(t, err, "pending")
	require.Equal(t, int32(1), posts.Load(), "retry must not create or authorize a second identity")
	require.NoError(t, enrollment.Close())
	relay.Close()
	transport.CloseIdleConnections()

	setConnectionRoot(t, serverRoot)
	serverTLS, authority, err := ServerTLSConfigWithAuthorization(ctx)
	require.NoError(t, err)
	daemon := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != AuthorizationProofPath {
			http.NotFound(w, r)
			return
		}
		proof, err := authority.Proof(r.Context(), *r.TLS)
		if err != nil {
			http.Error(w, "authorization denied", http.StatusForbidden)
			return
		}
		f.proofs.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(proof)
	}))
	require.NoError(t, daemon.Listener.Close())
	daemon.Listener, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", strings.TrimPrefix(address, "tcp://"))
	require.NoError(t, err)
	daemon.TLS = serverTLS
	daemon.StartTLS()
	t.Cleanup(daemon.Close)
	setConnectionRoot(t, clientRoot)
	return f
}

func (f *pendingPairingValidationFixture) assertSaved(t *testing.T, saved Connection) {
	t.Helper()
	require.True(t, f.retained.Connection.Client == saved.Client, "the exact retained certificate and private key must survive recovery")
	require.Equal(t, f.retained.Connection.ServerCertificate, saved.ServerCertificate)
	require.Equal(t, f.retained.Connection.Address, saved.Address)
	reloaded, exists, err := Get(f.ctx, saved.Name)
	require.NoError(t, err)
	require.True(t, exists && reloaded == saved, "fresh store read must retain the exact promoted connection")
	listed, err := ListPendingPairings(f.ctx)
	require.NoError(t, err)
	require.Empty(t, listed)
	after, err := os.ReadFile(f.serverPath)
	require.NoError(t, err)
	require.Equal(t, f.serverBefore, after, "proof and recovery must not mutate the original server grant")
}

func TestPendingPairingLostAuthorizationReplyRecoversAfterDaemonHandoff(t *testing.T) {
	f := newPendingPairingValidationFixture(t)
	saved, err := RecoverPairing(f.ctx, f.retained.OperationID, "")
	require.NoError(t, err)
	require.Equal(t, f.retained.Connection.Name, saved.Name)
	require.Equal(t, int32(1), f.proofs.Load())
	f.assertSaved(t, saved)
}

func TestPendingPairingNameConflictPreservesIdentitiesAndRecoversAlternate(t *testing.T) {
	f := newPendingPairingValidationFixture(t)
	identity, err := NewClientIdentity("already-saved")
	require.NoError(t, err)
	existing := Connection{Name: "occupied", Address: f.retained.Connection.Address, ServerCertificate: f.retained.Connection.ServerCertificate, Client: identity}
	require.NoError(t, SaveConnection(f.ctx, existing))
	storeBefore, err := os.ReadFile(f.clientPath)
	require.NoError(t, err)
	pendingBefore, err := os.ReadFile(pendingPairingPath(f.clientPath))
	require.NoError(t, err)
	_, err = RecoverPairing(f.ctx, f.retained.OperationID, existing.Name)
	var pendingErr *PairingPendingError
	require.ErrorAs(t, err, &pendingErr)
	require.Equal(t, f.retained.OperationID, pendingErr.OperationID)
	require.ErrorContains(t, err, "conflicts")
	storeAfter, err := os.ReadFile(f.clientPath)
	require.NoError(t, err)
	require.Equal(t, storeBefore, storeAfter)
	pendingAfter, err := os.ReadFile(pendingPairingPath(f.clientPath))
	require.NoError(t, err)
	require.Equal(t, pendingBefore, pendingAfter)
	saved, err := RecoverPairing(f.ctx, f.retained.OperationID, "explicit-unused-name")
	require.NoError(t, err)
	require.Equal(t, "explicit-unused-name", saved.Name)
	require.Equal(t, int32(2), f.proofs.Load())
	f.assertSaved(t, saved)
	unchanged, exists, err := Get(f.ctx, existing.Name)
	require.NoError(t, err)
	require.True(t, exists && unchanged == existing, "conflicting saved identity must remain unchanged")
	_, exists, err = Get(f.ctx, f.retained.Connection.Name)
	require.NoError(t, err)
	require.False(t, exists, "explicit promotion must not also save under the original name")
}

func TestPendingPairingPromotionCleanupFailureRetriesSameSavedIdentity(t *testing.T) {
	f := newPendingPairingValidationFixture(t)
	originalRename := renameStoreFile
	t.Cleanup(func() { renameStoreFile = originalRename })
	var cleanupFailures int
	renameStoreFile = func(source, destination string) error {
		if destination == pendingPairingPath(f.clientPath) {
			data, err := os.ReadFile(source)
			if err != nil {
				return err
			}
			var candidate pendingPairings
			if err := json.Unmarshal(data, &candidate); err != nil {
				return err
			}
			if len(candidate.Entries) == 0 {
				cleanupFailures++
				return errors.New("synthetic pending cleanup failure")
			}
		}
		return originalRename(source, destination)
	}
	saved, err := RecoverPairing(f.ctx, f.retained.OperationID, "promoted-name")
	var savedErr *PairingSavedError
	require.ErrorAs(t, err, &savedErr)
	require.Equal(t, "promoted-name", savedErr.Name)
	require.Equal(t, 1, cleanupFailures)
	require.True(t, saved.Client == f.retained.Connection.Client)
	retained, err := pendingPairingAt(f.ctx, f.clientPath, f.retained.OperationID)
	require.NoError(t, err)
	require.Equal(t, saved.Name, retained.PromotionName)
	require.True(t, retained.Connection == f.retained.Connection)
	storeBefore, err := os.ReadFile(f.clientPath)
	require.NoError(t, err)
	infoBefore, err := os.Stat(f.clientPath)
	require.NoError(t, err)
	renameStoreFile = originalRename
	_, err = RecoverPairing(f.ctx, f.retained.OperationID, "must-not-duplicate")
	require.ErrorContains(t, err, "already saved")
	_, exists, err := Get(f.ctx, "must-not-duplicate")
	require.NoError(t, err)
	require.False(t, exists)
	recovered, err := RecoverPairing(f.ctx, f.retained.OperationID, "")
	require.NoError(t, err)
	require.True(t, recovered == saved, "cleanup retry must preserve the original promotion name and identity")
	f.assertSaved(t, recovered)
	after, err := os.ReadFile(f.clientPath)
	require.NoError(t, err)
	require.Equal(t, storeBefore, after)
	infoAfter, err := os.Stat(f.clientPath)
	require.NoError(t, err)
	require.True(t, os.SameFile(infoBefore, infoAfter), "cleanup retry must not republish an already saved connection")
	require.Equal(t, int32(3), f.proofs.Load())
}
