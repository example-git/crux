package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

// Real exchange requests distinguish the chosen function from merely adopting
// another function's saved token, while the account lease prevents token reuse.
func TestExplicitRefresherUsesProvenSuccessorThroughHTTPS(t *testing.T) {
	for _, api := range []string{"entry", "active"} {
		t.Run(api, func(t *testing.T) {
			entry := rotationFixture(t)
			started, release := make(chan struct{}), make(chan struct{})
			var released atomic.Bool
			releasePeer := func() {
				if released.CompareAndSwap(false, true) {
					close(release)
				}
			}
			defer releasePeer()
			inputs := make(chan string, 2)
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Refresh string `json:"refresh"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, "invalid fixture", 400)
					return
				}
				inputs <- r.URL.Path + ":" + body.Refresh
				token := &oauth.Token{AccessToken: "peer-access", RefreshToken: "peer-refresh", ExpiresAt: time.Now().Add(time.Hour).Unix()}
				if r.URL.Path == "/peer" {
					close(started)
					<-release
				} else {
					token.AccessToken = "explicit-access"
					token.RefreshToken = "explicit-refresh"
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(token)
			}))
			defer func() { releasePeer(); host.Close() }()
			exchange := func(path string) Refresher {
				return func(ctx context.Context, refresh string) (*oauth.Token, error) {
					payload, _ := json.Marshal(map[string]string{"refresh": refresh})
					request, err := http.NewRequestWithContext(ctx, http.MethodPost, host.URL+path, strings.NewReader(string(payload)))
					if err != nil {
						return nil, err
					}
					response, err := host.Client().Do(request)
					if err != nil {
						return nil, err
					}
					defer response.Body.Close()
					var token oauth.Token
					err = json.NewDecoder(response.Body).Decode(&token)
					return &token, err
				}
			}
			RegisterRefresher("rotation", exchange("/peer"))
			t.Cleanup(func() { RegisterRefresher("rotation", nil) })
			peerDone := make(chan error, 1)
			go func() { _, err := AccessToken(t.Context(), "rotation"); peerDone <- err }()
			<-started
			result := make(chan struct {
				token string
				err   error
			}, 1)
			go func() {
				var token string
				var err error
				if api == "active" {
					token, err = AccessTokenWithRefresher(t.Context(), "rotation", exchange("/explicit"))
				} else {
					var got *Entry
					got, err = EnsureFreshWithRefresher(t.Context(), "rotation", &entry, exchange("/explicit"))
					if got != nil {
						token = got.AccessToken
					}
				}
				result <- struct {
					token string
					err   error
				}{token, err}
			}()
			select {
			case got := <-result:
				t.Fatalf("explicit exchange bypassed held account lease: %v", got.err)
			case <-time.After(100 * time.Millisecond):
			}
			require.Equal(t, "/peer:synthetic-old-refresh", <-inputs)
			select {
			case got := <-inputs:
				t.Fatalf("parallel refresh reused predecessor: %s", got)
			default:
			}
			releasePeer()
			require.NoError(t, <-peerDone)
			got := <-result
			require.NoError(t, got.err)
			require.Equal(t, "explicit-access", got.token)
			require.Equal(t, "/explicit:peer-refresh", <-inputs)
			saved, err := Active(t.Context(), "rotation")
			require.NoError(t, err)
			require.Equal(t, "explicit-access", saved.AccessToken)
			require.Equal(t, "explicit-refresh", saved.RefreshToken)
			// Owner-qualified callers keep the existing complete-rotation adoption
			// contract, now including both links in the serialized chain.
			adopted, err := EnsureFreshForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
				t.Error("owner consumer unnecessarily exchanged")
				return nil, errors.New("unexpected exchange")
			}, func() error { return nil })
			require.NoError(t, err)
			require.Equal(t, saved, adopted)
		})
	}
}

func TestExplicitRefresherRejectsUnprovenReplacement(t *testing.T) {
	for _, change := range []string{"manual", "logout", "identical-successor"} {
		t.Run(change, func(t *testing.T) {
			entry := rotationFixture(t)
			require.NoError(t, rotatePeerForExplicitTest(t, &entry))
			successor, err := Active(t.Context(), "rotation")
			require.NoError(t, err)
			switch change {
			case "manual":
				require.NoError(t, Save(t.Context(), "rotation", Entry{ID: entry.ID, AccessToken: "manual-access", RefreshToken: "manual-refresh"}))
			case "logout":
				require.NoError(t, RemoveProvider(t.Context(), "rotation"))
			case "identical-successor":
				require.NoError(t, Save(t.Context(), "rotation", *successor))
			}
			var called atomic.Bool
			_, err = EnsureFreshWithRefresher(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) { called.Store(true); return rotatedToken(), nil })
			require.ErrorIs(t, err, ErrCredentialChanged)
			require.False(t, called.Load())
		})
	}
}

func rotatePeerForExplicitTest(t *testing.T, entry *Entry) error {
	t.Helper()
	_, err := RefreshSelectedForOwner(t.Context(), "rotation", entry, func(context.Context, string) (*oauth.Token, error) { return rotatedToken(), nil }, func() error { return nil }, true)
	return err
}

func TestExplicitRefresherNoRefreshNeededKeepsLegacyBranches(t *testing.T) {
	for _, entry := range []Entry{{ID: "fresh", AccessToken: "fresh", RefreshToken: "refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, {ID: "no-refresh", AccessToken: "expired", ExpiresAt: time.Now().Add(-time.Hour).UnixMilli()}} {
		got, err := EnsureFreshWithRefresher(t.Context(), "unused", &entry, nil)
		require.NoError(t, err)
		require.Equal(t, entry, *got)
	}
}

func TestExplicitRefresherSuccessorStillRequiresUnchangedCommitTarget(t *testing.T) {
	for _, change := range []string{"manual", "logout", "identical-successor"} {
		t.Run(change, func(t *testing.T) {
			entry := rotationFixture(t)
			require.NoError(t, rotatePeerForExplicitTest(t, &entry))
			successor, err := Active(t.Context(), "rotation")
			require.NoError(t, err)
			var called bool
			_, err = EnsureFreshWithRefresher(t.Context(), "rotation", &entry, func(_ context.Context, refresh string) (*oauth.Token, error) {
				called = true
				require.Equal(t, successor.RefreshToken, refresh)
				switch change {
				case "manual":
					require.NoError(t, Save(t.Context(), "rotation", Entry{ID: entry.ID, AccessToken: "manual"}))
				case "logout":
					require.NoError(t, RemoveProvider(t.Context(), "rotation"))
				case "identical-successor":
					require.NoError(t, Save(t.Context(), "rotation", *successor))
				}
				return &oauth.Token{AccessToken: "must-not-publish", RefreshToken: "consumed-successor", ExpiresAt: time.Now().Add(time.Hour).Unix()}, nil
			})
			require.True(t, called)
			require.ErrorIs(t, err, ErrCredentialChanged)
			stored, err := Active(t.Context(), "rotation")
			require.NoError(t, err)
			switch change {
			case "logout":
				require.Nil(t, stored)
			case "manual":
				require.Equal(t, "manual", stored.AccessToken)
			default:
				require.Equal(t, successor, stored)
			}
		})
	}
}

func TestExplicitRefresherCancellationWhileWaitingNeverExchanges(t *testing.T) {
	entry := rotationFixture(t)
	started, release := make(chan struct{}), make(chan struct{})
	var released atomic.Bool
	releasePeer := func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}
	defer releasePeer()
	peerDone := make(chan error, 1)
	go func() {
		_, err := RefreshSelectedForOwner(t.Context(), "rotation", &entry, func(context.Context, string) (*oauth.Token, error) {
			close(started)
			<-release
			return rotatedToken(), nil
		}, func() error { return nil }, true)
		peerDone <- err
	}()
	<-started
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var called atomic.Bool
	done := make(chan error, 1)
	go func() {
		_, err := EnsureFreshWithRefresher(ctx, "rotation", &entry, func(context.Context, string) (*oauth.Token, error) { called.Store(true); return rotatedToken(), nil })
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("did not wait for lease: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	require.False(t, called.Load())
	releasePeer()
	require.NoError(t, <-peerDone)
}
