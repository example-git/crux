package providerauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOAuthCompletionAdmissionRetryDoesNotExchangeAgain(t *testing.T) {
	var exchanges, admitted, released atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"admission-token","refresh_token":"admission-refresh","expires_in":3600}`)
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "hosted-paste")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "hosted-paste")
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	refusal := errors.New("disposable admission unavailable")
	ctx := WithOAuthCompletionAdmission(t.Context(), func(context.Context) (func(), error) {
		if admitted.Add(1) == 1 {
			return nil, refusal
		}
		return func() { released.Add(1) }, nil
	})
	require.NoError(t, f.service.acquire(t.Context()))
	waiting, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	preflight, preflightErr := f.service.CompleteOAuthLogin(waiting, f.ref)
	stop()
	<-f.service.gate
	require.ErrorIs(t, preflightErr, context.DeadlineExceeded)
	require.Zero(t, preflight.Outcome.Progress)
	require.Zero(t, admitted.Load(), "a canceled service gate wait must not invoke durable admission")
	require.NotContains(t, f.service.receipts, f.ref.OperationID)

	initial, err := f.service.CompleteOAuthLogin(ctx, f.ref)
	require.ErrorIs(t, err, refusal)
	require.False(t, initial.LocalCommitPending())
	_, hasOwner := initial.OriginalOwner()
	require.False(t, hasOwner)
	require.Zero(t, initial.Outcome.Progress)
	require.NotContains(t, f.service.receipts, f.ref.OperationID)
	state, err := f.service.WaitOAuthLogin(t.Context(), f.ref, 0)
	require.NoError(t, err)
	require.Equal(t, OAuthLoginAuthorized, state.Phase)
	completed, err := f.service.CompleteOAuthLogin(ctx, f.ref)
	require.NoError(t, err)
	require.True(t, completed.Outcome.Progress.ConfigSaved && completed.Outcome.Progress.RuntimePublished)
	replayed, err := f.service.CompleteOAuthLogin(ctx, f.ref)
	require.NoError(t, err)
	require.Equal(t, completed.Outcome, replayed.Outcome)
	require.EqualValues(t, 2, admitted.Load())
	require.Eventually(t, func() bool { return released.Load() == 1 }, time.Second, time.Millisecond)
	require.EqualValues(t, 1, exchanges.Load())
}

func TestOAuthCompletionDetachedWorkerRetainsAdmissionLease(t *testing.T) {
	var exchanges atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"detached-token","refresh_token":"detached-refresh","expires_in":3600}`)
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "hosted-paste")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "hosted-paste")
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	journal, err := f.store.CaptureAuthenticationJournal(t.Context())
	require.NoError(t, err)
	key := config.AuthenticationJournalKey{Kind: config.AuthenticationJournalClient, WorkspaceID: f.ref.Target.WorkspaceID, OperationID: strings.Repeat("9", 32)}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	f.store.SetRuntimeGenerationPreparer(func(context.Context, config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		close(entered)
		<-release
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() {}}, nil
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx = WithOAuthCompletionAdmission(ctx, func(admission context.Context) (func(), error) { return journal.AcquireOperation(admission, key) })
	type completion struct {
		result MutationResult
		err    error
	}
	done := make(chan completion, 1)
	go func() { result, err := f.service.CompleteOAuthLogin(ctx, f.ref); done <- completion{result, err} }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("worker never admitted")
	}
	cancel()
	var interrupted completion
	select {
	case interrupted = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("caller wait did not cancel")
	}
	require.ErrorIs(t, interrupted.err, context.Canceled)
	require.True(t, interrupted.result.LocalCommitPending())
	require.Zero(t, interrupted.result.Outcome.Progress)
	waiting, stop := context.WithTimeout(t.Context(), 150*time.Millisecond)
	earlyRelease, err := journal.AcquireOperation(waiting, key)
	stop()
	if earlyRelease != nil {
		earlyRelease()
	}
	require.ErrorIs(t, err, context.DeadlineExceeded, "canceled caller must not release the active worker's lease")
	close(release)
	completed, err := f.service.CompleteOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	require.True(t, completed.Outcome.Progress.ConfigSaved)
	freed, err := journal.AcquireOperation(t.Context(), key)
	require.NoError(t, err)
	freed()
	require.EqualValues(t, 1, exchanges.Load())
}
