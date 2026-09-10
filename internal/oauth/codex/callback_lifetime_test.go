package codex

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeCallbackRetainsOwnerAndClientID(t *testing.T) {
	for _, replaced := range []bool{false, true} {
		t.Run(map[bool]string{false: "captured client", true: "owner replaced"}[replaced], func(t *testing.T) {
			t.Setenv("CODEX_OAUTH_CLIENT_ID", "synthetic-original")
			var calls atomic.Int32
			var clientID string
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.NoError(t, r.ParseForm())
				clientID = r.Form.Get("client_id")
				_, _ = io.WriteString(w, `{"access_token":"synthetic-access"}`)
			}))
			defer host.Close()
			original := http.DefaultClient
			target, err := url.Parse(host.URL)
			require.NoError(t, err)
			http.DefaultClient = &http.Client{Transport: codexRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, testClient().Token.BaseURL, r.URL.String())
				copy := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = target.Scheme, target.Host
				copy.URL = &address
				return host.Client().Transport.RoundTrip(copy)
			})}
			defer func() { http.DefaultClient = original }()
			var changed atomic.Bool
			ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
				if changed.Load() {
					return errors.New("captured owner changed")
				}
				return nil
			})
			callbacks := make(chan error, 1)
			opened := false
			token, err := testClient().Authorize(ctx, func(raw string) error {
				opened = true
				authorization, parseErr := url.Parse(raw)
				if parseErr != nil {
					return parseErr
				}
				require.Equal(t, "synthetic-original", authorization.Query().Get("client_id"))
				t.Setenv("CODEX_OAUTH_CLIENT_ID", "synthetic-replacement")
				changed.Store(replaced)
				callback, parseErr := url.Parse(authorization.Query().Get("redirect_uri"))
				if parseErr != nil {
					return parseErr
				}
				q := callback.Query()
				q.Set("state", authorization.Query().Get("state"))
				q.Set("code", "synthetic-code")
				callback.RawQuery = q.Encode()
				go func() {
					request, e := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
					if e != nil {
						callbacks <- e
						return
					}
					response, e := (&http.Client{Timeout: 3 * time.Second}).Do(request)
					if response != nil {
						response.Body.Close()
					}
					callbacks <- e
				}()
				return nil
			})
			require.True(t, opened, "authorization could not open its fixed callback listener: %v", err)
			require.NoError(t, <-callbacks)
			if replaced {
				require.ErrorContains(t, err, "captured owner changed")
				require.Nil(t, token)
				require.Zero(t, calls.Load())
			} else {
				require.NoError(t, err)
				require.Equal(t, "synthetic-access", token.AccessToken)
				require.Equal(t, "synthetic-original", clientID)
				require.EqualValues(t, 1, calls.Load())
			}
		})
	}
}

func TestAuthorizeBoundEnvironmentDoesNotUseAmbient(t *testing.T) {
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "ambient-client")
	_, err := testClient().Authorize(oauth.ContextWithEnvironment(t.Context(), nil), func(string) error { t.Fatal("absent captured client ID opened authorization"); return nil })
	require.ErrorContains(t, err, "not configured")
}

func TestAuthorizeCallbackDuplicateAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		t.Run(map[bool]string{false: "duplicate", true: "cancel exchange"}[canceled], func(t *testing.T) {
			ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_OAUTH_CLIENT_ID=synthetic-client"}))
			defer cancel()
			entered, requestCanceled := make(chan struct{}, 2), make(chan struct{}, 2)
			release := make(chan struct{})
			var once sync.Once
			var calls atomic.Int32
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
					_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":120}`)
				case <-r.Context().Done():
					requestCanceled <- struct{}{}
				}
			}))
			defer func() { once.Do(func() { close(release) }); host.Close() }()
			original := http.DefaultClient
			target, err := url.Parse(host.URL)
			require.NoError(t, err)
			http.DefaultClient = &http.Client{Transport: codexRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, testClient().Token.BaseURL, r.URL.String())
				copy := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = target.Scheme, target.Host
				copy.URL = &address
				return host.Client().Transport.RoundTrip(copy)
			})}
			defer func() { http.DefaultClient = original }()
			opened := make(chan string, 1)
			done := make(chan error, 1)
			go func() {
				_, err := testClient().Authorize(ctx, func(raw string) error { opened <- raw; return nil })
				done <- err
			}()
			var raw string
			select {
			case raw = <-opened:
			case err := <-done:
				t.Fatalf("authorization could not open callback listener: %v", err)
			case <-time.After(3 * time.Second):
				t.Fatal("authorization did not start")
			}
			authorization, err := url.Parse(raw)
			require.NoError(t, err)
			callback, err := url.Parse(authorization.Query().Get("redirect_uri"))
			require.NoError(t, err)
			q := callback.Query()
			q.Set("code", "synthetic-code")
			q.Set("state", authorization.Query().Get("state"))
			callback.RawQuery = q.Encode()
			browser := &http.Client{Timeout: 3 * time.Second}
			first := make(chan error, 1)
			go func() {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
				require.NoError(t, err)
				response, err := browser.Do(request)
				if response != nil {
					response.Body.Close()
				}
				first <- err
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("token exchange did not start")
			}
			if canceled {
				cancel()
				select {
				case <-requestCanceled:
				case <-time.After(time.Second):
					t.Fatal("token request survived authorization cancellation")
				}
				require.ErrorIs(t, <-done, context.Canceled)
			} else {
				request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback.String(), nil)
				require.NoError(t, err)
				duplicate, err := (&http.Client{Timeout: time.Second}).Do(request)
				once.Do(func() { close(release) })
				require.NoError(t, err)
				require.Equal(t, http.StatusConflict, duplicate.StatusCode)
				duplicate.Body.Close()
				require.NoError(t, <-done)
			}
			require.NoError(t, <-first)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}
