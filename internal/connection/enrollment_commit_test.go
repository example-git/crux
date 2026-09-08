package connection

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestEnrollmentTerminalStateFencesReservedPersistence(t *testing.T) {
	for _, terminal := range []string{"parent-cancel", "waiter-cancel", "expiry", "close", "attempt-limit"} {
		t.Run(terminal, func(t *testing.T) {
			setConnectionRoot(t, t.TempDir())
			_, err := EnsureServerIdentity(t.Context())
			require.NoError(t, err)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			e, err := StartEnrollment(ctx, "tcp://127.0.0.1:0", "", time.Minute)
			require.NoError(t, err)
			t.Cleanup(func() { _ = e.Close() })
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			t.Cleanup(unblock)
			e.authorizeClient = func(ctx context.Context, name, certificate string, commit authorizationCommit) error {
				return authorizeClientWithCommit(ctx, name, certificate, func(persist func() error) error {
					close(entered)
					<-release
					return commit(persist)
				})
			}
			identity, err := NewClientIdentity("candidate")
			require.NoError(t, err)
			body, err := json.Marshal(enrollmentRequest{Name: "candidate", Certificate: identity.Certificate})
			require.NoError(t, err)
			status := make(chan int, 1)
			go func() { code, _ := enrollmentRequestStatus(t.Context(), e.setup, body); status <- code }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("reservation barrier not reached")
			}
			before, err := os.ReadFile(storePath())
			require.NoError(t, err)
			var closer chan error
			switch terminal {
			case "parent-cancel":
				cancel()
			case "waiter-cancel":
				waitCtx, stop := context.WithCancel(t.Context())
				stop()
				_, err := e.Wait(waitCtx)
				require.ErrorIs(t, err, context.Canceled)
			case "expiry":
				e.expire()
			case "close":
				closer = make(chan error, 1)
				go func() { closer <- e.Close() }()
			case "attempt-limit":
				invalid := e.setup
				invalid.Token = "invalid"
				for index := range enrollmentMaxAttempts {
					code, err := enrollmentRequestStatus(t.Context(), invalid, body)
					require.NoError(t, err)
					if index == enrollmentMaxAttempts-1 {
						require.Equal(t, http.StatusTooManyRequests, code)
					} else {
						require.Equal(t, http.StatusUnauthorized, code)
					}
				}
			}
			waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			_, firstErr := e.Wait(waitCtx)
			require.Error(t, firstErr)
			require.NotErrorIs(t, firstErr, context.DeadlineExceeded)
			unblock()
			require.NotEqual(t, http.StatusCreated, <-status)
			if closer != nil {
				require.NoError(t, <-closer)
			}
			_, again := e.Wait(waitCtx)
			require.EqualError(t, again, firstErr.Error(), "terminal result must be repeatable for multiple waiters")
			after, err := os.ReadFile(storePath())
			require.NoError(t, err)
			require.Equal(t, before, after, "terminal failure must leave the authorization store byte-identical")
			authorized, err := ListAuthorizedClients(t.Context())
			require.NoError(t, err)
			require.Empty(t, authorized)
		})
	}
}

func TestEnrollmentCommittedSuccessWinsLaterCancellation(t *testing.T) {
	setConnectionRoot(t, t.TempDir())
	_, err := EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	e, err := StartEnrollment(t.Context(), "tcp://127.0.0.1:0", "", time.Minute)
	require.NoError(t, err)
	t.Cleanup(func() { _ = e.Close() })
	_, err = Pair(t.Context(), "committed", e.SetupCode())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for range 3 {
		result, err := e.Wait(ctx)
		require.NoError(t, err)
		require.Equal(t, "committed", result.Name)
	}
	authorized, err := ListAuthorizedClients(t.Context())
	require.NoError(t, err)
	require.Len(t, authorized, 1)
}
