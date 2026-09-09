package manifestflow

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestDeclarativeCallbackExchangeIsClaimedOnce(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access"}`)
	}))
	defer server.Close()
	executor, _ := examplePluginFlow(t, server)
	opened := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := executor.Authorize(t.Context(), func(raw string) error { opened <- raw; return nil }, nil)
		done <- err
	}()
	callbackURL := callbackURLForTest(t, <-opened)
	first := make(chan error, 1)
	go func() {
		response, err := http.Get(callbackURL)
		if response != nil {
			response.Body.Close()
		}
		first <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first exchange did not start")
	}
	client := &http.Client{Timeout: time.Second}
	second, err := client.Get(callbackURL)
	if err == nil {
		require.Equal(t, http.StatusConflict, second.StatusCode)
		second.Body.Close()
	}
	releaseOnce.Do(func() { close(release) })
	require.NoError(t, err, "a duplicate callback must not wait for another token exchange")
	require.NoError(t, <-first)
	require.NoError(t, <-done)
	require.EqualValues(t, 1, calls.Load())
}

func TestDeclarativeCallbackTimeoutCancelsExchange(t *testing.T) {
	entered, canceled := make(chan struct{}, 1), make(chan struct{}, 1)
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		entered <- struct{}{}
		select {
		case <-r.Context().Done():
			canceled <- struct{}{}
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	executor, _ := examplePluginFlow(t, server)
	executor.flow.TimeoutSeconds = 1
	opened := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		_, err := executor.Authorize(t.Context(), func(raw string) error { opened <- raw; return nil }, nil)
		done <- err
	}()
	callbackURL := callbackURLForTest(t, <-opened)
	go func() {
		response, _ := http.Get(callbackURL)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not start")
	}
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(4 * time.Second):
		t.Fatal("authorization did not expire")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Error("expired authorization left the token request active")
	}
}

func callbackURLForTest(t *testing.T, raw string) string {
	t.Helper()
	authorization, err := url.Parse(raw)
	require.NoError(t, err)
	callback, err := url.Parse(authorization.Query().Get("redirect_uri"))
	require.NoError(t, err)
	query := callback.Query()
	query.Set("code", "synthetic-code")
	query.Set("state", authorization.Query().Get("state"))
	callback.RawQuery = query.Encode()
	return callback.String()
}
