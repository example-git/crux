package connection

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// G1: terminal cancellation fences the persisted authorization, including a
// request that already reserved the token before cancellation.
func TestEnrollmentCancellationPreventsLateAuthorization(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	enrollment, err := StartEnrollment(ctx, "tcp://127.0.0.1:0", "", time.Minute, approveEnrollmentForTest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	entered, release := make(chan struct{}), make(chan struct{})
	enrollment.authorizeClient = func(ctx context.Context, name, certificate string, commit authorizationCommit) error {
		close(entered)
		<-release
		return authorizeClientWithCommit(ctx, name, certificate, commit)
	}
	identity, err := NewClientIdentity("late-client")
	require.NoError(t, err)
	body, err := json.Marshal(enrollmentRequest{Name: "late-client", Certificate: identity.Certificate})
	require.NoError(t, err)
	status := make(chan int, 1)
	go func() {
		code, _ := enrollmentRequestStatus(t.Context(), enrollment.setup, body)
		status <- code
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("authorization did not reach reservation barrier")
	}
	cancel()
	_, err = enrollment.Wait(t.Context())
	require.ErrorIs(t, err, context.Canceled)
	authorized, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Empty(t, authorized)
	close(release)
	require.NotEqual(t, http.StatusCreated, <-status)
	authorized, err = ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Empty(t, authorized, "cancellation must prevent a later authorization commit")
}

// Recover the exact durably retained key after setup has closed and the daemon
// has taken over its address. A local save failure must not require re-pairing.
func TestEnrollmentClientSaveFailureRecovery(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	enrollment, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute, approveEnrollmentForTest)
	require.NoError(t, err)
	t.Cleanup(func() { _ = enrollment.Close() })
	original := renameStoreFile
	t.Cleanup(func() { renameStoreFile = original })
	renameStoreFile = func(oldPath, newPath string) error {
		content, err := os.ReadFile(oldPath)
		if err != nil {
			return err
		}
		var candidate store
		if err := json.Unmarshal(content, &candidate); err != nil {
			return err
		}
		if len(candidate.Connections) > 0 {
			return errors.New("synthetic client save failure")
		}
		return original(oldPath, newPath)
	}
	_, err = Pair(t.Context(), "unsaved-client", enrollment.SetupCode())
	require.ErrorContains(t, err, "synthetic client save failure")
	var pendingError *PairingPendingError
	require.ErrorAs(t, err, &pendingError)
	renameStoreFile = original
	result, err := enrollment.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "unsaved-client", result.Name)
	_, exists, err := Get(t.Context(), "unsaved-client")
	require.NoError(t, err)
	require.False(t, exists)
	authorized, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Len(t, authorized, 1)
	pending, err := ListPendingPairings(t.Context())
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, pendingError.OperationID, pending[0].OperationID)
	require.Equal(t, result.Fingerprint, pending[0].ClientFingerprint)
	retained, err := pendingPairingAt(t.Context(), storePath(), pendingError.OperationID)
	require.NoError(t, err)
	info, err := os.Stat(pendingPairingPath(storePath()))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	_, err = Pair(t.Context(), "unsaved-client", enrollment.SetupCode())
	require.ErrorContains(t, err, "pending")
	require.NoError(t, enrollment.Close())

	serverTLS, authority, err := ServerTLSConfigWithAuthorization(t.Context())
	require.NoError(t, err)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != AuthorizationProofPath {
			http.NotFound(w, r)
			return
		}
		proof, err := authority.Proof(r.Context(), *r.TLS)
		if err != nil {
			http.Error(w, "authorization denied", http.StatusForbidden)
			return
		}
		_ = json.NewEncoder(w).Encode(proof)
	}))
	require.NoError(t, server.Listener.Close())
	server.Listener, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", strings.TrimPrefix(enrollment.Address(), "tcp://"))
	require.NoError(t, err)
	server.TLS = serverTLS
	server.StartTLS()
	t.Cleanup(server.Close)
	saved, err := RecoverPairing(t.Context(), pendingError.OperationID, "")
	require.NoError(t, err)
	require.True(t, saved.Client == retained.Connection.Client, "recovery must preserve the exact retained private identity")
	reloaded, exists, err := Get(t.Context(), "unsaved-client")
	require.NoError(t, err)
	require.True(t, exists && reloaded == saved, "recovered identity must be durably saved")
	pending, err = ListPendingPairings(t.Context())
	require.NoError(t, err)
	require.Empty(t, pending)
	after, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Equal(t, authorized, after, "recovery must not change server grants")
}
