//go:build unix

package connection

import (
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientAuthorizationFIFOIsRejectedWithoutBlocking(t *testing.T) {
	authorization, state := newClientAuthorizationFixture(t)
	require.NoError(t, os.Remove(authorization.path))
	require.NoError(t, syscall.Mkfifo(authorization.path, 0o600))
	result := make(chan error, 1)
	go func() { result <- authorization.Authorize(t.Context(), state) }()
	select {
	case err := <-result:
		require.ErrorIs(t, err, ErrClientAuthorization)
	case <-time.After(time.Second):
		t.Fatal("opening a FIFO must not block authorization admission")
	}
}

func TestClientAuthorizationUnreadableStoreFailsClosed(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read a permission-denied fixture")
	}
	authorization, state := newClientAuthorizationFixture(t)
	require.NoError(t, os.Chmod(authorization.path, 0))
	t.Cleanup(func() { _ = os.Chmod(authorization.path, 0o600) })
	require.ErrorIs(t, authorization.Authorize(t.Context(), state), ErrClientAuthorization)
}
