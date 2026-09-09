//go:build !windows

package accounts

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountSnapshotRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.json")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := CaptureStateAt(ctx, path, []string{snapshotNamespace})
		result <- err
	}()
	select {
	case err := <-result:
		require.ErrorContains(t, err, "not a regular file")
	case <-ctx.Done():
		// Release an incorrectly blocking reader so a regression cannot leave
		// the package-global account mutex held for the rest of the suite.
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = writer.Close()
		}
		<-result
		t.Fatal("capture blocked opening a non-regular account file")
	}
}
