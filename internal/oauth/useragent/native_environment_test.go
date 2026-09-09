package useragent

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/stretchr/testify/require"
)

func TestNativeContextIdentityCapturesEnvironment(t *testing.T) {
	entries := []string{"CODEX_VERSION=1.2.3", "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=captured-cli", "TERM_PROGRAM=CapturedTerminal", "TERM_PROGRAM_VERSION=4.5", "ANTIGRAVITY_CLI_VERSION=2.3.4"}
	ctx := oauth.ContextWithEnvironment(t.Context(), entries)
	t.Setenv("CODEX_VERSION", "9.9.9")
	t.Setenv("CODEX_INTERNAL_ORIGINATOR_OVERRIDE", "ambient-cli")
	t.Setenv("TERM_PROGRAM", "AmbientTerminal")
	t.Setenv("TERM_PROGRAM_VERSION", "9.9")
	t.Setenv("TERM", "ambient-term")
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "8.8.8")
	value, err := CodexForContext(ctx)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(value, "captured-cli/1.2.3 ("), value)
	require.True(t, strings.HasSuffix(value, " CapturedTerminal/4.5"), value)
	value, err = GeminiForContext(ctx)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(value, "antigravity/cli/2.3.4 "), value)
	for _, terminal := range []struct {
		entries  []string
		expected string
	}{{nil, "unknown"}, {[]string{"TERM=xterm-captured"}, "xterm-captured"}, {[]string{"TERM_PROGRAM=CapturedProgram"}, "CapturedProgram"}} {
		ctx := oauth.ContextWithEnvironment(t.Context(), append([]string{"CODEX_VERSION=1.2.3"}, terminal.entries...))
		value, err := CodexForContext(ctx)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(value, "codex_cli_rs/1.2.3 ("), value)
		require.True(t, strings.HasSuffix(value, " "+terminal.expected), value)
	}
	value, err = CodexForContext(t.Context())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(value, "ambient-cli/9.9.9 ("), value)
	require.True(t, strings.HasSuffix(value, " AmbientTerminal/9.9"), value)
	value, err = GeminiForContext(t.Context())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(value, "antigravity/cli/8.8.8 "), value)
}

func TestNativeContextVersionsBoundAbsenceAndCancellation(t *testing.T) {
	t.Setenv("CODEX_VERSION", "9.9.9")
	t.Setenv("ANTIGRAVITY_CLI_VERSION", "9.9.9")
	original := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: contextRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	defer func() { http.DefaultClient = original }()
	ctx := oauth.ContextWithEnvironment(t.Context(), nil)
	version, err := CodexVersionForContext(ctx)
	require.NoError(t, err)
	require.Equal(t, staticCodexVersion, version)
	version, err = GeminiVersionForContext(ctx)
	require.NoError(t, err)
	require.Equal(t, staticGeminiVersion, version)
	ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), []string{"CODEX_VERSION=1.2.3", "ANTIGRAVITY_CLI_VERSION=1.2.3"}))
	cancel()
	_, err = CodexForContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
	_, err = GeminiForContext(ctx)
	require.ErrorIs(t, err, context.Canceled)
}

func TestNativeMuslProbeUsesCapturedExecutableAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	for _, path := range []string{"/lib/libc.musl-x86_64.so.1", "/lib/libc.musl-aarch64.so.1"} {
		if _, err := os.Stat(path); err == nil {
			t.Skip("installed musl library bypasses executable probe")
		}
	}
	bin, ambient := t.TempDir(), t.TempDir()
	for _, directory := range []string{bin, ambient} {
		require.NoError(t, os.WriteFile(filepath.Join(directory, "ldd"), []byte("#!/bin/sh\nprintf 'musl 1.2.3\\n' >&2\n"), 0700))
	}
	t.Setenv("PATH", ambient)
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"PATH=" + bin})
	require.True(t, isMuslLinuxForContext(ctx))
	require.False(t, isMuslLinuxForContext(oauth.ContextWithEnvironment(t.Context(), nil)))
	require.True(t, isMuslLinuxForContext(t.Context()))
	ready := filepath.Join(t.TempDir(), "ready")
	require.NoError(t, os.WriteFile(filepath.Join(bin, "ldd"), []byte("#!/bin/sh\nprintf started > \"$PROBE_READY\"\nexec /bin/sleep 60\n"), 0700))
	ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), []string{"PATH=" + bin, "PROBE_READY=" + ready}))
	defer cancel()
	done := make(chan bool, 1)
	go func() { done <- isMuslLinuxForContext(ctx) }()
	require.Eventually(t, func() bool { _, err := os.Stat(ready); return err == nil }, 3*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case value := <-done:
		require.False(t, value)
	case <-time.After(time.Second):
		t.Fatal("canceled platform probe remains active")
	}
}
