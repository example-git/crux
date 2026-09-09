//go:build unix

package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestProviderContextOpenFIFOWithoutWriter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "instructions.txt")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	type opened struct {
		file *os.File
		err  error
	}
	done := make(chan opened, 1)
	go func() {
		file, err := openProviderContextFile(path)
		done <- opened{file, err}
	}()
	select {
	case result := <-done:
		require.NoError(t, result.err)
		defer result.file.Close()
		info, err := result.file.Stat()
		require.NoError(t, err)
		require.False(t, info.Mode().IsRegular(), "the caller must reject this opened FIFO")
	case <-time.After(time.Second):
		// Release a regressed blocking reader before failing so the test does
		// not leave a goroutine or descriptor behind.
		writer, err := os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			writer.Close()
		}
		select {
		case result := <-done:
			if result.file != nil {
				result.file.Close()
			}
		case <-time.After(time.Second):
		}
		t.Fatal("opening client instructions blocked waiting for a FIFO writer")
	}
}
