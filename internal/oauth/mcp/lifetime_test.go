package mcpoauth

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestHandlerSessionLifetimeCancelsOnlyItsRefresh(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		once.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer blocked.Close()
	defer close(release)
	var saved atomic.Int32
	token := func(endpoint string) *oauth.Token {
		return &oauth.Token{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "fixture", TokenURL: endpoint}}
	}
	first, err := NewHandlerWithContext(ctx, "same-name", blocked.URL, token(blocked.URL), nil, func(*oauth.Token) { saved.Add(1) }, false, 0)
	require.NoError(t, err)
	defer first.Close()
	source, err := first.TokenSource(ctx)
	require.NoError(t, err)
	result := make(chan error, 1)
	go func() { _, err := source.Token(); result <- err }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	first.Close()
	first.Close()
	select {
	case err := <-result:
		require.Error(t, err)
	case <-ctx.Done():
		t.Fatal("refresh survived handler shutdown")
	}
	require.Zero(t, saved.Load())
	_, err = first.TokenSource(ctx)
	require.ErrorIs(t, err, context.Canceled)
	host, serverURL := newFakeAS(t, fakeASOpts{clientID: "fixture", refreshedToken: "second-new", refreshToken: "second-refresh"})
	second, err := NewHandlerWithContext(ctx, "same-name", serverURL, token(host+"/token"), nil, func(*oauth.Token) { saved.Add(1) }, false, 0)
	require.NoError(t, err)
	defer second.Close()
	source, err = second.TokenSource(ctx)
	require.NoError(t, err)
	refreshed, err := source.Token()
	require.NoError(t, err)
	require.Equal(t, "second-new", refreshed.AccessToken)
	require.EqualValues(t, 1, saved.Load())
}

func TestSessionHTTPClientRejectsDetachedRequestsAfterClose(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(http.StatusNoContent) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	client := NewSessionHTTPClient(ctx)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	response, err := client.Do(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	cancel()
	request, err = http.NewRequestWithContext(context.Background(), http.MethodPost, server.URL, nil)
	require.NoError(t, err)
	response, err = client.Do(request)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorIs(t, err, context.Canceled)
	require.EqualValues(t, 1, requests.Load())
}

type sessionTestTransport func(*http.Request) (*http.Response, error)

func (f sessionTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSessionHTTPClientCustomTransportResponses(t *testing.T) {
	for _, test := range []struct {
		name      string
		response  *http.Response
		wantError bool
	}{
		{"nil response", nil, true},
		{"missing nonempty body", &http.Response{StatusCode: 200, ContentLength: 3}, true},
		{"empty body", &http.Response{StatusCode: 204}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := &http.Client{Transport: &lifetimeTransport{ctx: ctx, base: sessionTestTransport(func(*http.Request) (*http.Response, error) { return test.response, nil })}}
			request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://fixture.invalid", nil)
			require.NoError(t, err)
			response, err := client.Do(request)
			if test.wantError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, response.Body)
			require.NoError(t, response.Body.Close())
		})
	}
}

func TestHandlerSessionCloseWaitsForAdmittedTokenSave(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	host, serverURL := newFakeAS(t, fakeASOpts{clientID: "fixture", refreshedToken: "new", refreshToken: "refresh"})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var saved atomic.Int32
	handler, err := NewHandlerWithContext(ctx, "fixture", serverURL, &oauth.Token{AccessToken: "expired", RefreshToken: "refresh", ExpiresAt: time.Now().Add(-time.Hour).Unix(), Client: &oauth.OAuthClient{ClientID: "fixture", TokenURL: host + "/token"}}, nil, func(*oauth.Token) {
		close(entered)
		<-release
		saved.Add(1)
	}, false, 0)
	require.NoError(t, err)
	source, err := handler.TokenSource(ctx)
	require.NoError(t, err)
	refreshed := make(chan error, 1)
	go func() { _, err := source.Token(); refreshed <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	closed := make(chan struct{})
	go func() { handler.Close(); close(closed) }()
	select {
	case <-handler.lifetime.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-closed:
		t.Fatal("Close returned while its token save was still pending")
	case <-time.After(50 * time.Millisecond):
	}
	releaseOnce.Do(func() { close(release) })
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-refreshed:
		require.NoError(t, err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	require.EqualValues(t, 1, saved.Load())
	handler.Close()
	_, err = handler.TokenSource(ctx)
	require.ErrorIs(t, err, context.Canceled)
}
