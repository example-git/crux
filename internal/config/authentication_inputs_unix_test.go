//go:build !windows

package config

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthenticationInputsFIFORejectsWithoutBlocking(t *testing.T) {
	store, _ := authenticationInputsTestStore(t)
	require.NoError(t, os.MkdirAll(filepath.Dir(store.globalDataPath), 0o700))
	require.NoError(t, syscall.Mkfifo(store.globalDataPath, 0o600))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.CaptureAuthentication(ctx)
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "configuration inputs cannot be read")
	case <-ctx.Done():
		t.Fatal("FIFO config input blocked authentication capture")
	}
}

func TestAuthenticationInputsPathRecheckRejectsFIFOReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "crux.json")
	require.NoError(t, os.WriteFile(path, []byte("{}"), 0o600))
	file, err := openAuthenticationInput(path)
	require.NoError(t, err)
	defer file.Close()
	before, err := observeAuthenticationInput(file)
	require.NoError(t, err)
	require.NoError(t, os.Rename(path, path+".old"))
	// Ignore metadata effects of renaming the old inode, so verification must
	// actually open the path that is about to become a FIFO.
	before, err = observeAuthenticationInput(file)
	require.NoError(t, err)
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	done := make(chan error, 1)
	go func() { done <- verifyAuthenticationInput(t.Context(), path, file, before) }()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		t.Fatal("FIFO replacement blocked authentication input path recheck")
	}
}
