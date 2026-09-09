package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func journalOperation(t *testing.T, w *ClientWorkspace, id string) ProviderAuthenticationHistoryOperation {
	t.Helper()
	history, err := w.ProviderAuthenticationHistory(t.Context())
	require.NoError(t, err)
	for _, operation := range history.Operations {
		if operation.OperationID == id {
			return operation
		}
	}
	t.Fatalf("operation %s absent from durable history", id)
	return ProviderAuthenticationHistoryOperation{}
}

func journalReview(t *testing.T, w *ClientWorkspace, id string) ProviderAuthenticationHistoryReview {
	t.Helper()
	history, err := w.ProviderAuthenticationHistory(t.Context())
	require.NoError(t, err)
	for _, review := range history.Reviews {
		if review.Request.ReviewID == id {
			return review
		}
	}
	t.Fatalf("review %s absent from durable history", id)
	return ProviderAuthenticationHistoryReview{}
}

func journalReincarnation(t *testing.T, f *clientAuthenticationFixture) *ClientWorkspace {
	t.Helper()
	c, err := client.NewAuthenticatedClient(t.TempDir(), f.connection)
	require.NoError(t, err)
	c.SetLocalRuntimeStore(f.store)
	proposal, err := f.store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	return w
}

func TestPublicationJournalOriginalRetirementTLS(t *testing.T) {
	for _, disposition := range []string{"rejected", "lost"} {
		t.Run(disposition, func(t *testing.T) {
			f := newClientAuthenticationFixture(t, false)
			request := providerauth.SwitchRequest{OperationID: strings.Repeat("1", 32), Target: f.target(t), AccountID: f.second.ID}
			f.putMode.Store(1)
			if disposition == "lost" {
				f.putMode.Store(2)
				f.getMode.Store(1)
			}
			original, err := f.w.SwitchProviderAccount(t.Context(), request)
			require.Error(t, err)
			require.True(t, original.Progress.ConfigSaved)
			history := journalOperation(t, f.w, request.OperationID)
			require.Equal(t, original, history.Outcome)
			require.False(t, history.RemoteAcknowledged || history.Adopted)
			paths := []string{f.path, f.accountsPath}
			infos, contents := clientAuthenticationFiles(t, paths...)
			puts := f.puts.Load()
			retire := ProviderAuthenticationAbandonRequest{WorkspaceID: f.w.workspaceID(), Target: request.Target, OperationID: request.OperationID, AbandonID: strings.Repeat("2", 32), Revision: history.JournalRevision}
			wrong := retire
			wrong.Revision++
			_, err = f.w.AbandonProviderAuthentication(t.Context(), wrong)
			require.ErrorContains(t, err, "history changed")
			outcome, err := f.w.AbandonProviderAuthentication(t.Context(), retire)
			require.NoError(t, err)
			require.NoError(t, outcome.Validate(retire))
			require.Equal(t, original, outcome.Original)
			require.False(t, outcome.RemoteAcknowledged || outcome.Adopted)
			replayed, err := f.w.AbandonProviderAuthentication(t.Context(), retire)
			require.NoError(t, err)
			require.Equal(t, outcome, replayed)
			_, err = f.w.RecoverProviderAuthentication(t.Context(), ProviderAuthenticationRecoveryRequest{OperationID: request.OperationID, Target: request.Target, RecoveryID: strings.Repeat("3", 32), RecoverySequence: 1})
			require.ErrorContains(t, err, "abandoned")
			require.Nil(t, f.w.authority.pending)
			require.False(t, f.w.authority.unacknowledgedClientAuthentication(f.w.workspaceID()))
			require.Equal(t, puts, f.puts.Load())
			requireClientAuthenticationFilesUnchanged(t, paths, infos, contents)
			f.putMode.Store(0)
			f.getMode.Store(0)
			newWorkspace := journalReincarnation(t, f)
			historical := journalOperation(t, newWorkspace, request.OperationID)
			require.True(t, historical.HistoricalWorkspace && historical.Abandoned)
			require.Equal(t, original, historical.Outcome)
			require.NotContains(t, newWorkspace.authority.authenticationReceipts, request.OperationID)
			retire.WorkspaceID = newWorkspace.workspaceID()
			replayed, err = newWorkspace.AbandonProviderAuthentication(t.Context(), retire)
			require.NoError(t, err)
			require.NoError(t, replayed.Validate(retire))
			require.Equal(t, original, replayed.Original)
			retire.AbandonID = strings.Repeat("4", 32)
			_, err = newWorkspace.AbandonProviderAuthentication(t.Context(), retire)
			require.ErrorIs(t, err, providerauth.ErrOperationConflict)
			require.Equal(t, puts, f.puts.Load())
		})
	}
}

func TestPublicationJournalFreshReviewRetirementTLS(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	saved, err := f.w.SavedProviderAuthentication(t.Context())
	require.NoError(t, err)
	target := providerauth.Target{WorkspaceID: saved.WorkspaceID, Generation: saved.Generation, Owner: providerauth.PublicOwner(f.owner)}
	review := ProviderAuthenticationReviewRequest{FreshSaved: true, SavedTarget: target, ReviewID: strings.Repeat("5", 32), ReviewSequence: 1, Choice: ProviderAuthenticationReviewChoice{Kind: "saved-account", AccountID: f.first.ID}}
	preview, err := f.w.ReviewProviderAuthentication(t.Context(), review)
	require.NoError(t, err)
	history := journalReview(t, f.w, review.ReviewID)
	require.False(t, history.ApplyAttempted)
	retire := ProviderAuthenticationReviewAbandonRequest{WorkspaceID: f.w.workspaceID(), Review: review, PreviewID: preview.PreviewID, AbandonID: strings.Repeat("7", 32), Revision: history.JournalRevision}
	_, err = f.w.AbandonProviderAuthenticationReview(t.Context(), retire)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict, "a preview with no attempted PUT is not an unknown publication")
	f.putMode.Store(1)
	apply := ProviderAuthenticationApplyRequest{FreshSaved: true, SavedTarget: target, ReviewID: review.ReviewID, PreviewID: preview.PreviewID, ApplyID: strings.Repeat("6", 32)}
	original, err := f.w.ApplyProviderAuthenticationReview(t.Context(), apply)
	require.Error(t, err)
	history = journalReview(t, f.w, review.ReviewID)
	require.True(t, history.ApplyAttempted)
	require.Equal(t, original, *history.ApplyOutcome)
	retire.Revision = history.JournalRevision
	paths := []string{f.path, f.accountsPath}
	infos, contents := clientAuthenticationFiles(t, paths...)
	outcome, err := f.w.AbandonProviderAuthenticationReview(t.Context(), retire)
	require.NoError(t, err)
	require.NoError(t, outcome.Validate(retire))
	require.Equal(t, original, outcome.Original)
	require.False(t, f.w.authority.pendingAuthenticationReview(f.w.workspaceID()))
	require.Nil(t, f.w.authority.pending)
	_, err = f.w.ApplyProviderAuthenticationReview(t.Context(), apply)
	require.ErrorIs(t, err, providerauth.ErrStale)
	require.EqualValues(t, 1, f.puts.Load())
	requireClientAuthenticationFilesUnchanged(t, paths, infos, contents)
	f.putMode.Store(0)
	newWorkspace := journalReincarnation(t, f)
	historical := journalReview(t, newWorkspace, review.ReviewID)
	require.True(t, historical.HistoricalWorkspace && historical.Abandoned)
	require.Equal(t, original, *historical.ApplyOutcome)
	retire.WorkspaceID = newWorkspace.workspaceID()
	replayed, err := newWorkspace.AbandonProviderAuthenticationReview(t.Context(), retire)
	require.NoError(t, err)
	require.NoError(t, replayed.Validate(retire))
	require.Equal(t, original, replayed.Original)
	require.False(t, newWorkspace.authority.pendingAuthenticationReview(newWorkspace.workspaceID()))
	require.EqualValues(t, 1, f.puts.Load())
}

func TestPublicationJournalSupersededPreviewAndStrictReadTLS(t *testing.T) {
	f, request := rejectedAuthenticationReviewFixture(t, false)
	first := ProviderAuthenticationReviewRequest(authenticationReviewAction(request.OperationID, request.Target, 1))
	preview, err := f.w.ReviewProviderAuthentication(t.Context(), first)
	require.NoError(t, err)
	second := ProviderAuthenticationReviewRequest(authenticationReviewAction(request.OperationID, request.Target, 2))
	_, err = f.w.ReviewProviderAuthentication(t.Context(), second)
	require.NoError(t, err)
	history := journalReview(t, f.w, first.ReviewID)
	require.Equal(t, second.ReviewID, history.SupersededByReview)
	journal := *f.w.authority.authenticationJournal
	entry, found, err := journal.Load(t.Context(), f.w.authority.authenticationJournalKey(f.w.workspaceID(), "review", first.ReviewID))
	require.NoError(t, err)
	require.True(t, found && entry.Completed())
	require.Zero(t, entry.ReservedBytes())
	_, err = f.w.ApplyProviderAuthenticationReview(t.Context(), ProviderAuthenticationApplyRequest(authenticationReviewApply(clientAuthenticationReviewRequest(first), preview)))
	require.ErrorIs(t, err, providerauth.ErrStale)
	key := f.w.authority.authenticationJournalKey(f.w.workspaceID(), "review", second.ReviewID)
	entry, found, err = journal.Load(t.Context(), key)
	require.NoError(t, err)
	require.True(t, found)
	invalid := strings.Replace(string(entry.Payload()), `"Version":1`, `"Version":1,"Version":1`, 1)
	require.NotEqual(t, string(entry.Payload()), invalid)
	_, err = journal.Store(t.Context(), key, entry.Revision(), json.RawMessage(invalid), false)
	require.Error(t, err, "the writer rejects duplicate fields before persistence")
	_, err = f.w.ProviderAuthenticationHistory(t.Context())
	require.NoError(t, err, "rejected writes preserve the prior readable history")
	invalid = strings.Replace(string(entry.Payload()), `"Version":1`, `"Version":1,"Unexpected":true`, 1)
	_, err = journal.Store(t.Context(), key, entry.Revision(), json.RawMessage(invalid), false)
	require.NoError(t, err, "the shared envelope leaves typed field validation to the owner")
	_, err = f.w.ProviderAuthenticationHistory(t.Context())
	require.Error(t, err, "typed reader rejects unknown fields before exposing history")
	require.NotContains(t, err.Error(), f.first.AccessToken)
	require.EqualValues(t, 1, f.puts.Load())
}

func TestPublicationJournalOAuthWorkerFencesRetirementTLS(t *testing.T) {
	var exchanges atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"journal-worker-token","refresh_token":"journal-worker-refresh","expires_in":3600}`)
	}))
	defer host.Close()
	f := newWorkspaceOAuthFixture(t, host, "hosted-paste")
	authorizeWorkspaceOAuth(t, f, "hosted-paste")
	peer := NewClientWorkspace(f.w.client, f.w.cached())
	t.Cleanup(peer.Shutdown)
	_, err := peer.ProviderAuthenticationHistory(t.Context())
	require.NoError(t, err)
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
	type completion struct {
		outcome providerauth.MutationOutcome
		err     error
	}
	done := make(chan completion, 1)
	go func() { outcome, err := f.w.CompleteProviderOAuthLogin(ctx, f.ref); done <- completion{outcome, err} }()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("commit never admitted")
	}
	intent := journalOperation(t, peer, f.ref.OperationID)
	require.False(t, intent.LocalFinished)
	cancel()
	retire := ProviderAuthenticationAbandonRequest{WorkspaceID: peer.workspaceID(), Target: f.ref.Target, OperationID: f.ref.OperationID, AbandonID: strings.Repeat("8", 32), Revision: intent.JournalRevision}
	blocked, stop := context.WithTimeout(t.Context(), 150*time.Millisecond)
	_, err = peer.AbandonProviderAuthentication(blocked, retire)
	stop()
	require.ErrorIs(t, err, context.DeadlineExceeded, "another authority cannot retire an active local writer")
	close(release)
	var completed completion
	select {
	case completed = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker did not finish")
	}
	require.ErrorIs(t, completed.err, context.Canceled)
	require.True(t, completed.outcome.Progress.ConfigSaved)
	history := journalOperation(t, peer, f.ref.OperationID)
	require.True(t, history.LocalFinished)
	retire.Revision = history.JournalRevision
	outcome, err := peer.AbandonProviderAuthentication(t.Context(), retire)
	require.NoError(t, err)
	require.Equal(t, completed.outcome, outcome.Original)
	require.EqualValues(t, 1, exchanges.Load())
	require.Zero(t, f.transport.puts.Load())
	data, err := os.ReadFile(filepath.Join(f.root, "accounts", "accounts.json"))
	require.NoError(t, err)
	require.Contains(t, string(data), "journal-worker-token")
}
