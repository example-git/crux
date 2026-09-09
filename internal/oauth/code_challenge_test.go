package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCallbackRequirementAndPort(t *testing.T) {
	for _, test := range []struct {
		requirement   CallbackRequirement
		port, invalid uint16
	}{
		{CallbackRequirement{Mode: "hosted-paste"}, 0, 1},
		{CallbackRequirement{Mode: "loopback-fixed", Port: 1455, Path: "/auth/callback"}, 1455, 1456},
		{CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback%2Fpart"}, 4231, 0},
	} {
		require.NoError(t, test.requirement.ValidatePort(test.port))
		require.Error(t, test.requirement.ValidatePort(test.invalid))
	}
	for _, path := range []string{"", "callback", "//host/callback", "/callback?", "/callback?x=1", "/callback#", "/callback#x", "/bad%", "/space here", "/" + strings.Repeat("a", 256)} {
		require.Error(t, (CallbackRequirement{Mode: "loopback-dynamic", Path: path}).Validate(), path)
	}
	for _, r := range []CallbackRequirement{{Mode: "other"}, {Mode: "hosted-paste", Path: "/callback"}, {Mode: "hosted-paste", Port: 1}, {Mode: "loopback-fixed", Path: "/callback"}, {Mode: "loopback-dynamic", Port: 1, Path: "/callback"}} {
		require.Error(t, r.Validate())
	}
}

func TestCodeChallengeOneHTTPSExchangeAcrossCopiesAndReplay(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"synthetic-access","refresh_token":"synthetic-refresh","client":{"client_id":"captured-client","client_secret":"synthetic-secret"}}`)
	}))
	defer func() { once.Do(func() { close(release) }); server.Close() }()
	ctx := ContextWithEnvironment(t.Context(), []string{"CHALLENGE_CLIENT=captured-client"})
	challenge, err := NewCodeChallenge(ctx, "https://example.invalid/authorize?state=private-state", time.Now().Add(time.Minute), func(ctx context.Context, input string) (*Token, error) {
		value, ok := LookupEnvironment(ctx, "CHALLENGE_CLIENT")
		if !ok || value != "captured-client" {
			return nil, errors.New("lost captured context")
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(input))
		if err != nil {
			return nil, err
		}
		response, err := server.Client().Do(request)
		if err != nil {
			return nil, err
		}
		defer response.Body.Close()
		var token Token
		err = json.NewDecoder(response.Body).Decode(&token)
		return &token, err
	})
	require.NoError(t, err)
	defer challenge.Close()
	for _, value := range []any{challenge, *challenge, struct{ Challenge *CodeChallenge }{challenge}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			require.NotContains(t, fmt.Sprintf(format, value), "private-state")
		}
		_, err := json.Marshal(value)
		require.Error(t, err)
	}
	type result struct {
		token *Token
		err   error
	}
	first, replay := make(chan result, 1), make(chan result, 1)
	go func() {
		token, err := challenge.Exchange(ContextWithEnvironment(t.Context(), []string{"CHALLENGE_CLIENT=replacement"}), "private-code")
		first <- result{token, err}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("exchange did not reach HTTPS server")
	}
	copy := *challenge
	go func() { token, err := copy.Exchange(t.Context(), "private-code"); replay <- result{token, err} }()
	_, err = challenge.Exchange(t.Context(), "conflicting-private-code")
	require.ErrorContains(t, err, "different input")
	require.NotContains(t, err.Error(), "private-code")
	waiter, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = challenge.Exchange(waiter, "private-code")
	require.ErrorIs(t, err, context.Canceled)
	once.Do(func() { close(release) })
	a, b := <-first, <-replay
	require.NoError(t, a.err)
	require.NoError(t, b.err)
	require.Equal(t, a.token, b.token)
	a.token.AccessToken = "mutated"
	a.token.Client.ClientSecret = "mutated"
	b.token.Client.ClientID = "mutated"
	challenge.Close()
	retained, err := copy.Exchange(t.Context(), "private-code")
	require.NoError(t, err)
	require.Equal(t, "synthetic-access", retained.AccessToken)
	require.Equal(t, "synthetic-secret", retained.Client.ClientSecret)
	require.Equal(t, "captured-client", retained.Client.ClientID)
	require.EqualValues(t, 1, calls.Load())
}

func TestCodeChallengeCancellationReachesHTTPSAndRetainsFailure(t *testing.T) {
	for _, mode := range []string{"parent", "submission", "close", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				calls.Add(1)
				close(entered)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			defer func() { close(release); server.Close() }()
			parent, parentCancel := context.WithCancel(t.Context())
			defer parentCancel()
			submit, submitCancel := context.WithCancel(t.Context())
			defer submitCancel()
			expires := time.Now().Add(time.Minute)
			if mode == "deadline" {
				expires = time.Now().Add(time.Second)
			}
			challenge, err := NewCodeChallenge(parent, "https://example.invalid/authorize", expires, func(ctx context.Context, input string) (*Token, error) {
				request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, strings.NewReader(input))
				if err != nil {
					return nil, err
				}
				response, err := server.Client().Do(request)
				if response != nil {
					response.Body.Close()
				}
				return nil, err
			})
			require.NoError(t, err)
			defer challenge.Close()
			done := make(chan error, 1)
			go func() { _, err := challenge.Exchange(submit, "same-code"); done <- err }()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("exchange did not start")
			}
			switch mode {
			case "parent":
				parentCancel()
			case "submission":
				submitCancel()
			case "close":
				challenge.Close()
			}
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled challenge left HTTPS request active")
			}
			err = <-done
			require.Error(t, err)
			token, replayErr := challenge.Exchange(t.Context(), "same-code")
			require.Nil(t, token)
			require.Equal(t, err, replayErr)
			require.EqualValues(t, 1, calls.Load())
		})
	}
}

func TestCodeChallengeClosedBeforeExchangeAndOversizedInput(t *testing.T) {
	for _, mode := range []string{"closed", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			var calls int
			challenge, err := NewCodeChallenge(t.Context(), "https://example.invalid/authorize", time.Time{}, func(context.Context, string) (*Token, error) { calls++; return &Token{}, nil })
			require.NoError(t, err)
			defer challenge.Close()
			input := "code"
			if mode == "closed" {
				challenge.Close()
			} else {
				input = strings.Repeat("x", (64<<10)+1)
			}
			_, err = challenge.Exchange(t.Context(), input)
			require.Error(t, err)
			_, replayErr := challenge.Exchange(t.Context(), input)
			require.Equal(t, err, replayErr)
			require.Zero(t, calls)
		})
	}
}
