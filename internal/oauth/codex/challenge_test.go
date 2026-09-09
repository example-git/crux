package codex

import (
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func TestPrepareCodeCapturedClientPKCEAndSingleExchange(t *testing.T) {
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
	http.DefaultClient = &http.Client{Transport: codexRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, tokenURL, r.URL.String())
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})}
	defer func() { http.DefaultClient = original }()
	// A bound fixed port proves preparation does not try to listen on the owner.
	listener, err := net.Listen("tcp", "localhost:1455")
	require.NoError(t, err)
	defer listener.Close()
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_OAUTH_CLIENT_ID=captured-client"})
	_, err = PrepareCode(ctx, 1456)
	require.Error(t, err)
	challenge, err := PrepareCode(ctx, 1455)
	require.NoError(t, err)
	defer challenge.Close()
	require.Zero(t, calls.Load())
	u, err := url.Parse(challenge.AuthorizationURL())
	require.NoError(t, err)
	require.Equal(t, "captured-client", u.Query().Get("client_id"))
	require.Equal(t, "http://localhost:1455/auth/callback", u.Query().Get("redirect_uri"))
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "changed-client")
	input := url.Values{"code": {"synthetic-code"}, "state": {u.Query().Get("state")}}.Encode()
	token, err := challenge.Exchange(t.Context(), input)
	require.NoError(t, err)
	require.Equal(t, "synthetic-access", token.AccessToken)
	require.Equal(t, "captured-client", form.Get("client_id"))
	require.Equal(t, u.Query().Get("redirect_uri"), form.Get("redirect_uri"))
	digest := sha256.Sum256([]byte(form.Get("code_verifier")))
	require.Equal(t, u.Query().Get("code_challenge"), base64.RawURLEncoding.EncodeToString(digest[:]))
	_, err = challenge.Exchange(t.Context(), input)
	require.NoError(t, err)
	require.EqualValues(t, 1, calls.Load())
}

func TestPrepareCodeInvalidStateAndReplacedOwnerDoNotDispatch(t *testing.T) {
	original := http.DefaultClient
	var calls atomic.Int32
	http.DefaultClient = &http.Client{Transport: codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("unexpected request")
	})}
	defer func() { http.DefaultClient = original }()
	for _, mode := range []string{"state", "duplicate", "owner"} {
		t.Run(mode, func(t *testing.T) {
			var replaced atomic.Bool
			ctx := providertransport.ContextWithOwnerValidator(oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_OAUTH_CLIENT_ID=captured-client"}), func() error {
				if replaced.Load() {
					return errors.New("owner replaced")
				}
				return nil
			})
			challenge, err := PrepareCode(ctx, 1455)
			require.NoError(t, err)
			defer challenge.Close()
			u, err := url.Parse(challenge.AuthorizationURL())
			require.NoError(t, err)
			q := url.Values{"code": {"synthetic-code"}, "state": {u.Query().Get("state")}}
			switch mode {
			case "state":
				q.Set("state", "wrong")
			case "duplicate":
				q.Add("code", "second")
			case "owner":
				replaced.Store(true)
			}
			input := q.Encode()
			_, err = challenge.Exchange(t.Context(), input)
			require.Error(t, err)
			_, again := challenge.Exchange(t.Context(), input)
			require.Equal(t, err, again)
		})
	}
	require.Zero(t, calls.Load())
}
