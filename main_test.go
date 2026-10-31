package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/compatibility"
	"github.com/stretchr/testify/require"
)

type versionRoundTrip func(*http.Request) (*http.Response, error)

func (transport versionRoundTrip) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

func checkCompatibilityVersionsFollowNativeEndpoints(t *testing.T) {
	t.Helper()
	original := http.DefaultClient
	t.Cleanup(func() { http.DefaultClient = original })
	for _, test := range []struct {
		name   string
		url    string
		body   string
		output string
	}{
		{"codex", "https://github.com/openai/codex/releases/latest", "", "codex-cli 8.7.6\n"},
		{"agy", "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/", `{"version":"8.7.6"}`, "8.7.6\n"},
		{"copilot", "https://api.github.com/repos/github/copilot-cli/releases/latest", `{"tag_name":"v8.7.6"}`, "GitHub Copilot CLI 8.7.6\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			http.DefaultClient = &http.Client{Transport: versionRoundTrip(func(request *http.Request) (*http.Response, error) {
				calls++
				require.True(t, strings.HasPrefix(request.URL.String(), test.url), request.URL.String())
				response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body))}
				if test.name == "codex" {
					response.StatusCode = http.StatusFound
					response.Header.Set("Location", "/openai/codex/releases/tag/rust-v8.7.6")
				}
				return response, nil
			})}
			var stdout, stderr bytes.Buffer
			exitCode, handled := compatibility.Dispatch(t.Context(), compatibility.Invocation{
				Executable: "/private/compat/bin/" + test.name,
				Args:       []string{"--version"}, Env: []string{}, WorkingDir: t.TempDir(), Stdout: &stdout, Stderr: &stderr,
			})
			require.True(t, handled)
			require.Zero(t, exitCode, stderr.String())
			require.Equal(t, test.output, stdout.String())
			require.Equal(t, 1, calls)
		})
	}
}

func TestDefaultBuildRegistersCompatibilityAliasesWithoutChangingCrux(t *testing.T) {
	t.Setenv(compatibility.BypassEnvironment, "")
	require.NoError(t, registerCompatibility())
	t.Run("native endpoints", checkCompatibilityVersionsFollowNativeEndpoints)

	var stdout bytes.Buffer
	exitCode, handled := compatibility.Dispatch(t.Context(), compatibility.Invocation{
		Executable: "/private/compat/bin/codex",
		Args:       []string{"--version"},
		Env:        []string{"CODEX_VERSION=8.7.6"},
		WorkingDir: t.TempDir(),
		Stdout:     &stdout,
		Stderr:     &bytes.Buffer{},
	})
	require.True(t, handled)
	require.Zero(t, exitCode)
	require.Equal(t, "codex-cli 8.7.6\n", stdout.String())

	exitCode, handled = compatibility.Dispatch(t.Context(), compatibility.Invocation{
		Executable: "/usr/local/bin/crux",
		WorkingDir: t.TempDir(),
	})
	require.False(t, handled)
	require.Zero(t, exitCode)
}
