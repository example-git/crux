package providerauth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOAuthLoginServiceCancelAndCloseCancelActualExchange(t *testing.T) {
	for _, closeOwner := range []bool{false, true} {
		t.Run(fmt.Sprintf("close=%t", closeOwner), func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			var calls atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				if calls.Add(1) == 1 {
					close(entered)
					<-r.Context().Done()
					close(canceled)
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"peer-token","refresh_token":"peer-refresh","expires_in":120}`))
			}))
			defer host.Close()
			f := newOAuthServiceFixture(t, host, "loopback-dynamic")
			_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
			require.NoError(t, err)
			submission := submitOAuthServiceCode(t, f, "loopback-dynamic")
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("exchange did not reach HTTPS endpoint")
			}
			root := filepath.Dir(filepath.Dir(f.path))
			files := authenticationInputsTree(t, root)
			if closeOwner {
				f.service.Close()
			} else {
				state, err := f.service.CancelOAuthLogin(t.Context(), f.ref)
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, OAuthLoginCanceled, state.Phase)
			}
			select {
			case <-canceled:
			case <-time.After(5 * time.Second):
				t.Fatal("owner cancellation did not cancel actual HTTPS exchange")
			}
			require.Equal(t, files, authenticationInputsTree(t, root), "canceled login has no late account/config write")
			if !closeOwner {
				state, err := f.service.SubmitOAuthLoginCode(t.Context(), submission)
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, OAuthLoginCanceled, state.Phase)
			}
			_, err = f.service.CompleteOAuthLogin(t.Context(), f.ref)
			require.ErrorIs(t, err, context.Canceled)
			require.EqualValues(t, 1, calls.Load())
			// An independent owning service still exchanges and commits normally.
			peer := newOAuthServiceFixture(t, host, "loopback-dynamic")
			_, err = peer.service.BeginOAuthLogin(t.Context(), peer.ref)
			require.NoError(t, err)
			submitOAuthServiceCode(t, peer, "loopback-dynamic")
			awaitOAuthServicePhase(t, peer, OAuthLoginAuthorized)
			result, err := peer.service.CompleteOAuthLogin(t.Context(), peer.ref)
			require.NoError(t, err)
			require.True(t, result.Outcome.Progress.RuntimePublished)
			require.EqualValues(t, 2, calls.Load())
		})
	}
}

func TestOAuthLoginServiceEvictionPublishesCancellation(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("idle browser login must not issue HTTPS request")
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "loopback-dynamic")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	state := awaitOAuthServicePhase(t, f, OAuthLoginWaitingLoopback)
	require.NoError(t, f.service.acquire(t.Context()))
	oldest := f.service.logins[f.ref.LoginID]
	oldest.mu.Lock()
	changed := oldest.changed
	oldest.mu.Unlock()
	// Fill the bounded receipt cache with inactive sessions. The oldest entry
	// remains a real admitted/prepared browser login; the next Begin evicts it.
	for i := 1; i < mutationReceiptLimit; i++ {
		ref := f.ref
		ref.LoginID = fmt.Sprintf("%032x", i)
		ref.OperationID = fmt.Sprintf("%032x", 1000+i)
		ctx, cancel := context.WithCancel(f.service.lifetime)
		login := &oauthLoginSession{ctx: ctx, cancel: cancel, state: OAuthLoginState{Login: ref, Sequence: 1, Phase: OAuthLoginCanceled}, changed: make(chan struct{})}
		f.service.logins[ref.LoginID] = login
		f.service.loginIDs = append(f.service.loginIDs, ref.LoginID)
	}
	<-f.service.gate
	snapshot, err := f.service.Status(t.Context())
	require.NoError(t, err)
	next := f.ref
	next.LoginID = strings.Repeat("e", 32)
	next.OperationID = strings.Repeat("f", 32)
	next.Target.Generation = snapshot.Generation
	_, err = f.service.BeginOAuthLogin(t.Context(), next)
	require.NoError(t, err)
	select {
	case <-changed:
	case <-time.After(time.Second):
		t.Fatal("eviction did not wake existing state waiters")
	}
	canceled, err := oldest.snapshot()
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, OAuthLoginCanceled, canceled.Phase)
	require.Greater(t, canceled.Sequence, state.Sequence)
	_, err = f.service.WaitOAuthLogin(t.Context(), f.ref, 0)
	require.ErrorIs(t, err, ErrOAuthLoginUnavailable)
}

func TestOAuthLoginServiceCloseJoinsAdmittedConfigurationPreparation(t *testing.T) {
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"synthetic-new","refresh_token":"synthetic-refresh","expires_in":120}`))
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "loopback-dynamic")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "loopback-dynamic")
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	root := filepath.Dir(filepath.Dir(f.path))
	originalConfig := f.store.Config()
	journal, err := f.store.CaptureAuthenticationJournal(t.Context())
	require.NoError(t, err)
	localKey := config.AuthenticationJournalKey{Kind: config.AuthenticationJournalLocal, WorkspaceID: f.ref.Target.WorkspaceID, OperationID: f.ref.OperationID}
	oauthKey := localKey
	oauthKey.Kind = config.AuthenticationJournalOAuth
	oauthBefore, found, err := journal.Load(t.Context(), oauthKey)
	require.NoError(t, err)
	require.True(t, found)
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
	completion := make(chan error, 1)
	go func() { _, err := f.service.CompleteOAuthLogin(t.Context(), f.ref); completion <- err }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("commit not admitted")
	}
	// Preparation has acquired its ordinary coordination locks and journaled
	// intent. Cancellation must finish that record as no-effects before Close
	// returns, while preserving credentials, configuration, and runtime.
	files := authenticationInputsTree(t, root)
	localBefore, found, err := journal.Load(t.Context(), localKey)
	require.NoError(t, err)
	require.True(t, found)
	require.False(t, localBefore.Completed())
	closed := make(chan struct{})
	go func() { f.service.Close(); close(closed) }()
	select {
	case <-f.service.lifetime.Done():
	case <-time.After(time.Second):
		t.Fatal("close did not cancel owner")
	}
	select {
	case <-closed:
		t.Fatal("close returned while configuration preparation was still running")
	default:
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not join released commit")
	}
	select {
	case err := <-completion:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("completion did not return after Close joined its worker")
	}
	require.Same(t, originalConfig, f.store.Config())
	localAfter, found, err := journal.Load(t.Context(), localKey)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, localAfter.Completed())
	require.Equal(t, localBefore.Revision()+1, localAfter.Revision())
	var beforePayload, afterPayload map[string]any
	require.NoError(t, json.Unmarshal(localBefore.Payload(), &beforePayload))
	require.NoError(t, json.Unmarshal(localAfter.Payload(), &afterPayload))
	require.Equal(t, false, beforePayload["finished"])
	require.Equal(t, false, beforePayload["no_effects"])
	require.Equal(t, true, afterPayload["finished"])
	require.Equal(t, true, afterPayload["no_effects"])
	afterPayload["finished"], afterPayload["no_effects"] = false, false
	require.Equal(t, beforePayload, afterPayload, "only the terminal no-effects disposition may change")
	local, found, err := f.store.LoadAuthenticationLocalChange(t.Context(), localKey)
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, local.Summary().NoEffects)
	require.Equal(t, config.LocalAuthenticationProgress{}, local.Summary().Original)
	oauthAfter, found, err := journal.Load(t.Context(), oauthKey)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, oauthBefore, oauthAfter, "the observed OAuth result remains available for explicit recovery")
	afterClose := authenticationInputsTree(t, root)
	require.Len(t, afterClose, len(files))
	journalPath := f.path + ".authentication-journal.json"
	for path, before := range files {
		after, found := afterClose[path]
		require.True(t, found, path)
		switch path {
		case journalPath:
			require.Equal(t, before.mode, after.mode, "terminal bookkeeping must retain private file mode")
		case filepath.Dir(journalPath):
			after.mtime = before.mtime // Atomic replacement changes only the parent directory timestamp.
			require.Equal(t, before, after, path)
		default:
			require.Equal(t, before, after, "no account/config/input write is permitted: %s", path)
		}
	}
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, afterClose, authenticationInputsTree(t, root), "no write, including journal bookkeeping, may outlive Close")
}
