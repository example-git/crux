package cmd

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAuthenticationConsoleCancelsPipeWithoutClosingIt(t *testing.T) {
	t.Parallel()

	input, writer, err := os.Pipe()
	require.NoError(t, err)
	defer input.Close()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	console, closeInput, err := newAuthenticationConsole(ctx, input, io.Discard, nil, nil)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		_, err := console.readLine(ctx)
		done <- err
	}()
	timer := time.AfterFunc(50*time.Millisecond, cancel)
	defer timer.Stop()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("authentication pipe read ignored cancellation")
	}
	closeInput()
	_, err = writer.Write([]byte("still open\n"))
	require.NoError(t, err)
	reused, closeReused, err := newAuthenticationConsole(t.Context(), input, io.Discard, nil, nil)
	require.NoError(t, err)
	defer closeReused()
	line, err := reused.readLine(t.Context())
	require.NoError(t, err)
	require.Equal(t, "still open", line)
}
