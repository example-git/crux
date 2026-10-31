package localaddon

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

type versionRoundTrip func(*http.Request) (*http.Response, error)

func (f versionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func requireVersionOutput(t *testing.T, err error, expected string) {
	t.Helper()
	var exit *compatibility.ExitError
	require.ErrorAs(t, err, &exit)
	require.Zero(t, exit.Code)
	require.Equal(t, expected, exit.Stdout)
}

func TestCodexInitializeUserAgentUsesNativeIdentity(t *testing.T) {
	invocation := compatibility.Invocation{Env: []string{"CODEX_VERSION=0.161.0-alpha.3", "TERM_PROGRAM=iTerm.app", "TERM_PROGRAM_VERSION=3.7.0"}}
	userAgent, err := codexInitializeUserAgent(t.Context(), invocation)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(userAgent, "codex_cli_rs/0.161.0-alpha.3 ("), userAgent)
	require.True(t, strings.HasSuffix(userAgent, ") iTerm.app/3.7.0"), userAgent)
	require.NotContains(t, strings.ToLower(userAgent), "crux")
	invocation.Env = append(invocation.Env, "CODEX_INTERNAL_ORIGINATOR_OVERRIDE=Codex Desktop")
	userAgent, err = codexInitializeUserAgent(t.Context(), invocation)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(userAgent, "Codex Desktop/0.161.0-alpha.3 ("), userAgent)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = codexInitializeUserAgent(ctx, invocation)
	require.ErrorIs(t, err, context.Canceled)
}

func TestCodexInitializeUserAgentUsesNativeVersionLookup(t *testing.T) {
	original := http.DefaultClient
	defer func() { http.DefaultClient = original }()
	requests := 0
	http.DefaultClient = &http.Client{Transport: versionRoundTrip(func(request *http.Request) (*http.Response, error) {
		requests++
		require.Equal(t, "https://github.com/openai/codex/releases/latest", request.URL.String())
		require.Equal(t, http.MethodHead, request.Method)
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"/openai/codex/releases/tag/rust-v8.7.6"}}, Body: http.NoBody}, nil
	})}
	userAgent, err := codexInitializeUserAgent(t.Context(), compatibility.Invocation{Env: []string{"TERM=dumb"}})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(userAgent, "codex_cli_rs/8.7.6 ("), userAgent)
	require.Equal(t, 1, requests)
}

func TestCompatibilityVersionsUseNativeEndpoints(t *testing.T) {
	original := http.DefaultClient
	defer func() { http.DefaultClient = original }()
	var requests []string
	http.DefaultClient = &http.Client{Transport: versionRoundTrip(func(request *http.Request) (*http.Response, error) {
		requests = append(requests, request.URL.String())
		header := make(http.Header)
		body := ""
		status := http.StatusOK
		switch request.URL.Host {
		case "github.com":
			require.Equal(t, "/openai/codex/releases/latest", request.URL.Path)
			require.Equal(t, http.MethodHead, request.Method)
			status = http.StatusFound
			header.Set("Location", "https://github.com/openai/codex/releases/tag/rust-v8.7.6")
		case "api.github.com":
			require.Equal(t, "/repos/github/copilot-cli/releases/latest", request.URL.Path)
			body = `{"tag_name":"v8.7.6"}`
		default:
			require.Contains(t, request.URL.String(), "/manifests/")
			body = `{"version":"8.7.6"}`
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	for _, test := range []struct {
		adapter  compatibility.Adapter
		args     []string
		expected string
		override string
	}{
		{codexAdapter{}, []string{"--version"}, "codex-cli 8.7.6\n", "CODEX_VERSION"},
		{agyAdapter{}, []string{"--version"}, "8.7.6\n", "ANTIGRAVITY_CLI_VERSION"},
		{copilotAdapter{}, []string{"--version"}, "GitHub Copilot CLI 8.7.6\n", "COPILOT_CLI_VERSION"},
		{copilotAdapter{}, []string{"version"}, "GitHub Copilot CLI 8.7.6\n", "COPILOT_CLI_VERSION"},
	} {
		before := len(requests)
		_, err := test.adapter.Translate(t.Context(), compatibility.Invocation{Args: test.args, Env: []string{}})
		requireVersionOutput(t, err, test.expected)
		require.Len(t, requests, before+1)
		_, err = test.adapter.Translate(t.Context(), compatibility.Invocation{Args: test.args, Env: []string{test.override + "=9.8.7"}})
		requireVersionOutput(t, err, strings.ReplaceAll(test.expected, "8.7.6", "9.8.7"))
		require.Len(t, requests, before+1)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = test.adapter.Translate(ctx, compatibility.Invocation{Args: test.args, Env: []string{}})
		require.ErrorIs(t, err, context.Canceled)
	}
}

func TestCodexAliasRetainsDetectedVersionOffline(t *testing.T) {
	original := http.DefaultClient
	defer func() { http.DefaultClient = original }()
	available := true
	http.DefaultClient = &http.Client{Transport: versionRoundTrip(func(*http.Request) (*http.Response, error) {
		if !available {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: http.NoBody}, nil
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"/openai/codex/releases/tag/rust-v0.159.2"}},
			Body:       http.NoBody,
		}, nil
	})}
	home := t.TempDir()
	invocation := compatibility.Invocation{Args: []string{"--version"}, Env: []string{"HOME=" + home, "USERPROFILE=" + home}}
	_, err := (codexAdapter{}).Translate(t.Context(), invocation)
	requireVersionOutput(t, err, "codex-cli 0.159.2\n")
	available = false
	_, err = (codexAdapter{}).Translate(t.Context(), invocation)
	requireVersionOutput(t, err, "codex-cli 0.159.2\n")
}

func TestClaudeVersionUsesTrustedNativeOwner(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = fmt.Fprint(w, "8.7.6")
	}))
	defer host.Close()
	original := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	defer func() { http.DefaultTransport = original }()
	endpoint, err := url.Parse(host.URL)
	require.NoError(t, err)
	root := t.TempDir()
	source := filepath.Join(root, "claude.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	data, err := os.ReadFile("../../../docs/provider-plugins/examples/minimal.plugin/manifest.json")
	require.NoError(t, err)
	for path, value := range map[string]any{
		"id":                                     "test.claude.version",
		"provider.id":                            "claude-ai",
		"provider.account_namespace":             "test.claude.version",
		"capabilities.endpoints.0.base_url":      host.URL,
		"capabilities.endpoints.0.allowed_hosts": []string{endpoint.Hostname()},
		"capabilities.operations.0.protocol":     "anthropic-messages",
		"capabilities.operations.0.transport":    "sse",
		"capabilities.operations.0.path":         "/v1/messages",
		"capabilities.anthropic": manifest.AnthropicPolicy{
			ClientIdentity: &manifest.ResolvedClientIdentity{
				Environment: "CLAUDE_CODE_VERSION", LatestURL: host.URL + "/latest", CacheKey: "claude-version-test",
				FallbackVersion: "0.0.1", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "claude-cli/{version} ({os}; {arch})",
				ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024,
			},
			MaxRequestBytes: 1 << 20, TransformFailure: "error",
		},
	} {
		data, err = sjson.SetBytes(data, path, value)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	global := filepath.Join(root, "global")
	cache := filepath.Join(root, "cache")
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(global, cache))
	require.NoError(t, err)
	defer manager.Close()
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	_, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{Digest: status.Digest, Trusted: true})
	require.NoError(t, err)
	configPath := filepath.Join(root, "crux.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{}`), 0o600))
	invocation := compatibility.Invocation{Args: []string{"--version"}, WorkingDir: root, Env: []string{
		"HOME=" + root, "CRUX_GLOBAL_DATA=" + global, "CRUX_CACHE_DIR=" + cache, "CRUX_GLOBAL_CONFIG=" + root,
	}}
	_, err = (claudeAdapter{}).Translate(t.Context(), invocation)
	requireVersionOutput(t, err, "8.7.6 (Claude Code)\n")
	require.EqualValues(t, 1, requests.Load())
	invocation.Env = append(invocation.Env, "CLAUDE_CODE_VERSION=9.8.7")
	_, err = (claudeAdapter{}).Translate(t.Context(), invocation)
	requireVersionOutput(t, err, "9.8.7 (Claude Code)\n")
	require.EqualValues(t, 1, requests.Load())
	data, err = os.ReadFile(configPath)
	require.NoError(t, err)
	require.Equal(t, `{}`, string(data))
	_, err = os.Stat(filepath.Join(global, "crux.json"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, os.WriteFile(configPath, []byte(`{"options":{"disable_default_providers":true}}`), 0o600))
	_, err = (claudeAdapter{}).Translate(t.Context(), invocation)
	require.ErrorContains(t, err, "requires an active trusted client identity declaration")
	require.EqualValues(t, 1, requests.Load())
}
