package copilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/example-git/crux/internal/redact"
)

const (
	clientID              = "Iv1.b507a08c87ecfe98"
	codebaseIndexClientID = "01ab8ac9400c4e429b23"

	deviceCodeURL   = "https://github.com/login/device/code"
	accessTokenURL  = "https://github.com/login/oauth/access_token"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
)

var ErrNotAvailable = errors.New("github copilot not available")

type DeviceCode struct {
	expiresAt       time.Time
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// RequestDeviceCode initiates the device code flow with GitHub.
func RequestDeviceCode(ctx context.Context) (*DeviceCode, error) {
	return requestDeviceCode(ctx, clientID)
}

func RequestCodebaseIndexDeviceCode(ctx context.Context) (*DeviceCode, error) {
	return requestDeviceCode(ctx, codebaseIndexClientID)
}

func GitHubIdentity(ctx context.Context, accessToken string) (string, string, json.RawMessage) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", "", nil
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)
	client := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.CapturedOriginHTTPClient(&http.Client{Timeout: 30 * time.Second}, req.URL.String()))
	resp, err := client.Do(req)
	if err != nil {
		return "", "", nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", nil
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&user); err != nil || user.ID <= 0 || user.Login == "" {
		return "", "", nil
	}
	return fmt.Sprint(user.ID), user.Login, nil
}

func requestDeviceCode(ctx context.Context, clientID string) (*DeviceCode, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("scope", "read:user")

	req, err := http.NewRequestWithContext(ctx, "POST", deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	userAgent, err := useragent.CopilotGitHubUserAgentForContext(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	client := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.CapturedOriginHTTPClient(&http.Client{Timeout: 30 * time.Second}, req.URL.String()))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("device code request failed: %s - %s", resp.Status, string(body))
	}

	var dc DeviceCode
	if err := json.NewDecoder(resp.Body).Decode(&dc); err != nil {
		return nil, err
	}
	dc.expiresAt = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	return &dc, nil
}

// ExpiresAt is the exact deadline captured from the device-code response.
// Manually constructed legacy DeviceCode values have no captured deadline.
func (dc *DeviceCode) ExpiresAt() time.Time {
	if dc == nil {
		return time.Time{}
	}
	return dc.expiresAt
}

// PollForToken polls GitHub for the access token after user authorization.
func PollForToken(ctx context.Context, dc *DeviceCode) (*oauth.Token, error) {
	return pollForToken(ctx, dc, tryGetToken)
}

func PollForGitHubToken(ctx context.Context, dc *DeviceCode) (*oauth.Token, error) {
	return pollForToken(ctx, dc, func(ctx context.Context, deviceCode string) (*oauth.Token, error) {
		return tryGetGitHubTokenForClient(ctx, deviceCode, codebaseIndexClientID)
	})
}

func pollForToken(ctx context.Context, dc *DeviceCode, exchange func(context.Context, string) (*oauth.Token, error)) (*oauth.Token, error) {
	interval := max(dc.Interval, 5)
	deadline := dc.expiresAt
	if deadline.IsZero() {
		// Compatibility for callers that construct DeviceCode directly. Actual
		// RequestDeviceCode results always retain their original deadline.
		deadline = time.Now().Add(time.Duration(dc.ExpiresIn) * time.Second)
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(time.Duration(interval) * time.Second)
	defer ticker.Stop()

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}

		token, err := exchange(ctx, dc.DeviceCode)
		if err == errPending {
			continue
		}
		if err == errSlowDown {
			interval += 5
			ticker.Reset(time.Duration(interval) * time.Second)
			continue
		}
		if err != nil {
			return nil, err
		}
		return token, nil
	}

	return nil, fmt.Errorf("authorization timed out")
}

var (
	errPending  = fmt.Errorf("pending")
	errSlowDown = fmt.Errorf("slow_down")
)

func tryGetToken(ctx context.Context, deviceCode string) (*oauth.Token, error) {
	token, err := tryGetGitHubToken(ctx, deviceCode)
	if err != nil {
		return nil, err
	}
	return getCopilotToken(ctx, token.AccessToken)
}

func tryGetGitHubToken(ctx context.Context, deviceCode string) (*oauth.Token, error) {
	return tryGetGitHubTokenForClient(ctx, deviceCode, clientID)
}

func tryGetGitHubTokenForClient(ctx context.Context, deviceCode, clientID string) (*oauth.Token, error) {
	data := url.Values{}
	data.Set("client_id", clientID)
	data.Set("device_code", deviceCode)
	data.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

	req, err := http.NewRequestWithContext(ctx, "POST", accessTokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	userAgent, err := useragent.CopilotGitHubUserAgentForContext(ctx)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)

	client := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.CapturedOriginHTTPClient(&http.Client{Timeout: 30 * time.Second}, req.URL.String()))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var result struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}

	switch result.Error {
	case "":
		if result.AccessToken == "" {
			return nil, errPending
		}
		redact.Register(result.AccessToken)
		return &oauth.Token{AccessToken: result.AccessToken}, nil
	case "authorization_pending":
		return nil, errPending
	case "slow_down":
		return nil, errSlowDown
	default:
		return nil, fmt.Errorf("authorization failed: %s", result.Error)
	}
}

func getCopilotToken(ctx context.Context, githubToken string) (*oauth.Token, error) {
	redact.Register(githubToken)
	req, err := http.NewRequestWithContext(ctx, "GET", copilotTokenURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", githubToken))
	headers, err := HeadersForContext(ctx)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.CapturedOriginHTTPClient(&http.Client{Timeout: 30 * time.Second}, req.URL.String()))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrNotAvailable
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot token request failed: %s", resp.Status)
	}

	var result struct {
		Token     string `json:"token"`
		ExpiresAt int64  `json:"expires_at"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}

	copilotToken := &oauth.Token{
		AccessToken:  result.Token,
		RefreshToken: githubToken,
		ExpiresAt:    result.ExpiresAt,
	}
	copilotToken.SetExpiresIn()
	redact.Register(copilotToken.AccessToken, copilotToken.RefreshToken)

	return copilotToken, nil
}

// RefreshToken refreshes the Copilot token using the GitHub token.
func RefreshToken(ctx context.Context, githubToken string) (*oauth.Token, error) {
	return getCopilotToken(ctx, githubToken)
}
