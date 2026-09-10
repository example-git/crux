package useragent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

type contextRoundTrip func(*http.Request) (*http.Response, error)

func (f contextRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCopilotCapturedIdentitySettingsAndAbsence(t *testing.T) {
	t.Setenv("COPILOT_ADVERTISE_MODE", "cli")
	t.Setenv("COPILOT_CLI_VERSION", "9.9.9")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "9.9.9")
	t.Setenv("COPILOT_VSCODE_INTEGRATION_ID", "ambient-integration")
	t.Setenv("COPILOT_VSCODE_EDITOR_VERSION", "ambient-editor")
	t.Setenv("COPILOT_VSCODE_EDITOR_PLUGIN_VERSION", "ambient-plugin")
	t.Setenv("TERM_PROGRAM", "ambient-terminal")
	for _, explicit := range []bool{true, false} {
		entries := []string{"COPILOT_VSCODE_EXTENSION_VERSION=1.2.3"}
		if explicit {
			entries = append(entries, "COPILOT_ADVERTISE_MODE=vscode", "COPILOT_VSCODE_INTEGRATION_ID=captured-integration", "COPILOT_VSCODE_EDITOR_VERSION=captured-editor", "COPILOT_VSCODE_EDITOR_PLUGIN_VERSION=captured-plugin")
		}
		identity, err := CopilotForContext(oauth.ContextWithEnvironment(t.Context(), entries))
		require.NoError(t, err)
		require.Equal(t, CopilotModeVSCode, identity.Mode)
		require.Equal(t, "GitHubCopilotChat/1.2.3", identity.UserAgent)
		if explicit {
			require.Equal(t, "captured-integration", identity.IntegrationID)
			require.Equal(t, "captured-editor", identity.EditorVersion)
			require.Equal(t, "captured-plugin", identity.EditorPluginVersion)
		} else {
			require.Equal(t, "vscode-chat", identity.IntegrationID)
			require.Equal(t, copilotVSCodeEditorVersion, identity.EditorVersion)
			require.Equal(t, "copilot-chat/1.2.3", identity.EditorPluginVersion)
		}
	}
	identity, err := CopilotForContext(oauth.ContextWithEnvironment(t.Context(), []string{"COPILOT_ADVERTISE_MODE=cli", "COPILOT_CLI_VERSION=2.3.4"}))
	require.NoError(t, err)
	require.Contains(t, identity.UserAgent, "copilot/2.3.4 ")
	require.Contains(t, identity.UserAgent, "term/terminal")
	require.NotContains(t, identity.UserAgent, "ambient-terminal")
	identity, err = CopilotForContext(t.Context())
	require.NoError(t, err)
	require.Equal(t, CopilotModeCLI, identity.Mode)
	require.Contains(t, identity.UserAgent, "copilot/9.9.9 ")
	require.Contains(t, identity.UserAgent, "term/ambient-terminal")
}

func TestCopilotCapturedVersionFallbackFiles(t *testing.T) {
	original := http.DefaultClient
	var requests atomic.Int32
	http.DefaultClient = &http.Client{Transport: contextRoundTrip(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		return &http.Response{StatusCode: 404, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	defer func() { http.DefaultClient = original }()
	ambient, captured := t.TempDir(), t.TempDir()
	write := func(home, path, data string) {
		require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(home, path)), 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(home, path), []byte(data), 0o600))
	}
	write(ambient, ".config/github-copilot/versions.json", `{"version":"9.8.7"}`)
	write(ambient, ".ai-cli/useragent-versions.json", `{"copilot-cli":"9.8.7","copilot-extension":"9.8.7"}`)
	write(captured, ".config/github-copilot/versions.json", `{"version":"3.4.5"}`)
	write(captured, ".ai-cli/useragent-versions.json", `{"copilot-cli":"4.5.6","copilot-extension":"4.5.6"}`)
	t.Setenv("HOME", ambient)
	t.Setenv("USERPROFILE", ambient)
	t.Setenv("COPILOT_CLI_VERSION", "8.8.8")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "8.8.8")
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"HOME=" + captured, "USERPROFILE=" + captured})
	for _, resolve := range []func(context.Context) (string, error){CopilotCLIVersionForContext, CopilotExtensionVersionForContext} {
		version, err := resolve(ctx)
		require.NoError(t, err)
		require.Equal(t, "3.4.5", version)
	}
	require.NoError(t, os.Remove(filepath.Join(captured, ".config/github-copilot/versions.json")))
	for _, resolve := range []func(context.Context) (string, error){CopilotCLIVersionForContext, CopilotExtensionVersionForContext} {
		version, err := resolve(ctx)
		require.NoError(t, err)
		require.Equal(t, "4.5.6", version)
	}
	version, err := CopilotCLIVersionForContext(oauth.ContextWithEnvironment(t.Context(), nil))
	require.NoError(t, err)
	require.Equal(t, staticCopilotCLIVersion, version)
	version, err = CopilotExtensionVersionForContext(oauth.ContextWithEnvironment(t.Context(), nil))
	require.NoError(t, err)
	require.Equal(t, staticCopilotExtensionVersion, version)
	require.EqualValues(t, 6, requests.Load())
	data, err := os.ReadFile(filepath.Join(ambient, ".ai-cli/useragent-versions.json"))
	require.NoError(t, err)
	require.Equal(t, `{"copilot-cli":"9.8.7","copilot-extension":"9.8.7"}`, string(data))
}

func TestCopilotCapturedProbePathAndChildEnvironment(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	bin := t.TempDir()
	ambient := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "github-copilot-cli"), []byte("#!/bin/sh\nprintf '%s\\n' \"$SYNTHETIC_PROBE_VERSION\"\n"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(ambient, "github-copilot-cli"), []byte("#!/bin/sh\nprintf '9.9.9\\n'\n"), 0o700))
	t.Setenv("PATH", ambient)
	t.Setenv("SYNTHETIC_PROBE_VERSION", "9.8.7")
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"PATH=" + bin, "SYNTHETIC_PROBE_VERSION=5.6.7"})
	require.Equal(t, "5.6.7", runToolVersionForContext(ctx, "github-copilot-cli"))
	require.Empty(t, runToolVersionForContext(oauth.ContextWithEnvironment(t.Context(), nil), "github-copilot-cli"))
	require.NoError(t, os.WriteFile(filepath.Join(bin, "github-copilot-cli"), []byte("#!/bin/sh\nexec /bin/sleep 60\n"), 0o700))
	canceled, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := commandOutputForContext(canceled, "github-copilot-cli", "--version")
	require.Error(t, err)
	require.ErrorIs(t, canceled.Err(), context.DeadlineExceeded)
	require.Less(t, time.Since(start), time.Second)
}

func TestCopilotCapturedIdentityRejectsCanceledAndReplacedOwner(t *testing.T) {
	ctx := oauth.ContextWithEnvironment(t.Context(), []string{"COPILOT_VSCODE_EXTENSION_VERSION=1.2.3"})
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err := CopilotForContext(canceled)
	require.ErrorIs(t, err, context.Canceled)
	ctx = providertransport.ContextWithOwnerValidator(ctx, func() error { return errors.New("owner replaced") })
	_, err = CopilotForContext(ctx)
	require.ErrorContains(t, err, "owner replaced")
}

func TestCopilotCapturedMetadataCancellationReachesHTTPS(t *testing.T) {
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
	original := http.DefaultClient
	target, err := url.Parse(server.URL)
	require.NoError(t, err)
	http.DefaultClient = &http.Client{Transport: contextRoundTrip(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "marketplace.visualstudio.com", r.URL.Host)
		copy := r.Clone(r.Context())
		address := *r.URL
		address.Scheme, address.Host = target.Scheme, target.Host
		copy.URL = &address
		return server.Client().Transport.RoundTrip(copy)
	})}
	defer func() { http.DefaultClient = original }()
	ctx, cancel := context.WithCancel(oauth.ContextWithEnvironment(t.Context(), nil))
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := CopilotExtensionVersionForContext(ctx); done <- err }()
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
	require.ErrorIs(t, <-done, context.Canceled)
}
