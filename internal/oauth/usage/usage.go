// Package usage fetches and normalizes provider quota/rate-limit windows.
// Manifest-owned operations use the generic executor; temporary integrated
// adapters remain for providers that have not completed extraction.
package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/useragent"
)

// Window is one normalized usage window.
type Window struct {
	// Name is a short display label, e.g. "5h", "wk", "pro".
	Name string
	// Percent is utilization 0-100.
	Percent int
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time
}

// Usage is the normalized usage snapshot for one provider.
type Usage struct {
	ProviderID string
	Plan       string
	Windows    []Window
	FetchedAt  time.Time
}

// Fetcher is a core-owned provider quota adapter. Declarative plugins do not
// supply callbacks; generic operation-based usage is interpreted by core.
type Fetcher func(context.Context, string) (*Usage, error)

const maxBody = 1 << 20

// Fetch returns the current usage snapshot using the registered account
// namespace and core-owned fetcher. Missing credentials return no snapshot.
func Fetch(ctx context.Context, providerID, accountNamespace string, fetcher Fetcher) (*Usage, error) {
	if accountNamespace == "" || fetcher == nil {
		return nil, nil
	}
	token, err := accounts.AccessToken(ctx, accountNamespace)
	if err != nil || token == "" {
		return nil, err
	}
	u, err := fetcher(ctx, token)
	if err != nil || u == nil {
		return nil, err
	}
	u.ProviderID = providerID
	u.FetchedAt = time.Now()
	return u, nil
}

func getJSON(ctx context.Context, method, url, token string, body io.Reader, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("usage: %s returned HTTP %d", url, resp.StatusCode)
	}
	return json.Unmarshal(data, out)
}

// --- Codex ---

var codexUsageURL = "https://chatgpt.com/backend-api/wham/usage"

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"` // unix seconds
}

func FetchCodex(ctx context.Context, token string) (*Usage, error) {
	var payload struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	err := getJSON(ctx, http.MethodGet, codexUsageURL, token, nil, map[string]string{
		"Origin":     "https://chatgpt.com",
		"Referer":    "https://chatgpt.com/",
		"User-Agent": useragent.Codex(),
	}, &payload)
	if err != nil {
		return nil, err
	}
	u := &Usage{Plan: payload.PlanType}
	add := func(fallback string, w *codexWindow) {
		if w == nil || w.UsedPercent == nil {
			return
		}
		name := fallback
		if w.LimitWindowSeconds != nil {
			if label := codexWindowLabel(*w.LimitWindowSeconds); label != "" {
				name = label
			}
		}
		win := Window{Name: name, Percent: clampPct(*w.UsedPercent)}
		if w.ResetAt != nil && *w.ResetAt > 0 {
			win.ResetsAt = time.Unix(*w.ResetAt, 0)
		}
		u.Windows = append(u.Windows, win)
	}
	if payload.RateLimit != nil {
		add("usage", payload.RateLimit.Primary)
		add("secondary usage", payload.RateLimit.Secondary)
	}
	return u, nil
}

// codexWindowLabel labels a rolling window by its duration, matching the
// Codex CLI semantics: durations within ±5% of a known window get a short
// label; anything else returns "" so the caller falls back to a generic
// label.
func codexWindowLabel(limitWindowSeconds int64) string {
	if limitWindowSeconds <= 0 {
		return ""
	}
	// Window duration in minutes, rounded up like the Codex CLI does.
	minutes := (limitWindowSeconds + 59) / 60
	known := []struct {
		minutes int64
		label   string
	}{
		{5 * 60, "5h"},
		{24 * 60, "daily"},
		{7 * 24 * 60, "weekly"},
		{30 * 24 * 60, "monthly"},
		{365 * 24 * 60, "annual"},
	}
	for _, k := range known {
		lo := float64(k.minutes) * 0.95
		hi := float64(k.minutes) * 1.05
		if m := float64(minutes); m >= lo && m <= hi {
			return k.label
		}
	}
	return ""
}

// --- Gemini / Antigravity ---

var (
	geminiLoadCodeAssistURL = "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist"
	geminiUserQuotaURL      = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuota"
)

func FetchGemini(ctx context.Context, token string) (*Usage, error) {
	ua := map[string]string{"User-Agent": useragent.Gemini()}

	var assist struct {
		CurrentTier *struct {
			Name string `json:"name"`
		} `json:"currentTier"`
		PaidTier *struct {
			Name string `json:"name"`
		} `json:"paidTier"`
		Project string `json:"cloudaicompanionProject"`
	}
	if err := getJSON(ctx, http.MethodPost, geminiLoadCodeAssistURL, token, strings.NewReader("{}"), ua, &assist); err != nil {
		return nil, err
	}

	body, _ := json.Marshal(map[string]string{"project": assist.Project})
	var quota struct {
		Buckets []struct {
			RemainingAmount   string   `json:"remainingAmount"`
			RemainingFraction *float64 `json:"remainingFraction"`
			ResetTime         string   `json:"resetTime"`
			ModelID           string   `json:"modelId"`
		} `json:"buckets"`
	}
	if err := getJSON(ctx, http.MethodPost, geminiUserQuotaURL, token, strings.NewReader(string(body)), ua, &quota); err != nil {
		return nil, err
	}

	u := &Usage{}
	if assist.CurrentTier != nil {
		u.Plan = assist.CurrentTier.Name
	} else if assist.PaidTier != nil {
		u.Plan = assist.PaidTier.Name
	}

	// Merge buckets by window name keeping the highest utilization and the
	// earliest reset.
	merged := map[string]Window{}
	for _, b := range quota.Buckets {
		if b.ModelID == "" || b.RemainingFraction == nil {
			continue
		}
		used, limit := 0.0, 100.0
		if b.RemainingAmount != "" {
			remaining, err := strconv.ParseFloat(b.RemainingAmount, 64)
			if err == nil && *b.RemainingFraction > 0 {
				limit = math.Round(remaining / *b.RemainingFraction)
				used = limit - remaining
			} else {
				used = (1 - *b.RemainingFraction) * 100
			}
		} else {
			used = (1 - *b.RemainingFraction) * 100
		}
		pct := 0
		if limit > 0 {
			pct = clampPct(used / limit * 100)
		}
		win := Window{Name: geminiWindowName(b.ModelID), Percent: pct}
		if t, err := time.Parse(time.RFC3339, b.ResetTime); err == nil {
			win.ResetsAt = t
		}
		if prev, ok := merged[win.Name]; ok {
			if prev.Percent > win.Percent {
				win.Percent = prev.Percent
			}
			if !prev.ResetsAt.IsZero() && (win.ResetsAt.IsZero() || prev.ResetsAt.Before(win.ResetsAt)) {
				win.ResetsAt = prev.ResetsAt
			}
		}
		merged[win.Name] = win
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		u.Windows = append(u.Windows, merged[name])
	}
	return u, nil
}

func geminiWindowName(modelID string) string {
	id := strings.ToLower(modelID)
	switch {
	case strings.Contains(id, "flash-lite"):
		return "flash-lite"
	case strings.Contains(id, "flash"):
		return "flash"
	case strings.Contains(id, "pro"):
		return "pro"
	}
	return id
}

func clampPct(v float64) int {
	return min(100, max(0, int(math.Round(v))))
}
