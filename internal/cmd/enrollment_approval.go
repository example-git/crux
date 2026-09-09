package cmd

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/example-git/crux/internal/connection"
	"github.com/muesli/cancelreader"
	"github.com/spf13/cobra"
)

func commandEnrollmentApprover(command *cobra.Command) connection.EnrollmentApprover {
	return func(ctx context.Context, candidate connection.EnrollmentCandidate) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		endpoint := candidate.Endpoint
		if endpoint == "" {
			endpoint = "local authorization (no enrollment endpoint)"
		}
		expiry := "this local command"
		if !candidate.ExpiresAt.IsZero() {
			expiry = candidate.ExpiresAt.UTC().Format(time.RFC3339)
		}
		if _, err := fmt.Fprintf(command.ErrOrStderr(), "\nClient: %q\nClient fingerprint (SHA-256): %s\nServer fingerprint (SHA-256): %s\nEndpoint: %s\nExpires: %s\nAuthorize this exact client? [y/N] ", candidate.ClientName, candidate.ClientFingerprint, candidate.ServerFingerprint, endpoint, expiry); err != nil {
			return err
		}
		answer, err := readEnrollmentApproval(ctx, command.InOrStdin())
		if err != nil {
			return err
		}
		if answer != "y" && answer != "yes" {
			return connection.ErrEnrollmentApprovalDenied
		}
		return ctx.Err()
	}
}

// Interactive file descriptors use the same cancelable reader as the terminal
// stack. Join its cancellation before closing its resources; stdin itself stays
// open. Finite in-memory inputs and regular files need no background reader.
func readEnrollmentApproval(ctx context.Context, input io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	reader := input
	closeReader := func() {}
	switch value := input.(type) {
	case *bytes.Buffer, *bytes.Reader, *strings.Reader:
	case *os.File:
		info, err := value.Stat()
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() && value.SetReadDeadline(time.Time{}) == nil {
			finished := make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				_ = value.SetReadDeadline(time.Now())
				close(finished)
			})
			closeReader = func() {
				if !stop() {
					<-finished
				}
				_ = value.SetReadDeadline(time.Time{})
			}
		} else if !info.Mode().IsRegular() {
			// cancelreader's Windows fallback cannot interrupt pipe reads and
			// its console implementation opens CONIN$ rather than the supplied
			// pipe. Never silently replace redirected input with the console.
			switch runtime.GOOS {
			case "windows":
				if value.Fd() != os.Stdin.Fd() || !term.IsTerminal(value.Fd()) {
					return "", errors.New("approval input cannot be canceled; use an interactive terminal or a regular input file")
				}
			case "darwin", "linux", "solaris", "freebsd", "netbsd", "openbsd", "dragonfly":
			default:
				return "", errors.New("approval input does not support cancellation on this platform")
			}
			canceled, err := cancelreader.NewReader(value)
			if err != nil {
				return "", err
			}
			finished := make(chan struct{})
			stop := context.AfterFunc(ctx, func() { canceled.Cancel(); close(finished) })
			closeReader = func() {
				if !stop() {
					<-finished
				}
				_ = canceled.Close()
			}
			reader = canceled
		}
	case *io.PipeReader:
		finished := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { _ = value.CloseWithError(ctx.Err()); close(finished) })
		closeReader = func() {
			if !stop() {
				<-finished
			}
		}
	default:
		return "", errors.New("approval input does not support cancellation")
	}
	defer closeReader()
	answer, err := bufio.NewReader(io.LimitReader(reader, 129)).ReadString('\n')
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if len(answer) > 128 {
		return "", errors.New("approval response is too long")
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(answer)), nil
}
