package tools

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/cookieutil"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/question"
	"github.com/stretchr/testify/require"
)

func requireChromeFetchHeaders(t *testing.T, request *http.Request, fetchSite string) {
	t.Helper()
	require.Equal(t, BrowserUserAgent, request.UserAgent())
	require.NotContains(t, strings.ToLower(request.UserAgent()), "crux")
	for name, value := range map[string]string{
		"Accept-Language":           "en-US,en;q=0.9",
		"Cache-Control":             "max-age=0",
		"Priority":                  "u=0, i",
		"Sec-Ch-Ua":                 `"Chromium";v="157", "Not_A Brand";v="99", "Google Chrome";v="157"`,
		"Sec-Ch-Ua-Mobile":          "?0",
		"Sec-Ch-Ua-Platform":        fmt.Sprintf("%q", browserClientHintPlatform),
		"Sec-Fetch-Dest":            "document",
		"Sec-Fetch-Mode":            "navigate",
		"Sec-Fetch-Site":            fetchSite,
		"Sec-Fetch-User":            "?1",
		"Upgrade-Insecure-Requests": "1",
	} {
		require.Equal(t, value, request.Header.Get(name), name)
	}
}

func TestFetchBrowserConsentAndCopiedCookies(t *testing.T) {
	const initial = "synthetic*fetch*original-cookie-81903"
	const rotated = "synthetic*fetch*rotated-cookie-74981"
	home := t.TempDir()
	profileDir := ""
	switch runtime.GOOS {
	case "darwin":
		profileDir = filepath.Join(home, "Library", "Application Support", "Firefox", "Profiles", "synthetic")
	case "linux":
		profileDir = filepath.Join(home, ".mozilla", "firefox", "synthetic")
	case "windows":
		profileDir = filepath.Join(home, "Mozilla", "Firefox", "Profiles", "synthetic")
	default:
		t.Skip("browser profiles unavailable on this platform")
	}
	require.NoError(t, os.MkdirAll(profileDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(profileDir, "compatibility.ini"), []byte("[Compatibility]\nLastVersion=144.0_20260905/20260905\n"), 0o600))
	databasePath := filepath.Join(profileDir, "cookies.sqlite")
	database, err := sql.Open("sqlite", databasePath)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `CREATE TABLE moz_cookies (host TEXT, path TEXT, isSecure INTEGER, expiry INTEGER, name TEXT, value TEXT, isHttpOnly INTEGER)`)
	require.NoError(t, err)
	_, err = database.ExecContext(t.Context(), `INSERT INTO moz_cookies VALUES ('127.0.0.1', '/', 1, 0, 'session', ?, 1)`, initial)
	require.NoError(t, err)
	require.NoError(t, database.Close())
	before, err := os.ReadFile(databasePath)
	require.NoError(t, err)
	environment := []string{"HOME=" + home, "APPDATA=" + home, "LOCALAPPDATA=" + home}
	profiles := cookieutil.BrowserProfiles(environment)
	require.Len(t, profiles, 1)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		requireChromeFetchHeaders(t, r, map[string]string{"/start": "none", "/result": "same-origin"}[r.URL.Path])
		cookie, err := r.Cookie("session")
		if err != nil {
			t.Error("missing browser cookie")
			http.Error(w, "missing session", http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/start" {
			if cookie.Value != initial {
				t.Error("initial operational cookie was changed")
			}
			http.SetCookie(w, &http.Cookie{Name: "session", Value: rotated, Path: "/", Secure: true, HttpOnly: true})
			http.Redirect(w, r, "/result", http.StatusFound)
			return
		}
		if cookie.Value != rotated {
			t.Error("rotated operational cookie was not reused")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprintf(w, "<h1>Authenticated content</h1><p>%s %s</p>", initial, rotated)
	}))
	defer server.Close()
	client := server.Client()
	client.Transport = cruxlog.WrapHTTPTransport(client.Transport)
	questions := question.NewService()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	events := questions.Subscribe(ctx)
	browser := NewBrowserFetchService(questions, true)
	permissions := permission.NewPermissionService(t.TempDir(), true, []string{FetchToolName})
	permissions.AutoApproveSession("primary")
	type result struct {
		response fantasy.ToolResponse
		err      error
	}
	for _, step := range []struct {
		session, choice string
		allowed         bool
	}{
		{"primary", "once", true},
		{"primary", "session", true},
		{"primary", "", true},
		{"different", "deny", false},
		{"cancelled", "cancel", false},
	} {
		tool := NewFetchTool(permissions, t.TempDir(), client, browser, environment)
		input, err := json.Marshal(FetchParams{URL: server.URL + "/start", Mode: "user"})
		require.NoError(t, err)
		done := make(chan result, 1)
		previousRequests := requests.Load()
		go func() {
			response, err := tool.Run(context.WithValue(ctx, SessionIDContextKey, step.session), fantasy.ToolCall{ID: "browser-fetch", Name: FetchToolName, Input: string(input)})
			done <- result{response, err}
		}()
		if step.choice != "" {
			select {
			case event := <-events:
				require.Equal(t, previousRequests, requests.Load(), "network access preceded consent")
				require.Contains(t, event.Payload.Questions[0].Description, "Firefox / synthetic")
				require.Contains(t, event.Payload.Questions[0].Description, server.URL)
				if step.choice == "cancel" {
					require.True(t, questions.Cancel())
				} else {
					require.True(t, questions.Answer([]question.Answer{{QuestionID: event.Payload.Questions[0].ID, SelectedIDs: []string{step.choice}}}))
				}
			case got := <-done:
				t.Fatalf("user mode completed without consent: %+v", got)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		select {
		case got := <-done:
			require.NoError(t, got.err)
			if step.allowed {
				require.False(t, got.response.IsError, got.response.Content)
				require.Contains(t, got.response.Content, "# Authenticated content")
				require.NotContains(t, got.response.Content, initial)
				require.NotContains(t, got.response.Content, rotated)
				require.NotContains(t, got.response.Content, "original-cookie-81903")
				require.NotContains(t, got.response.Content, "rotated-cookie-74981")
				require.Equal(t, previousRequests+2, requests.Load())
			} else {
				require.True(t, got.response.StopTurn)
				require.Equal(t, previousRequests, requests.Load())
			}
		case <-events:
			t.Fatal("unexpected extra consent prompt")
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	after, err := os.ReadFile(databasePath)
	require.NoError(t, err)
	require.Equal(t, before, after, "browser cookie database was modified")
}

type fetchQuestionService struct {
	question.Service
	ask func(question.Request) ([]question.Answer, error)
}

func (s fetchQuestionService) Ask(_ context.Context, request question.Request) ([]question.Answer, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	return s.ask(request)
}

func TestFetchBrowserProfileSelectionAndFailures(t *testing.T) {
	var prompts int
	profiles := make([]browserFetchProfile, 7)
	for i := range profiles {
		profiles[i] = browserFetchProfile{
			id: fmt.Sprintf("profile-%d", i), name: fmt.Sprintf("Browser %d / ", i) + strings.Repeat("界", 200),
			cookies: func(context.Context, *url.URL) (http.CookieJar, error) { return cookiejar.New(nil) },
		}
	}
	selections := []string{"next", "next", "previous", "profile-4", "once"}
	questions := fetchQuestionService{ask: func(req question.Request) ([]question.Answer, error) {
		selection := selections[prompts]
		prompts++
		return []question.Answer{{QuestionID: req.Questions[0].ID, SelectedIDs: []string{selection}}}, nil
	}}
	browser := NewBrowserFetchService(questions, true)
	browser.profiles = func([]string) []browserFetchProfile { return profiles }
	client, err := browser.client(t.Context(), "session", "call", "https://example.test/"+strings.Repeat("x", 1000), nil, http.DefaultClient)
	require.NoError(t, err)
	require.Equal(t, "profile-4", client.Transport.(*browserFetchTransport).profile.id)
	require.Equal(t, len(selections), prompts)
	for _, test := range []struct {
		name, selection string
		interactive     bool
	}{
		{"denied", "deny", true},
		{"unknown-answer", "allow-everything", true},
		{"noninteractive", "once", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			q := fetchQuestionService{ask: func(req question.Request) ([]question.Answer, error) {
				return []question.Answer{{QuestionID: req.Questions[0].ID, SelectedIDs: []string{test.selection}}}, nil
			}}
			service := NewBrowserFetchService(q, test.interactive)
			service.profiles = func([]string) []browserFetchProfile { return []browserFetchProfile{profiles[0]} }
			client, err := service.client(t.Context(), "session", "call", "https://example.test", nil, http.DefaultClient)
			require.Error(t, err)
			require.Nil(t, client)
		})
	}
}

func TestFetchBrowserCloudflareRelayRefreshesCookiesForLaterFetches(t *testing.T) {
	target, err := url.Parse("https://protected.example.test/account")
	require.NoError(t, err)
	var cookieCopies atomic.Int32
	var requests atomic.Int32
	var opened atomic.Int32
	var clearanceReady atomic.Bool
	profile := browserFetchProfile{id: "chrome-default", name: "Chrome / Default", cookies: func(_ context.Context, requested *url.URL) (http.CookieJar, error) {
		cookieCopies.Add(1)
		jar, err := cookiejar.New(nil)
		if err != nil {
			return nil, err
		}
		value := "stale-clearance"
		if clearanceReady.Load() {
			value = "fresh-clearance"
		}
		jar.SetCookies(requested, []*http.Cookie{{Name: "cf_clearance", Value: value, Path: "/", Secure: true, HttpOnly: true}})
		return jar, nil
	}}
	selections := []string{"session", "completed"}
	prompts := 0
	questions := fetchQuestionService{ask: func(request question.Request) ([]question.Answer, error) {
		selection := selections[prompts]
		prompts++
		return []question.Answer{{QuestionID: request.Questions[0].ID, SelectedIDs: []string{selection}}}, nil
	}}
	browserService := NewBrowserFetchService(questions, true)
	browserService.profiles = func([]string) []browserFetchProfile { return []browserFetchProfile{profile} }
	browserService.openURL = func(address string) error {
		opened.Add(1)
		require.Equal(t, target.String(), address)
		clearanceReady.Store(true)
		return nil
	}
	base := sourcegraphRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requestNumber := requests.Add(1)
		requireChromeFetchHeaders(t, request, "none")
		cookie, err := request.Cookie("cf_clearance")
		require.NoError(t, err)
		header := http.Header{"Content-Type": []string{"text/html; charset=utf-8"}}
		status := http.StatusOK
		body := "<h1>Authenticated page</h1>"
		if cookie.Value != "fresh-clearance" {
			require.Equal(t, int32(1), requestNumber)
			status = http.StatusForbidden
			header.Set("Cf-Mitigated", "challenge")
			body = "<title>Just a moment...</title>"
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	tool := NewWebFetchToolWithIdentity(t.TempDir(), &http.Client{Transport: base}, FetchIdentity{Mode: "user", Browser: browserService, SessionID: "fetch-session"})
	run := func(callID string) fantasy.ToolResponse {
		input, err := json.Marshal(WebFetchParams{URL: target.String()})
		require.NoError(t, err)
		response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: callID, Name: WebFetchToolName, Input: string(input)})
		require.NoError(t, err)
		return response
	}
	first := run("first-page")
	require.False(t, first.IsError, first.Content)
	require.Contains(t, first.Content, "# Authenticated page")
	require.Equal(t, int32(2), requests.Load())
	require.Equal(t, int32(2), cookieCopies.Load())
	require.Equal(t, int32(1), opened.Load())
	require.Equal(t, 2, prompts)

	second := run("delegated-page")
	require.False(t, second.IsError, second.Content)
	require.Contains(t, second.Content, "# Authenticated page")
	require.Equal(t, int32(3), requests.Load())
	require.Equal(t, int32(3), cookieCopies.Load())
	require.Equal(t, int32(1), opened.Load())
	require.Equal(t, 2, prompts)
}

func TestFetchBrowserCloudflareRelayCancellationStopsBeforeRetry(t *testing.T) {
	target, err := url.Parse("https://protected.example.test/account")
	require.NoError(t, err)
	profile := browserFetchProfile{id: "chrome-default", name: "Chrome / Default", cookies: func(_ context.Context, requested *url.URL) (http.CookieJar, error) {
		jar, err := cookiejar.New(nil)
		if err == nil {
			jar.SetCookies(requested, []*http.Cookie{{Name: "cf_clearance", Value: "stale", Path: "/", Secure: true}})
		}
		return jar, err
	}}
	selections := []string{"once", "cancel"}
	prompts := 0
	questions := fetchQuestionService{ask: func(request question.Request) ([]question.Answer, error) {
		selection := selections[prompts]
		prompts++
		return []question.Answer{{QuestionID: request.Questions[0].ID, SelectedIDs: []string{selection}}}, nil
	}}
	browserService := NewBrowserFetchService(questions, true)
	browserService.profiles = func([]string) []browserFetchProfile { return []browserFetchProfile{profile} }
	var opened, requests atomic.Int32
	browserService.openURL = func(address string) error {
		opened.Add(1)
		require.Equal(t, target.String(), address)
		return nil
	}
	base := sourcegraphRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests.Add(1)
		requireChromeFetchHeaders(t, request, "none")
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Cf-Mitigated": []string{"challenge"}}, Body: io.NopCloser(strings.NewReader("challenge")), Request: request}, nil
	})
	tool := NewWebFetchToolWithIdentity(t.TempDir(), &http.Client{Transport: base}, FetchIdentity{Mode: "user", Browser: browserService, SessionID: "fetch-session"})
	input, err := json.Marshal(WebFetchParams{URL: target.String()})
	require.NoError(t, err)
	response, err := tool.Run(t.Context(), fantasy.ToolCall{ID: "cancelled-page", Name: WebFetchToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.StopTurn)
	require.Equal(t, int32(1), requests.Load())
	require.Equal(t, int32(1), opened.Load())
	require.Equal(t, 2, prompts)
}

func TestFetchBrowserRedirectCookieIsolationAndImportFailure(t *testing.T) {
	var loaded []string
	profile := browserFetchProfile{cookies: func(_ context.Context, target *url.URL) (http.CookieJar, error) {
		loaded = append(loaded, target.Host)
		if target.Host == "unavailable.test" {
			return nil, errors.New("cookie copy failed")
		}
		jar, _ := cookiejar.New(nil)
		jar.SetCookies(target, []*http.Cookie{{Name: "session", Value: target.Host, Path: "/"}})
		return jar, nil
	}}
	transport := &browserFetchTransport{profile: profile, jars: make(map[string]http.CookieJar)}
	transport.base = sourcegraphRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		cookie, err := req.Cookie("session")
		require.NoError(t, err)
		require.Equal(t, req.URL.Host, cookie.Value)
		requireChromeFetchHeaders(t, req, map[string]string{"first.test": "none", "second.test": "cross-site"}[req.URL.Host])
		header := http.Header{}
		status := http.StatusOK
		if req.URL.Host == "first.test" {
			status = http.StatusFound
			header.Set("Location", "https://second.test/result")
		}
		return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader("ok")), Request: req}, nil
	})
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://first.test/start", nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", "unrelated=must-not-leak")
	response, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	require.Equal(t, []string{"first.test", "second.test"}, loaded)
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, "https://unavailable.test", nil)
	require.NoError(t, err)
	response, err = client.Do(req)
	if response != nil {
		require.NoError(t, response.Body.Close())
	}
	require.ErrorContains(t, err, "cookie copy failed")
}
