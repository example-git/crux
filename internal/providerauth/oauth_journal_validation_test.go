package providerauth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/stretchr/testify/require"
)

func oauthJournalValidationTarget(t *testing.T, service *Service, owner Owner) Target {
	t.Helper()
	snapshot, err := service.Status(t.Context())
	require.NoError(t, err)
	return Target{WorkspaceID: snapshot.WorkspaceID, Generation: snapshot.Generation, Owner: owner}
}

func oauthJournalValidationReload(t *testing.T, f oauthServiceFixture) *config.ConfigStore {
	t.Helper()
	root := filepath.Dir(filepath.Dir(f.path))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfilePluginNative), "CRUX_PROVIDER_PLUGINS": "example-responses"}
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	return store
}

func submitOAuthJournalValidationFailingCode(t *testing.T, f oauthServiceFixture) {
	t.Helper()
	state := awaitOAuthServicePhase(t, f, OAuthLoginWaitingCode)
	uri, err := url.Parse(state.AuthorizationURL)
	require.NoError(t, err)
	request := OAuthLoginCodeRequest{Login: f.ref, SubmissionID: strings.Repeat("d", 32), Input: url.Values{"code": {"synthetic-code"}, "state": {uri.Query().Get("state")}}.Encode()}
	state, err = f.service.SubmitOAuthLoginCode(t.Context(), request)
	if err != nil {
		require.Equal(t, f.ref, state.Login)
		require.Equal(t, OAuthLoginFailed, state.Phase, "a fast admitted failure may be returned by Submit itself")
	}
}

func TestOAuthJournalValidationServiceRestartRecoveryAndExactLink(t *testing.T) {
	var calls atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/token" {
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			http.Error(w, "bad endpoint", 400)
			return
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("code") != "synthetic-code" || r.Form.Get("client_id") != "synthetic-client" {
			t.Errorf("unexpected synthetic exchange form")
		}
		_, _ = w.Write([]byte(`{"access_token":"journal-synthetic-access","refresh_token":"journal-synthetic-refresh","expires_in":120}`))
	}))
	defer host.Close()
	f := newOAuthServiceFixture(t, host, "hosted-paste")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "hosted-paste")
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	f.service.Close()
	store := oauthJournalValidationReload(t, f)
	peer := NewWithContext(t.Context(), store, "restarted-workspace")
	defer peer.Close()
	target := oauthJournalValidationTarget(t, peer, f.ref.Target.Owner)
	list, err := peer.ListOAuthLoginResults(t.Context(), target)
	require.NoError(t, err)
	require.NoError(t, list.Validate())
	require.Len(t, list.Results, 1)
	require.Equal(t, f.ref.Target.WorkspaceID, list.Results[0].OriginalWorkspaceID)
	require.Equal(t, "token-result-recorded", list.Results[0].State)
	encoded, err := json.Marshal(list)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "journal-synthetic")
	require.NotContains(t, string(encoded), `"account_namespace"`)
	fresh := OAuthLoginRequest{Target: target, LoginID: strings.Repeat("e", 32), OperationID: strings.Repeat("f", 32)}
	request := OAuthLoginRecoveryRequest{Login: fresh, OriginalWorkspaceID: f.ref.Target.WorkspaceID, OriginalOperationID: f.ref.OperationID}
	_, err = peer.RecoverOAuthLogin(t.Context(), request)
	require.NoError(t, err)
	copy := f
	copy.service = peer
	copy.store = store
	copy.ref = fresh
	state := awaitOAuthServicePhase(t, copy, OAuthLoginAuthorized)
	require.True(t, state.MatchesOAuthLoginRecovery(request.OriginalWorkspaceID, request.OriginalOperationID))
	_, err = peer.RecoverOAuthLogin(t.Context(), request)
	require.NoError(t, err)
	wrong := request
	wrong.OriginalWorkspaceID = "wrong-workspace"
	_, err = peer.RecoverOAuthLogin(t.Context(), wrong)
	require.ErrorIs(t, err, ErrOperationConflict)
	result, err := peer.CompleteOAuthLogin(t.Context(), fresh)
	require.NoError(t, err)
	require.True(t, result.Outcome.Progress.ConfigSaved)
	require.True(t, result.Outcome.Progress.RuntimePublished)
	require.EqualValues(t, 1, calls.Load(), "recovery must not repeat HTTPS exchange")
	provider, ok := store.Config().Providers.Get("example-responses")
	require.True(t, ok)
	require.Equal(t, "journal-synthetic-access", provider.OAuthToken.AccessToken)
	require.Equal(t, "journal-synthetic-refresh", provider.OAuthToken.RefreshToken)
	remaining, err := peer.ListOAuthLoginResults(t.Context(), oauthJournalValidationTarget(t, peer, f.ref.Target.Owner))
	require.NoError(t, err)
	require.Empty(t, remaining.Results, "committed original token result must release its pending reservation")
}

func TestOAuthJournalValidationServiceRetiresUnstartedAndUnknown(t *testing.T) {
	for _, started := range []bool{false, true} {
		t.Run(map[bool]string{false: "not-started", true: "unknown"}[started], func(t *testing.T) {
			var calls atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				http.Error(w, `{"error":"synthetic-rejection"}`, 400)
			}))
			defer host.Close()
			f := newOAuthServiceFixture(t, host, "hosted-paste")
			_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
			require.NoError(t, err)
			awaitOAuthServicePhase(t, f, OAuthLoginWaitingCode)
			if started {
				submitOAuthJournalValidationFailingCode(t, f)
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var sequence uint64
				for {
					state, err := f.service.WaitOAuthLogin(ctx, f.ref, sequence)
					if oauthLoginTerminal(state.Phase) {
						require.Error(t, err)
						break
					}
					require.NoError(t, err)
					sequence = state.Sequence
				}
			}
			target := oauthJournalValidationTarget(t, f.service, f.ref.Target.Owner)
			list, err := f.service.ListOAuthLoginResults(t.Context(), target)
			require.NoError(t, err)
			require.Len(t, list.Results, 1)
			request := OAuthLoginAbandonRequest{Target: target, OriginalWorkspaceID: f.ref.Target.WorkspaceID, OriginalOperationID: f.ref.OperationID}
			outcome, err := f.service.AbandonOAuthLoginResult(t.Context(), request)
			require.NoError(t, err)
			require.NoError(t, outcome.Validate(request))
			require.True(t, outcome.Abandoned)
			require.Equal(t, map[bool]string{false: "not-started", true: "unknown"}[started], outcome.ExchangeOutcome)
			again, err := f.service.AbandonOAuthLoginResult(t.Context(), request)
			require.NoError(t, err)
			require.Equal(t, outcome, again)
			if !started {
				submitOAuthJournalValidationFailingCode(t, f)
				// Retired preparation cannot start its old exchange.
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				var sequence uint64
				for {
					state, err := f.service.WaitOAuthLogin(ctx, f.ref, sequence)
					if oauthLoginTerminal(state.Phase) {
						require.Error(t, err)
						break
					}
					require.NoError(t, err)
					sequence = state.Sequence
				}
			}
			require.EqualValues(t, map[bool]int{false: 0, true: 1}[started], calls.Load())
		})
	}
}

func TestOAuthJournalValidationActiveHTTPSExchangeFencesRetirement(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
			_, _ = w.Write([]byte(`{"access_token":"held-access","refresh_token":"held-refresh","expires_in":120}`))
		case <-r.Context().Done():
		}
	}))
	defer host.Close()
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()

	f := newOAuthServiceFixture(t, host, "hosted-paste")
	_, err := f.service.BeginOAuthLogin(t.Context(), f.ref)
	require.NoError(t, err)
	submitOAuthServiceCode(t, f, "hosted-paste")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("exchange did not reach HTTPS")
	}
	target := oauthJournalValidationTarget(t, f.service, f.ref.Target.Owner)
	request := OAuthLoginAbandonRequest{Target: target, OriginalWorkspaceID: f.ref.Target.WorkspaceID, OriginalOperationID: f.ref.OperationID}
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	outcome, err := f.service.AbandonOAuthLoginResult(ctx, request)
	cancel()
	require.Error(t, err)
	require.False(t, outcome.Abandoned)
	close(release)
	awaitOAuthServicePhase(t, f, OAuthLoginAuthorized)
	request.Target = oauthJournalValidationTarget(t, f.service, f.ref.Target.Owner)
	outcome, err = f.service.AbandonOAuthLoginResult(t.Context(), request)
	require.Error(t, err)
	require.False(t, outcome.Abandoned)
	list, err := f.service.ListOAuthLoginResults(t.Context(), request.Target)
	require.NoError(t, err)
	require.Len(t, list.Results, 1)
	require.Equal(t, "token-result-recorded", list.Results[0].State)
	require.False(t, list.Results[0].Abandoned)
	require.EqualValues(t, 1, calls.Load())
	// No account/config save was needed to establish the durable known result.
	data, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.NotContains(t, string(data), "held-access")
}
