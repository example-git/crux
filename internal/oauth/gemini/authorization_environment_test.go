package gemini

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeRetainsClientCredentials(t *testing.T) {
	for _, captured := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy-start", true: "captured-environment"}[captured], func(t *testing.T) {
			t.Setenv("GEMINI_OAUTH_CLIENT_ID", "synthetic-original-id")
			t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "synthetic-original-secret")
			ctx := t.Context()
			if captured {
				ctx = oauth.ContextWithEnvironment(ctx, []string{"GEMINI_OAUTH_CLIENT_ID=synthetic-captured-id", "GEMINI_OAUTH_CLIENT_SECRET=synthetic-captured-secret"})
			}
			expectedID, expectedSecret := "synthetic-original-id", "synthetic-original-secret"
			if captured {
				expectedID, expectedSecret = "synthetic-captured-id", "synthetic-captured-secret"
			}
			var form url.Values
			host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.NoError(t, r.ParseForm())
				form = r.Form
				_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":120}`)
			}))
			defer host.Close()
			original := http.DefaultClient
			target, err := url.Parse(host.URL)
			require.NoError(t, err)
			http.DefaultClient = &http.Client{Transport: geminiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				require.Equal(t, tokenURL, r.URL.String())
				copy := r.Clone(r.Context())
				address := *r.URL
				address.Scheme, address.Host = target.Scheme, target.Host
				copy.URL = &address
				return host.Client().Transport.RoundTrip(copy)
			})}
			defer func() { http.DefaultClient = original }()
			token, err := Authorize(ctx, func(raw string) error {
				u, e := url.Parse(raw)
				require.NoError(t, e)
				require.Equal(t, expectedID, u.Query().Get("client_id"))
				t.Setenv("GEMINI_OAUTH_CLIENT_ID", "changed-id")
				t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "changed-secret")
				return nil
			}, func() (string, error) { return "synthetic-code", nil })
			require.NoError(t, err)
			require.Equal(t, expectedID, form.Get("client_id"))
			require.Equal(t, expectedSecret, form.Get("client_secret"))
			require.Equal(t, expectedID, token.Client.ClientID)
		})
	}
}

func TestBoundClientCredentialsDoNotUseAmbientValues(t *testing.T) {
	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "ambient-id")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "ambient-secret")
	_, err := Authorize(oauth.ContextWithEnvironment(t.Context(), nil), func(string) error { t.Fatal("missing captured credentials opened browser"); return nil }, func() (string, error) { t.Fatal("missing captured credentials requested code"); return "", nil })
	require.ErrorContains(t, err, "not configured")
}

func TestAuthorizeCancellationReleasesPastedCodeWait(t *testing.T) {
	ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), []string{"GEMINI_OAUTH_CLIENT_ID=synthetic-id", "GEMINI_OAUTH_CLIENT_SECRET=synthetic-secret"}))
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	done := make(chan error, 1)
	go func() {
		_, err := Authorize(ctx, func(string) error { return nil }, func() (string, error) { close(entered); <-release; return "late-code", nil })
		done <- err
	}()
	<-entered
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("canceled authorization remains blocked on pasted code")
	}
}
