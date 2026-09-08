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
	"golang.org/x/net/publicsuffix"
)

type browserFetchProfile struct {
	id        string
	name      string
	userAgent func(context.Context) (string, error)
	cookies   func(context.Context, *url.URL) (http.CookieJar, error)
}

type BrowserFetchService struct {
	questions   question.Service
	interactive bool
	gate        chan struct{}
	approved    map[string]browserFetchProfile
	profiles    func([]string) []browserFetchProfile
}

func NewBrowserFetchService(questions question.Service, interactive bool) *BrowserFetchService {
	return &BrowserFetchService{
		questions: questions, interactive: interactive,
		gate: make(chan struct{}, 1), approved: make(map[string]browserFetchProfile),
		profiles: func(environment []string) []browserFetchProfile {
			var profiles []browserFetchProfile
			for _, profile := range cookieutil.BrowserProfiles(environment) {
				profiles = append(profiles, browserFetchProfile{id: profile.ID, name: profile.Name, userAgent: profile.UserAgent, cookies: profile.CopyCookiesForURL})
			}
			return profiles
		},
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
		choice, err := s.ask(ctx, sessionID, toolCallID, "Allow fetch to use this browser session?", fmt.Sprintf("Browser: %s\nURL: %s\nCopies cookies and uses a matching browser user agent, including redirects. No cookies are written back. Session approval covers all user-mode fetches in this session.", browserFetchLabel(profile.name, 120), browserFetchLabel(address, 280)), []question.Choice{
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
	userAgent, err := profile.userAgent(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(userAgent) == "" || strings.ContainsAny(userAgent, "\r\n") {
		return nil, errors.New("selected browser user agent is invalid")
	}
	client := *base
	client.Jar = nil
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &browserFetchTransport{base: transport, profile: profile, userAgent: userAgent, jars: make(map[string]http.CookieJar)}
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

func (s *BrowserFetchService) ask(ctx context.Context, sessionID, toolCallID, text, description string, choices []question.Choice) (string, error) {
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

func browserFetchLabel(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit-3], "") + "..."
}

type browserFetchTransport struct {
	base      http.RoundTripper
	profile   browserFetchProfile
	userAgent string
	jars      map[string]http.CookieJar
}

func (t *browserFetchTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.User != nil || request.URL.Hostname() == "" || (request.URL.Scheme != "http" && request.URL.Scheme != "https") {
		return nil, errors.New("browser fetch requires an HTTP or HTTPS URL without embedded credentials")
	}
	domain := strings.ToLower(request.URL.Hostname())
	if registered, err := publicsuffix.EffectiveTLDPlusOne(domain); err == nil {
		domain = registered
	}
	jar := t.jars[domain]
	if jar == nil {
		var err error
		jar, err = t.profile.cookies(request.Context(), request.URL)
		if err != nil {
			return nil, err
		}
		if jar == nil {
			return nil, errors.New("selected browser cookie copy is unavailable")
		}
		t.jars[domain] = jar
	}
	request = request.Clone(request.Context())
	request.Header.Del("Cookie")
	request.Header.Set("User-Agent", t.userAgent)
	for _, cookie := range jar.Cookies(request.URL) {
		request.AddCookie(cookie)
	}
	response, err := t.base.RoundTrip(request)
	if err == nil && response != nil {
		jar.SetCookies(request.URL, response.Cookies())
	}
	return response, err
}
