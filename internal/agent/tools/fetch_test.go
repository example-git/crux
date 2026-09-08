package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/permission"
	"github.com/stretchr/testify/require"
)

func runFetchTest(t *testing.T, tool fantasy.AgentTool, session string, params FetchParams) fantasy.ToolResponse {
	t.Helper()
	input, err := json.Marshal(params)
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, session)
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "fetch-test", Name: FetchToolName, Input: string(input)})
	require.NoError(t, err)
	return response
}

func TestFetchParentCancellationRemainsFatal(t *testing.T) {
	for _, phase := range []string{"headers", "body"} {
		t.Run(phase, func(t *testing.T) {
			started := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if phase == "body" {
					w.Header().Set("Content-Type", "text/plain")
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
				}
				close(started)
				<-r.Context().Done()
			}))
			defer server.Close()
			ctx, cancel := context.WithCancel(context.WithValue(t.Context(), SessionIDContextKey, "cancel-fetch"))
			defer cancel()
			tool := NewFetchTool(permission.NewPermissionService(t.TempDir(), true, nil), t.TempDir(), server.Client(), nil, nil)
			input, err := json.Marshal(FetchParams{URL: server.URL})
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() {
				_, err := tool.Run(ctx, fantasy.ToolCall{ID: "cancel-fetch", Name: FetchToolName, Input: string(input)})
				done <- err
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("fetch did not reach the server")
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(5 * time.Second):
				t.Fatal("fetch did not stop after cancellation")
			}
		})
	}
}

func TestFetchMarkdownDefaultPreservesContent(t *testing.T) {
	page := `<html><head><style>style-noise</style><script>` + strings.Repeat("script-noise", 20000) + `</script></head><body>
<header><h1>Reference guide</h1></header><nav><a href="../index">Index</a></nav>
<p>Read <strong>carefully</strong> &amp; use <code>value</code>.</p>
<ul><li>Parent<ul><li>Child</li></ul></li></ul>
<blockquote>Quoted detail</blockquote><aside>Important caveat</aside>
<table><tr><th>Name</th><th>Value</th></tr><tr><td>Alpha</td><td>42</td></tr></table>
<pre><code class="language-go">one()


three()
</code></pre><p><del>Retired</del></p>
<div hidden>hidden-noise</div><div aria-hidden="true">aria-noise</div>
<img alt="Diagram" src="data:image/png;base64,` + strings.Repeat("A", 2000) + `">
<svg><title>Flow chart</title><path d="huge-path-noise"/></svg>
<noscript>Server-rendered fallback</noscript></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/docs/guide", http.StatusFound)
			return
		}
		if r.Header.Get("Cookie") != "" || r.UserAgent() != "crux/1.0" {
			t.Error("normal fetch changed its anonymous request identity")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, page)
	}))
	defer server.Close()
	browser := NewBrowserFetchService(nil, true)
	browser.profiles = func([]string) []browserFetchProfile { panic("normal fetch must not discover browsers") }
	tool := NewFetchTool(permission.NewPermissionService(t.TempDir(), true, nil), t.TempDir(), server.Client(), browser, nil)
	response := runFetchTest(t, tool, "normal", FetchParams{URL: server.URL + "/start"})
	require.False(t, response.IsError, response.Content)
	for _, value := range []string{"# Reference guide", "[Index](" + server.URL + "/index)", "**carefully**", "`value`", "Parent", "Child", "> Quoted detail", "Important caveat", "| Name", "Alpha", "42", "one()\n\n\nthree()", "~~Retired~~", "Diagram", "Flow chart", "Server-rendered fallback"} {
		require.Contains(t, response.Content, value)
	}
	for _, value := range []string{"script-noise", "style-noise", "hidden-noise", "aria-noise", "huge-path-noise", "data:image", "Content truncated"} {
		require.NotContains(t, response.Content, value)
	}
	require.False(t, strings.HasPrefix(response.Content, "```"))
	require.Less(t, len(response.Content), len(page)/100)
	t.Logf("HTML bytes: %d; Markdown bytes: %d", len(page), len(response.Content))
}

func TestFetchFormatsAndCharsets(t *testing.T) {
	for _, test := range []struct {
		name, format, contentType, body, want string
	}{
		{"markdown", "markdown", "text/html", "<p>Hello <b>world</b></p>", "Hello **world**"},
		{"text", "text", "text/html", "<p>Hello <b>world</b></p><script>noise</script>", "Hello world"},
		{"html", "html", "text/html", "<p>Hello</p><script>noise</script>", "<html>\n<body>\n<p>Hello</p><script>noise</script>\n</body>\n</html>"},
		{"xhtml", "", "application/xhtml+xml; charset=utf-8", "<h1>Guide</h1>", "# Guide"},
		{"latin1", "", "text/html; charset=iso-8859-1", "<p>caf\xe9</p>", "café"},
		{"meta-charset", "", "text/html", "<meta charset=windows-1252><p>caf\xe9</p>", "café"},
		{"json", "", "application/json", `{"value":42}`, `{"value":42}`},
		{"plain-markdown", "", "text/markdown", "```go\nmain()\n```", "```go\nmain()\n```"},
		{"base-url", "", "text/html", `<base href="https://docs.example.test/api/"><a href="method">Method</a>`, "[Method](https://docs.example.test/api/method)"},
		{"malformed-html", "", "text/html", "<h2>Title</h2><ul><li>First<li>Second", "## Title\n\n- First\n- Second"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			tool := NewFetchTool(permission.NewPermissionService(t.TempDir(), true, nil), t.TempDir(), server.Client(), nil, nil)
			response := runFetchTest(t, tool, "formats", FetchParams{URL: server.URL, Format: test.format})
			require.False(t, response.IsError, response.Content)
			require.Equal(t, test.want, response.Content)
		})
	}
}

func TestFetchOutputBudgetAndInputErrors(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body, want string
		isError                       bool
	}{
		{"utf8-budget", "text/plain", strings.Repeat("界", MaxFetchSize), "Content truncated", false},
		{"input-limit", "text/html", strings.Repeat("x", maxFetchInputSize+1), "5MB input limit", true},
		{"invalid-utf8", "text/plain", "\xff", "not valid UTF-8", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			tool := NewFetchTool(permission.NewPermissionService(t.TempDir(), true, nil), t.TempDir(), server.Client(), nil, nil)
			response := runFetchTest(t, tool, "limits", FetchParams{URL: server.URL})
			require.Equal(t, test.isError, response.IsError)
			require.Contains(t, response.Content, test.want)
			require.True(t, utf8.ValidString(response.Content))
			require.LessOrEqual(t, len(response.Content), MaxFetchSize)
		})
	}
}

func TestFetchRejectsInvalidExplicitOptionsBeforeAccess(t *testing.T) {
	tool := NewFetchTool(nil, t.TempDir(), nil, nil, nil)
	for _, params := range []FetchParams{
		{URL: "https://example.test", Mode: "automatic"},
		{URL: "https://example.test", Format: "summary"},
		{URL: "https://", Mode: "user"},
		{URL: "https://person:password@example.test", Mode: "user"},
	} {
		response := runFetchTest(t, tool, "invalid", params)
		require.True(t, response.IsError)
	}
}

func TestFetchURLAndConvertUsesSharedMarkdownPipeline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = fmt.Fprint(w, `<header><h1>Title</h1></header><aside>Note</aside><a href="/guide">Guide</a><script>noise</script>`)
	}))
	defer server.Close()
	content, err := FetchURLAndConvert(t.Context(), server.Client(), server.URL)
	require.NoError(t, err)
	require.Contains(t, content, "# Title")
	require.Contains(t, content, "Note")
	require.Contains(t, content, "[Guide]("+server.URL+"/guide)")
	require.NotContains(t, content, "noise")
}
