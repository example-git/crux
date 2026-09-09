package manifestflow

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestDeviceCodeDeadlineCancelsActualPollingRequest(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/device" {
			_, _ = io.WriteString(w, `{"device_code":"synthetic-device","user_code":"ABCD","verification_uri":"https://example.invalid/device","expires_in":2,"interval":1}`)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		close(entered)
		select {
		case <-r.Context().Done():
			close(canceled)
		case <-release:
		}
	}))
	defer func() { close(release); server.Close() }()
	_, value := examplePluginFlow(t, server)
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	value.Capabilities.Endpoints = append(value.Capabilities.Endpoints, manifest.Endpoint{ID: "device", BaseURL: server.URL + "/device", AllowedSchemes: []string{"https"}, AllowedHosts: []string{target.Hostname()}})
	flow := &value.Capabilities.OAuth[0]
	flow.Redirect = manifest.OAuthRedirect{Mode: "device-code"}
	flow.PKCE = "disabled"
	flow.DeviceCode = &manifest.DeviceCodeFlow{Endpoint: "device", Request: []manifest.FieldRule{{Name: "client_id", Value: manifest.Template{Kind: "context", Ref: "oauth.client_id"}}}, DeviceCodePointer: "/device_code", UserCodePointer: "/user_code", VerificationURLPointer: "/verification_uri", ExpiresInPointer: "/expires_in", IntervalPointer: "/interval", DefaultIntervalSeconds: 1, Poll: []manifest.FieldRule{{Name: "device_code", Value: manifest.Template{Kind: "context", Ref: "oauth.device_code"}}}, ErrorPointer: "/error", MaxBodyBytes: 1024}
	executor, err := New(value, *flow)
	require.NoError(t, err)
	executor.client = server.Client()
	authorization, err := executor.RequestDeviceCode(t.Context())
	require.NoError(t, err)
	require.False(t, authorization.ExpiresAt().IsZero())
	copy := *authorization
	require.Equal(t, authorization.ExpiresAt(), copy.ExpiresAt())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := executor.PollDeviceCode(ctx, &copy); done <- err }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("device token request did not start")
	}
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("expired device request remains active")
	}
	require.ErrorIs(t, <-done, context.DeadlineExceeded)
	require.NoError(t, ctx.Err(), "poll did not honor captured device expiry")
}
