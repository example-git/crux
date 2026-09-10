package callbackrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func callbackRequest(t *testing.T, relay *Relay, method, path string) (int, string) {
	t.Helper()
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), method, fmt.Sprintf("http://localhost:%d%s", relay.Port(), path), nil)
	require.NoError(t, err)
	response, err := client.Do(req)
	require.NoError(t, err)
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return response.StatusCode, string(data)
}

func TestCallbackRelayExactEscapedPathOneQueryAndPrivateCopies(t *testing.T) {
	r, err := Start(t.Context(), oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/oauth%2Fcallback"})
	require.NoError(t, err)
	defer r.Close()
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{"GET", "/oauth/callback?code=wrong", 404},
		{"POST", "/oauth%2Fcallback?code=wrong", 405},
		{"GET", "/oauth%2Fcallback", 400},
		{"GET", "/oauth%2Fcallback?code=" + strings.Repeat("a", InputLimit), 413},
	} {
		status, _ := callbackRequest(t, r, request.method, request.path)
		require.Equal(t, request.status, status)
	}
	input := "code=private-code&state=private-state"
	status, body := callbackRequest(t, r, "GET", "/oauth%2Fcallback?"+input)
	require.Equal(t, 200, status)
	require.Contains(t, body, "received")
	require.NotContains(t, body, "saved")
	require.NotContains(t, body, "private-code")
	received, err := r.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, input, received)
	status, _ = callbackRequest(t, r, "GET", "/oauth%2Fcallback?code=another&state=another")
	require.Equal(t, 409, status)
	received, err = r.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, input, received)
	for _, value := range []any{r, *r} {
		for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
			require.NotContains(t, fmt.Sprintf(format, value), "private-code")
		}
		_, err = json.Marshal(value)
		require.Error(t, err)
	}
	require.NoError(t, r.Close())
	received, err = r.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, input, received)
}

func TestCallbackRelayFixedPortConflictDoesNotChooseAnotherPort(t *testing.T) {
	occupied, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "localhost:0")
	require.NoError(t, err)
	port := uint16(occupied.Addr().(*net.TCPAddr).Port)
	descriptor := oauth.CallbackRequirement{Mode: "loopback-fixed", Port: port, Path: "/callback"}
	r, err := Start(t.Context(), descriptor)
	require.Error(t, err)
	require.Nil(t, r)
	require.NoError(t, occupied.Close())
	r, err = Start(t.Context(), descriptor)
	require.NoError(t, err)
	defer r.Close()
	require.Equal(t, port, r.Port())
	status, _ := callbackRequest(t, r, "GET", "/callback?code=fixed&state=fixed")
	require.Equal(t, 200, status)
}

func TestCallbackRelayWaitCancellationAndWorkspaceCloseAreIndependent(t *testing.T) {
	lifetime, cancelLifetime := context.WithCancel(t.Context())
	first, err := Start(lifetime, oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"})
	require.NoError(t, err)
	defer first.Close()
	second, err := Start(t.Context(), oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"})
	require.NoError(t, err)
	defer second.Close()
	waitCtx, cancelWait := context.WithCancel(t.Context())
	cancelWait()
	_, err = first.Wait(waitCtx)
	require.ErrorIs(t, err, context.Canceled)
	status, _ := callbackRequest(t, first, "GET", "/callback?code=kept&state=kept")
	require.Equal(t, 200, status)
	input, err := first.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "code=kept&state=kept", input)
	cancelLifetime()
	require.NoError(t, first.Close())
	status, _ = callbackRequest(t, second, "GET", "/callback?code=other&state=other")
	require.Equal(t, 200, status)
	input, err = second.Wait(t.Context())
	require.NoError(t, err)
	require.Equal(t, "code=other&state=other", input)
	pending, err := Start(t.Context(), oauth.CallbackRequirement{Mode: "loopback-dynamic", Path: "/callback"})
	require.NoError(t, err)
	finished := make(chan error, 1)
	go func() { _, err := pending.Wait(t.Context()); finished <- err }()
	require.NoError(t, pending.Close())
	select {
	case err := <-finished:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("closed relay did not wake its waiter")
	}
}

func TestCallbackRelayRejectsNonLoopbackOrInvalidRequirement(t *testing.T) {
	for _, descriptor := range []oauth.CallbackRequirement{
		{Mode: "hosted-paste"},
		{Mode: "loopback-fixed", Path: "/callback"},
		{Mode: "loopback-dynamic", Port: 1234, Path: "/callback"},
		{Mode: "loopback-dynamic", Path: "//remote.example/callback"},
		{Mode: "loopback-dynamic", Path: "/callback?query=bad"},
	} {
		r, err := Start(t.Context(), descriptor)
		require.Error(t, err)
		require.Nil(t, r)
	}
}
