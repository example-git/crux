package gemini

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestPrepareCodeHostedCapturedCredentialsAndValidation(t *testing.T) {
	var calls atomic.Int32
	var form url.Values
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.NoError(t, r.ParseForm())
		form = r.Form
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","expires_in":120}`)
	}))
	defer server.Close()
	original := http.DefaultClient
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	http.DefaultClient = &http.Client{Transport: geminiRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, testClient().Token.BaseURL, r.URL.String())
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})}
	defer func() { http.DefaultClient = original }()
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"GEMINI_OAUTH_CLIENT_ID=captured-id", "GEMINI_OAUTH_CLIENT_SECRET=captured-secret"})
	_, err = testClient().PrepareCode(ctx, 1)
	require.Error(t, err)
	for _, mode := range []string{"bare", "callback", "state", "duplicate", "malformed", "owner"} {
		t.Run(mode, func(t *testing.T) {
			before := calls.Load()
			var replaced atomic.Bool
			owner := providertransport.ContextWithOwnerValidator(ctx, func() error {
				if replaced.Load() {
					return errors.New("owner replaced")
				}
				return nil
			})
			challenge, err := testClient().PrepareCode(owner, 0)
			require.NoError(t, err)
			defer challenge.Close()
			require.Equal(t, before, calls.Load())
			u, err := url.Parse(challenge.AuthorizationURL())
			require.NoError(t, err)
			require.Equal(t, testClient().RedirectURI, u.Query().Get("redirect_uri"))
			t.Setenv("GEMINI_OAUTH_CLIENT_ID", "ambient-changed")
			t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "ambient-secret")
			q := url.Values{"code": {"synthetic-code"}, "state": {u.Query().Get("state")}}
			input := q.Encode()
			switch mode {
			case "bare":
				input = "synthetic-code"
			case "callback":
				input = testClient().RedirectURI + "?" + input
			case "state":
				q.Set("state", "wrong")
				input = q.Encode()
			case "duplicate":
				q.Add("state", "wrong")
				input = q.Encode()
			case "malformed":
				input = "code=%zz&state=" + q.Get("state")
			case "owner":
				replaced.Store(true)
			}
			token, err := challenge.Exchange(t.Context(), input)
			if mode == "bare" || mode == "callback" {
				require.NoError(t, err)
				require.Equal(t, "captured-id", form.Get("client_id"))
				require.Equal(t, "captured-secret", form.Get("client_secret"))
				require.Equal(t, testClient().RedirectURI, form.Get("redirect_uri"))
				require.Equal(t, "synthetic-access", token.AccessToken)
				require.Equal(t, before+1, calls.Load())
			} else {
				require.Error(t, err)
				require.Nil(t, token)
				require.Equal(t, before, calls.Load())
			}
			_, again := challenge.Exchange(t.Context(), input)
			require.Equal(t, err, again)
		})
	}
}
