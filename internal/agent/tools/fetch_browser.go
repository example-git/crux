package tools

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/example-git/crux/internal/cookieutil"
	"github.com/example-git/crux/internal/question"
	"github.com/google/uuid"
	"github.com/pkg/browser"
	"golang.org/x/net/publicsuffix"
)

type browserFetchProfile struct {
	id      string
	name    string
	cookies func(context.Context, *url.URL) (http.CookieJar, error)
}

type BrowserFetchService struct {
	questions   question.Service
	interactive bool
	gate        chan struct{}
	approved    map[string]browserFetchProfile
	profiles    func([]string) []browserFetchProfile
	openURL     func(string) error
}

func NewBrowserFetchService(questions question.Service, interactive bool) *BrowserFetchService {
	return &BrowserFetchService{
		questions: questions, interactive: interactive,
		gate: make(chan struct{}, 1), approved: make(map[string]browserFetchProfile),
		profiles: func(environment []string) []browserFetchProfile {
			var profiles []browserFetchProfile
			for _, profile := range cookieutil.BrowserProfiles(environment) {
				profiles = append(profiles, browserFetchProfile{id: profile.ID, name: profile.Name, cookies: profile.CopyCookiesForURL})
			}
			return profiles
		},
		openURL: browser.OpenURL,
	}
}

func (s *BrowserFetchService) client(ctx context.Context, sessionID, toolCallID, address string, environment []string, base *http.Client) (*http.Client, error) {
	if s == nil || !s.interactive || s.questions == nil {
		return nil, errors.New("user-mode fetch requires an interactive user consent prompt")
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	profile, approved := s.approved[sessionID]
	if !approved {
		profiles := s.profiles(environment)
		if len(profiles) == 0 {
			return nil, errors.New("no supported browser profiles were found on the execution host")
		}
		var err error
		profile, err = s.selectProfile(ctx, sessionID, toolCallID, profiles)
		if err != nil {
			return nil, err
		}
		choice, err := s.ask(ctx, sessionID, toolCallID, "Allow fetch to use this browser session?", fmt.Sprintf("Cookie source: %s\nURL: %s\nCopies cookies and sends recent Google Chrome navigation headers, including redirects. Cloudflare challenges may open in your browser for you to complete. No cookies are written back. Session approval covers all user-mode fetches in this session.", browserFetchLabel(profile.name, 80), browserFetchLabel(address, 180)), []question.Choice{
			{ID: "once", Label: "Allow this request"},
			{ID: "session", Label: "Allow for this session"},
			{ID: "deny", Label: "Deny"},
		})
		if err != nil {
			return nil, err
		}
		switch choice {
		case "deny":
			return nil, question.ErrCancelled
		case "session":
			s.approved[sessionID] = profile
		}
	}
	client := *base
	client.Jar = nil
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &browserFetchTransport{
		base: transport, service: s, profile: profile,
		sessionID: sessionID, toolCallID: toolCallID, jars: make(map[string]http.CookieJar),
	}
	return &client, nil
}

func (s *BrowserFetchService) selectProfile(ctx context.Context, sessionID, toolCallID string, profiles []browserFetchProfile) (browserFetchProfile, error) {
	if len(profiles) == 1 {
		return profiles[0], nil
	}
	for start := 0; ; {
		end := min(start+3, len(profiles))
		choices := make([]question.Choice, 0, question.MaxChoices)
		for _, profile := range profiles[start:end] {
			choices = append(choices, question.Choice{ID: profile.id, Label: browserFetchLabel(profile.name, question.MaxChoiceLabelLength), Description: profile.id})
		}
		if start > 0 {
			choices = append(choices, question.Choice{ID: "previous", Label: "Previous profiles"})
		}
		if end < len(profiles) {
			choices = append(choices, question.Choice{ID: "next", Label: "More profiles"})
		}
		choice, err := s.ask(ctx, sessionID, toolCallID, "Which browser profile should fetch use?", "Choose a profile on the execution host. Browser cookies will not be read until you approve the following consent prompt.", choices)
		if err != nil {
			return browserFetchProfile{}, err
		}
		switch choice {
		case "previous":
			start -= 3
		case "next":
			start = end
		default:
			for _, profile := range profiles[start:end] {
				if profile.id == choice {
					return profile, nil
				}
			}
			return browserFetchProfile{}, errors.New("selected browser profile is unavailable")
		}
	}
}

func (s *BrowserFetchService) ask(ctx context.Context, sessionID, toolCallID string, text, description string, choices []question.Choice) (string, error) {
	id := uuid.NewString()
	answers, err := s.questions.Ask(ctx, question.Request{SessionID: sessionID, ToolCallID: toolCallID, Questions: []question.Question{{ID: id, Type: question.TypeSingleChoice, Text: text, Description: description, Choices: choices}}})
	if err != nil {
		return "", err
	}
	if len(answers) != 1 || answers[0].QuestionID != id || len(answers[0].SelectedIDs) != 1 || answers[0].FillInText != "" {
		return "", errors.New("browser consent received an invalid answer")
	}
	selected := answers[0].SelectedIDs[0]
	for _, choice := range choices {
		if choice.ID == selected {
			return selected, nil
		}
	}
	return "", errors.New("browser consent received an unknown selection")
}

func (s *BrowserFetchService) completeCloudflareChallenge(ctx context.Context, sessionID, toolCallID string, profile browserFetchProfile, address string) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case s.gate <- struct{}{}:
	}
	defer func() { <-s.gate }()
	if s.openURL == nil {
		return errors.New("browser challenge relay is unavailable")
	}
	if err := s.openURL(address); err != nil {
		return fmt.Errorf("failed to open Cloudflare challenge in the browser: %w", err)
	}
	choice, err := s.ask(ctx, sessionID, toolCallID, "Complete the Cloudflare check in your browser", fmt.Sprintf("Browser profile: %s\nURL: %s\nFinish the check in this profile, then return here. Crux will copy fresh cookies from the profile and retry the blocked request once.", browserFetchLabel(profile.name, 80), browserFetchLabel(address, 180)), []question.Choice{
		{ID: "completed", Label: "I completed the check"},
		{ID: "cancel", Label: "Cancel fetch"},
	})
	if err != nil {
		return err
	}
	if choice != "completed" {
		return question.ErrCancelled
	}
	return nil
}

func browserFetchLabel(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit-3], "") + "..."
}

type browserFetchTransport struct {
	base       http.RoundTripper
	service    *BrowserFetchService
	profile    browserFetchProfile
	sessionID  string
	toolCallID string
	jars       map[string]http.CookieJar
}

func (t *browserFetchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.User != nil || request.URL.Hostname() == "" || (request.URL.Scheme != "http" && request.URL.Scheme != "https") {
		return nil, errors.New("browser fetch requires an HTTP or HTTPS URL without embedded credentials")
	}
	for attempt := 0; attempt < 2; attempt++ {
		jar, err := t.jarForRequest(request.Context(), request.URL, attempt > 0)
		if err != nil {
			return nil, err
		}
		prepared := request.Clone(request.Context())
		prepared.Header.Del("Cookie")
		setBrowserFetchHeaders(prepared)
		for _, cookie := range jar.Cookies(prepared.URL) {
			prepared.AddCookie(cookie)
		}
		response, err := t.base.RoundTrip(prepared)
		if err != nil {
			return response, err
		}
		jar.SetCookies(prepared.URL, response.Cookies())
		if !isCloudflareChallenge(response) || attempt > 0 {
			return response, nil
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		if err := t.service.completeCloudflareChallenge(request.Context(), t.sessionID, t.toolCallID, t.profile, prepared.URL.String()); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("Cloudflare challenge retry exhausted")
}

func (t *browserFetchTransport) jarForRequest(ctx context.Context, target *url.URL, refresh bool) (http.CookieJar, error) {
	domain := browserFetchDomain(target)
	if !refresh {
		if jar := t.jars[domain]; jar != nil {
			return jar, nil
		}
	}
	jar, err := t.profile.cookies(ctx, target)
	if err != nil {
		return nil, err
	}
	if jar == nil {
		return nil, errors.New("selected browser cookie copy is unavailable")
	}
	t.jars[domain] = jar
	return jar, nil
}

func browserFetchDomain(target *url.URL) string {
	domain := strings.ToLower(target.Hostname())
	if registered, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		domain = registered
	}
	return domain
}

func setBrowserFetchHeaders(request *http.Request) {
	request.Header.Set("User-Agent", BrowserUserAgent)
	request.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8,application/signed-exchange;v=b3;q=0.7")
	request.Header.Set("Accept-Language", "en-US,en;q=0.9")
	request.Header.Set("Cache-Control", "max-age=0")
	request.Header.Set("Upgrade-Insecure-Requests", "1")
	request.Header.Set("Sec-Ch-Ua", `"Chromium";v="157", "Not_A Brand";v="99", "Google Chrome";v="157"`)
	request.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	request.Header.Set("Sec-Ch-Ua-Platform", fmt.Sprintf("%q", browserClientHintPlatform))
	request.Header.Set("Sec-Fetch-Dest", "document")
	request.Header.Set("Sec-Fetch-Mode", "navigate")
	request.Header.Set("Sec-Fetch-Site", browserFetchSite(request))
	request.Header.Set("Sec-Fetch-User", "?1")
	request.Header.Set("Priority", "u=0, i")
}

func browserFetchSite(request *http.Request) string {
	if request.Response == nil || request.Response.Request == nil || request.Response.Request.URL == nil {
		return "none"
	}
	previous := request.Response.Request.URL
	if strings.EqualFold(previous.Scheme, request.URL.Scheme) && strings.EqualFold(previous.Hostname(), request.URL.Hostname()) && browserFetchPort(previous) == browserFetchPort(request.URL) {
		return "same-origin"
	}
	if strings.EqualFold(previous.Scheme, request.URL.Scheme) && browserFetchDomain(previous) == browserFetchDomain(request.URL) {
		return "same-site"
	}
	return "cross-site"
}

func browserFetchPort(target *url.URL) string {
	if port := target.Port(); port != "" {
		return port
	}
	if target.Scheme == "https" {
		return "443"
	}
	return "80"
}

func isCloudflareChallenge(response *http.Response) bool {
	return response != nil && strings.EqualFold(strings.TrimSpace(response.Header.Get("Cf-Mitigated")), "challenge")
}
