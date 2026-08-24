package task

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func newOutputStoreForTest(t *testing.T, options OutputStoreOptions) *OutputStore {
	t.Helper()
	store, err := NewOutputStore(filepath.Join(t.TempDir(), "tasks", "output"), options)
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestOutputStoreCreateAndReadStreams(t *testing.T) {
	t.Parallel()

	store := newOutputStoreForTest(t, OutputStoreOptions{})
	output, err := store.Create("b12345678")
	require.NoError(t, err)
	_, err = output.Stdout().Write([]byte("out-one\n"))
	require.NoError(t, err)
	_, err = output.Stderr().Write([]byte("err-one\n"))
	require.NoError(t, err)
	_, err = output.Stdout().Write([]byte("out-two\n"))
	require.NoError(t, err)
	require.NoError(t, output.Close())

	stdout, err := store.Read(output.Ref(), ReadOptions{Stream: OutputStreamStdout})
	require.NoError(t, err)
	require.Equal(t, "out-one\nout-two\n", string(stdout.Output))
	stderr, err := store.Read(output.Ref(), ReadOptions{Stream: OutputStreamStderr})
	require.NoError(t, err)
	require.Equal(t, "err-one\n", string(stderr.Output))
	merged, err := store.Read(output.Ref(), ReadOptions{})
	require.NoError(t, err)
	require.Equal(t, "out-one\nerr-one\nout-two\n", string(merged.Output))
	require.Equal(t, int64(len(merged.Output)), merged.Metadata.OutputBytes)
	require.NotZero(t, merged.Metadata.ClosedAt)

	if runtime.GOOS != "windows" {
		rootInfo, err := os.Stat(store.Root())
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), rootInfo.Mode().Perm())
		for _, name := range outputNames("b12345678") {
			info, err := os.Stat(filepath.Join(store.Root(), name))
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
		}
	}
}

func TestOutputStoreEnforcesExactCombinedLimit(t *testing.T) {
	t.Parallel()

	store := newOutputStoreForTest(t, OutputStoreOptions{MaxOutputBytes: 10})
	output, err := store.Create("b12345678")
	require.NoError(t, err)
	written, err := output.Stdout().Write([]byte("12345678"))
	require.NoError(t, err)
	require.Equal(t, 8, written)
	written, err = output.Stderr().Write([]byte("abcdef"))
	require.ErrorIs(t, err, ErrOutputLimitExceeded)
	require.Equal(t, 2, written)
	written, err = output.Stdout().Write([]byte("x"))
	require.ErrorIs(t, err, ErrOutputLimitExceeded)
	require.Zero(t, written)
	require.NoError(t, output.Close())

	stdout, err := store.Read(output.Ref(), ReadOptions{Stream: OutputStreamStdout})
	require.NoError(t, err)
	require.Equal(t, "12345678", string(stdout.Output))
	stderr, err := store.Read(output.Ref(), ReadOptions{Stream: OutputStreamStderr})
	require.NoError(t, err)
	require.Equal(t, "ab", string(stderr.Output))
	merged, err := store.Read(output.Ref(), ReadOptions{})
	require.NoError(t, err)
	require.Equal(t, "12345678ab", string(merged.Output))
	require.True(t, merged.OutputTruncated)
	require.Equal(t, int64(10), merged.Metadata.OutputBytes)
}

func TestOutputStoreRangeAndTailReads(t *testing.T) {
	t.Parallel()

	store := newOutputStoreForTest(t, OutputStoreOptions{})
	output, err := store.Create("b12345678")
	require.NoError(t, err)
	_, err = output.Stdout().Write([]byte("0123456789"))
	require.NoError(t, err)
	require.NoError(t, output.Close())

	offset := int64(3)
	result, err := store.Read(output.Ref(), ReadOptions{Offset: &offset, MaxBytes: 4})
	require.NoError(t, err)
	require.Equal(t, "3456", string(result.Output))
	require.Equal(t, int64(7), result.NextOffset)
	tail := int64(3)
	result, err = store.Read(output.Ref(), ReadOptions{TailBytes: &tail})
	require.NoError(t, err)
	require.Equal(t, "789", string(result.Output))
	require.Equal(t, int64(10), result.NextOffset)
	beyond := int64(100)
	result, err = store.Read(output.Ref(), ReadOptions{Offset: &beyond})
	require.NoError(t, err)
	require.Empty(t, result.Output)
	require.Equal(t, int64(10), result.NextOffset)

	negative := int64(-1)
	_, err = store.Read(output.Ref(), ReadOptions{Offset: &negative})
	require.EqualError(t, err, "offset must not be negative")
	_, err = store.Read(output.Ref(), ReadOptions{Offset: &offset, TailBytes: &tail})
	require.EqualError(t, err, "offset and tail_bytes are mutually exclusive")
	_, err = store.Read(output.Ref(), ReadOptions{MaxBytes: MaxReadBytes + 1})
	require.ErrorContains(t, err, "max_bytes must be between")
}

func TestOutputStoreRejectsInvalidReferencesAndCollisions(t *testing.T) {
	t.Parallel()

	store := newOutputStoreForTest(t, OutputStoreOptions{})
	_, err := store.Create("../../escape")
	require.Error(t, err)
	_, err = store.Read("task-output:../../escape", ReadOptions{})
	require.Error(t, err)
	_, err = store.Read("file:b12345678", ReadOptions{})
	require.EqualError(t, err, "invalid task output reference")

	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.WriteFile(target, []byte("protected"), 0o600))
	if runtime.GOOS != "windows" {
		require.NoError(t, os.Symlink(target, filepath.Join(store.Root(), outputName("b12345678", OutputStreamStdout))))
		_, err = store.Create("b12345678")
		require.Error(t, err)
		contents, readErr := os.ReadFile(target)
		require.NoError(t, readErr)
		require.Equal(t, "protected", string(contents))
	}
}

func TestOutputStoreCleanupUsesClosedMetadata(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_800_000_000, 0)
	store := newOutputStoreForTest(t, OutputStoreOptions{Now: func() time.Time { return now }})
	closed, err := store.Create("b12345678")
	require.NoError(t, err)
	require.NoError(t, closed.Close())
	open, err := store.Create("babcdefgh")
	require.NoError(t, err)
	t.Cleanup(func() { _ = open.Close() })

	removed, err := store.Cleanup(now, DefaultCleanupLimit)
	require.NoError(t, err)
	require.Zero(t, removed)
	removed, err = store.Cleanup(now.Add(time.Millisecond), 1)
	require.NoError(t, err)
	require.Equal(t, 1, removed)
	_, err = store.Read(closed.Ref(), ReadOptions{})
	require.Error(t, err)
	_, err = store.Read(open.Ref(), ReadOptions{})
	require.NoError(t, err)
}

func TestOutputStoreConcurrentStreamsRemainBounded(t *testing.T) {
	t.Parallel()

	const writes = 100
	store := newOutputStoreForTest(t, OutputStoreOptions{MaxOutputBytes: 2 * writes})
	output, err := store.Create("b12345678")
	require.NoError(t, err)
	var waitGroup sync.WaitGroup
	for _, writer := range []interface{ Write([]byte) (int, error) }{output.Stdout(), output.Stderr()} {
		waitGroup.Go(func() {
			for range writes {
				written, writeErr := writer.Write([]byte("x"))
				require.NoError(t, writeErr)
				require.Equal(t, 1, written)
			}
		})
	}
	waitGroup.Wait()
	require.NoError(t, output.Close())
	result, err := store.Read(output.Ref(), ReadOptions{})
	require.NoError(t, err)
	require.Len(t, result.Output, 2*writes)
	require.True(t, bytes.Equal(result.Output, bytes.Repeat([]byte("x"), 2*writes)))
	require.Equal(t, int64(2*writes), result.Metadata.OutputBytes)
}

func TestWaitForOutput(t *testing.T) {
	t.Parallel()

	done := make(chan struct{})
	status, err := WaitForOutput(t.Context(), done, false, 0)
	require.NoError(t, err)
	require.Equal(t, RetrievalNotReady, status)
	status, err = WaitForOutput(t.Context(), done, true, 0)
	require.NoError(t, err)
	require.Equal(t, RetrievalTimeout, status)

	close(done)
	status, err = WaitForOutput(t.Context(), done, true, DefaultOutputWait)
	require.NoError(t, err)
	require.Equal(t, RetrievalReady, status)

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = WaitForOutput(canceled, make(chan struct{}), true, time.Minute)
	require.ErrorIs(t, err, context.Canceled)
	_, err = WaitForOutput(t.Context(), make(chan struct{}), true, MaxOutputWait+time.Millisecond)
	require.Error(t, err)
}

func TestOutputStoreInvalidOptions(t *testing.T) {
	t.Parallel()

	_, err := NewOutputStore("relative", OutputStoreOptions{})
	require.EqualError(t, err, "task output root must be absolute")
	_, err = NewOutputStore(filepath.Join(t.TempDir(), "output"), OutputStoreOptions{MaxOutputBytes: MaxOutputBytes + 1})
	require.Error(t, err)
	require.True(t, errors.Is(ErrOutputLimitExceeded, ErrOutputLimitExceeded))
}
