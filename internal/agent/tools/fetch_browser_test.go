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
	expectedUA, err := profiles[0].UserAgent(t.Context())
	require.NoError(t, err)
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.UserAgent() != expectedUA {
			t.Errorf("browser user agent not preserved: %s", r.UserAgent())
		}
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
	var reads, prompts int
	profiles := make([]browserFetchProfile, 7)
	for i := range profiles {
		profiles[i] = browserFetchProfile{
			id: fmt.Sprintf("profile-%d", i), name: fmt.Sprintf("Browser %d / ", i) + strings.Repeat("界", 200),
			userAgent: func(context.Context) (string, error) { reads++; return "SelectedBrowser/144", nil },
			cookies:   func(context.Context, *url.URL) (http.CookieJar, error) { return cookiejar.New(nil) },
		}
	}
	selections := []string{"next", "next", "previous", "profile-4", "once"}
	questions := fetchQuestionService{ask: func(req question.Request) ([]question.Answer, error) {
		require.Zero(t, reads)
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
	require.Equal(t, 1, reads)
	for _, test := range []struct {
		name, selection string
		interactive     bool
		profileError    bool
	}{
		{"denied", "deny", true, false},
		{"unknown-answer", "allow-everything", true, false},
		{"noninteractive", "once", false, false},
		{"identity-unavailable", "once", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			reads = 0
			q := fetchQuestionService{ask: func(req question.Request) ([]question.Answer, error) {
				return []question.Answer{{QuestionID: req.Questions[0].ID, SelectedIDs: []string{test.selection}}}, nil
			}}
			service := NewBrowserFetchService(q, test.interactive)
			profile := profiles[0]
			profile.userAgent = func(context.Context) (string, error) {
				reads++
				return "", errors.New("selected profile version unavailable")
			}
			service.profiles = func([]string) []browserFetchProfile { return []browserFetchProfile{profile} }
			client, err := service.client(t.Context(), "session", "call", "https://example.test", nil, http.DefaultClient)
			require.Error(t, err)
			require.Nil(t, client)
			if !test.profileError {
				require.Zero(t, reads)
			}
		})
	}
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
	transport := &browserFetchTransport{profile: profile, userAgent: "SelectedBrowser/144", jars: make(map[string]http.CookieJar)}
	transport.base = sourcegraphRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		cookie, err := req.Cookie("session")
		require.NoError(t, err)
		require.Equal(t, req.URL.Host, cookie.Value)
		require.Equal(t, "SelectedBrowser/144", req.UserAgent())
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
