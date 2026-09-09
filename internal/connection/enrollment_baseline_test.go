package connection

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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

// A4 baseline: Pair creates a fresh private identity before the HTTP request,
// but currently loses it when saving locally fails after server authorization.
func TestEnrollmentClientSaveFailureBaseline(t *testing.T) {
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
	result, err := enrollment.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "unsaved-client", result.Name)
	_, exists, err := Get(t.Context(), "unsaved-client")
	require.NoError(t, err)
	require.False(t, exists)
	authorized, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Len(t, authorized, 1, "baseline: server authorization survives without a saved client identity")
}
