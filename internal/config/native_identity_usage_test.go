package config

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientNativeUsageKeepsAcceptedIdentityAcrossReplacement(t *testing.T) {
	headers := make(chan http.Header, 8)
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	nativeIdentityTLS(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/backend-api/wham/usage", r.URL.Path)
		headers <- r.Header.Clone()
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		_, _ = io.WriteString(w, `{"plan_type":"fixture","rate_limit":{"primary_window":{"used_percent":17,"limit_window_seconds":18000}}}`)
	})
	store, proposal := clientRefreshRuntimeFixture(t)
	firstIdentity := *proposal.Providers[0].NativeIdentity
	oldRequest := ProviderUsageRequest{Owner: proposal.Credentials[0].Owner, Revision: 1, Digest: proposal.Digest}
	t.Setenv("CODEX_VERSION", "9.9.9")
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "hostile-usage")
	t.Setenv("TERM_PROGRAM", "HostileTerminal")
	type outcome struct {
		result *ProviderUsageResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() { result, err := store.ProviderUsage(t.Context(), oldRequest); done <- outcome{result, err} }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("usage request did not start")
	}
	nextIdentity := NativeIdentity{UserAgent: "next-client/2.3.4 (NextOS 1; fixture) NextTerminal", Version: "2.3.4", Originator: "next-client"}
	proposal.Providers[0].NativeIdentity = &nextIdentity
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	proposal = sealRemoteRuntime(t, proposal)
	_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, strings.Repeat("a", 64), 1)
	close(release)
	require.NoError(t, err)
	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, uint64(1), result.result.Revision)
	require.Equal(t, oldRequest.Digest, result.result.Digest)
	require.Equal(t, 17, result.result.Usage.Windows[0].Percent)
	require.Equal(t, firstIdentity.UserAgent, (<-headers).Get("User-Agent"))
	current := ProviderUsageRequest{Owner: oldRequest.Owner, Revision: 2, Digest: proposal.Digest}
	usage, err := store.ProviderUsage(t.Context(), current)
	require.NoError(t, err)
	require.Equal(t, uint64(2), usage.Revision)
	require.Equal(t, nextIdentity.UserAgent, (<-headers).Get("User-Agent"))
	_, err = store.ProviderUsage(t.Context(), oldRequest)
	require.ErrorIs(t, err, ErrProviderUsageAuthority)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = store.ProviderUsage(ctx, current)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, int32(2), calls.Load(), "stale and canceled usage must not send HTTP")
}

func TestClientNativeUsageCancellationReachesHTTPS(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	nativeIdentityTLS(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		close(canceled)
	})
	store, proposal := clientRefreshRuntimeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.ProviderUsage(ctx, ProviderUsageRequest{Owner: proposal.Credentials[0].Owner, Revision: 1, Digest: proposal.Digest})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("usage did not start")
	}
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not reach request")
	}
}
