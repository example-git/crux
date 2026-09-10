package useragent_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providerregistry/registrytest"
	"github.com/stretchr/testify/require"
)

type metadataRoundTrip func(*http.Request) (*http.Response, error)

func (f metadataRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func routeMetadata(t *testing.T, server *httptest.Server) {
	t.Helper()
	original := http.DefaultClient
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	http.DefaultClient = &http.Client{Transport: metadataRoundTrip(func(r *http.Request) (*http.Response, error) {
		require.True(t, strings.HasSuffix(r.URL.Path, "/identity") || strings.HasSuffix(r.URL.Path, "/project"), "unexpected non-metadata request %s", r.URL.Path)
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})}
	t.Cleanup(func() { http.DefaultClient = original })
}

func TestNativeMetadataRequestsKeepCapturedHeaders(t *testing.T) {
	requests := make(chan http.Header, 8)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r.Header.Clone()
		require.Equal(t, "Bearer synthetic-access", r.Header.Get("Authorization"))
		if strings.HasSuffix(r.URL.Path, "/identity") {
			_, _ = io.WriteString(w, `{"email":"synthetic@example.invalid"}`)
		} else {
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"synthetic-project"}`)
		}
	}))
	defer server.Close()
	routeMetadata(t, server)
	for _, mode := range []string{"captured", "captured-absence", "unbound"} {
		t.Run(mode, func(t *testing.T) {
			entries := []string{"CODEX_VERSION=1.2.3", "ANTIGRAVITY_CLI_VERSION=2.3.4"}
			origin, terminal := "codex_cli_rs", "unknown"
			if mode == "captured" {
				entries = append(entries, "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=captured-cli", "TERM_PROGRAM=CapturedTerminal", "TERM_PROGRAM_VERSION=4.5")
				origin, terminal = "captured-cli", "CapturedTerminal/4.5"
			}
			ctx := oauth.ContextWithEnvironment(t.Context(), entries)
			t.Setenv("CODEX_VERSION", "9.9.9")
			t.Setenv("ANTIGRAVITY_CLI_VERSION", "8.8.8")
			t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "ambient-cli")
			t.Setenv("TERM_PROGRAM", "AmbientTerminal")
			t.Setenv("TERM_PROGRAM_VERSION", "9.9")
			codexVersion, geminiVersion := "1.2.3", "2.3.4"
			if mode == "unbound" {
				ctx = t.Context()
				origin, terminal, codexVersion, geminiVersion = "ambient-cli", "AmbientTerminal/9.9", "9.9.9", "8.8.8"
			}
			require.Equal(t, "synthetic@example.invalid", registrytest.Provider("codex").Codex.AccountEmail(ctx, "synthetic-access"))
			header := <-requests
			require.Equal(t, origin, header.Get("originator"))
			require.True(t, strings.HasPrefix(header.Get("User-Agent"), origin+"/"+codexVersion+" ("), header.Get("User-Agent"))
			require.True(t, strings.HasSuffix(header.Get("User-Agent"), " "+terminal), header.Get("User-Agent"))
			require.Equal(t, "synthetic-project", registrytest.Provider("gemini-ag").Gemini.ProjectForCredential(ctx, "synthetic-access"))
			header = <-requests
			require.True(t, strings.HasPrefix(header.Get("User-Agent"), "antigravity/cli/"+geminiVersion+" "), header.Get("User-Agent"))
		})
	}
}

func TestNativeMetadataCancellationReachesActualRequest(t *testing.T) {
	for _, provider := range []string{"codex", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			entered, canceled := make(chan struct{}), make(chan struct{})
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(entered)
				select {
				case <-r.Context().Done():
					close(canceled)
				case <-release:
				}
			}))
			defer func() { close(release); server.Close() }()
			routeMetadata(t, server)
			ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_VERSION=1.2.3", "ANTIGRAVITY_CLI_VERSION=2.3.4"}))
			defer cancel()
			done := make(chan string, 1)
			go func() {
				if provider == "codex" {
					done <- registrytest.Provider("codex").Codex.AccountEmail(ctx, "synthetic-access")
				} else {
					done <- registrytest.Provider("gemini-ag").Gemini.ProjectForCredential(ctx, "synthetic-access")
				}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				t.Fatal("metadata request did not start")
			}
			cancel()
			select {
			case <-canceled:
			case <-time.After(3 * time.Second):
				t.Fatal("canceled metadata request remains active")
			}
			require.Empty(t, <-done)
			require.ErrorIs(t, ctx.Err(), context.Canceled)
		})
	}
}
