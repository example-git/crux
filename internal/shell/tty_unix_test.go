//go:build !windows

package shell

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/stretchr/testify/require"
)

func TestExecTTYProvidesTerminalAndInput(t *testing.T) {
	hostMaster, hostTerminal, err := pty.Open()
	require.NoError(t, err)
	defer hostMaster.Close()
	defer hostTerminal.Close()

	var captured bytes.Buffer
	commandShell := NewShell(&Options{WorkingDir: t.TempDir()})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- commandShell.ExecTTY(
			ctx,
			`sh -c 'test -t 0 && printf "tty=yes\n"; IFS= read -r value; printf "input=%s\n" "$value"'`,
			hostTerminal,
			io.MultiWriter(hostTerminal, &captured),
		)
	}()

	time.Sleep(100 * time.Millisecond)
	_, err = hostMaster.Write([]byte("verified-input\n"))
	require.NoError(t, err)
	require.NoError(t, <-done)
	require.Contains(t, captured.String(), "tty=yes")
	require.Contains(t, captured.String(), "input=verified-input")
}
