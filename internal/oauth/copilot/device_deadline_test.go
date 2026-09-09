package copilot

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRequestedDeviceCodeRetainsOriginalDeadline(t *testing.T) {
	t.Setenv("COPILOT_ADVERTISE_MODE", "vscode")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "0.45.2026041705")
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "/login/device/code", r.URL.Path)
		_, _ = io.WriteString(w, `{"device_code":"synthetic-device","user_code":"ABCD","verification_uri":"https://example.invalid/device","expires_in":1,"interval":30}`)
	}))
	defer server.Close()
	original := http.DefaultTransport
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	http.DefaultTransport = copilotRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, deviceCodeURL, r.URL.String())
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})
	defer func() { http.DefaultTransport = original }()
	before := time.Now()
	authorization, err := RequestDeviceCode(t.Context())
	require.NoError(t, err)
	require.WithinDuration(t, before.Add(time.Second), authorization.ExpiresAt(), time.Second)
	captured := authorization.ExpiresAt()
	copy := *authorization
	copy.ExpiresIn = 3600
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = PollForToken(ctx, &copy)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, captured, copy.ExpiresAt())
	require.NoError(t, ctx.Err(), "polling restarted its relative expiry")
	require.EqualValues(t, 1, calls.Load())
}
