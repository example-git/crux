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
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/oauth/copilot"
	"github.com/example-git/crux/internal/oauth/useragent"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

// Window is one normalized usage window.
type Window struct {
	// Name is a short display label, e.g. "5h", "wk", "pro".
	Name string `json:"name"`
	// Percent is utilization 0-100.
	Percent int `json:"percent"`
	// ResetsAt is when the window resets; zero when unknown.
	ResetsAt time.Time `json:"resets_at"`
}

// Usage is the normalized usage snapshot for one provider.
type Usage struct {
	ProviderID string    `json:"provider_id"`
	Plan       string    `json:"plan"`
	Windows    []Window  `json:"windows"`
	FetchedAt  time.Time `json:"fetched_at"`
}

// Request is a captured provider usage operation prepared without I/O.
type Request func(context.Context) (*Usage, error)

// Fetcher is a core-owned provider quota adapter. Declarative plugins do not
// supply callbacks; generic operation-based usage is interpreted by core.
type Fetcher func(context.Context, string) (*Usage, error)

const maxBody = 1 << 20

// Fetch returns the current usage snapshot using the registered account
// namespace and core-owned fetcher. Missing credentials return no snapshot.
func Fetch(ctx context.Context, providerID, accountNamespace string, fetcher Fetcher) (*Usage, error) {
	return FetchForOwner(ctx, providerID, accountNamespace, fetcher, nil, nil)
}

func FetchForOwner(ctx context.Context, providerID, accountNamespace string, fetcher Fetcher, refresher accounts.Refresher, validate accounts.Validator) (*Usage, error) {
	if accountNamespace == "" || fetcher == nil {
		return nil, nil
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	var token string
	var err error
	if validate == nil && refresher == nil {
		token, err = accounts.AccessToken(ctx, accountNamespace)
	} else {
		token, err = accounts.AccessTokenForOwner(ctx, accountNamespace, refresher, validate)
	}
	if err != nil || token == "" {
		return nil, err
	}
	return FetchWithTokenForOwner(ctx, providerID, token, fetcher, validate)
}

func FetchWithTokenForOwner(ctx context.Context, providerID, token string, fetcher Fetcher, validate accounts.Validator) (*Usage, error) {
	if token == "" || fetcher == nil {
		return nil, nil
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
	}
	if validate != nil {
		ctx = providertransport.ContextWithOwnerValidator(ctx, providertransport.OwnerValidator(validate))
	}
	u, err := fetcher(ctx, token)
	if err != nil || u == nil {
		return nil, err
	}
	if validate != nil {
		if err := validate(); err != nil {
			return nil, err
		}
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
	resp, err := providertransport.ClientWithContextOwnerValidator(ctx, providertransport.CapturedOriginHTTPClient(http.DefaultClient, req.URL.String())).Do(req)
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

type codexWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"` // unix seconds
}

// CodexFetcher preserves Codex window decoding while executing the declared
// usage operation, including its endpoint, headers and transport policy.
func CodexFetcher(operation *providertransport.Operation) (Fetcher, error) {
	if operation == nil {
		return nil, fmt.Errorf("Codex usage operation is required")
	}
	operation = operation.Clone()
	operation.Headers = append([]manifest.HeaderRule{{Name: "User-Agent", Operation: "set", Value: &manifest.Template{Kind: "context", Ref: "client.user_agent"}}}, operation.Headers...)
	compiled, err := compileManifestUsageOperation(operation)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context, token string) (*Usage, error) {
		userAgent, err := useragent.CodexRequestUserAgent(ctx)
		if err != nil {
			return nil, err
		}
		document, err := compiled.execute(ctx, token, providertransport.TemplateValues{Context: map[string]string{"client.user_agent": userAgent}})
		if err != nil {
			return nil, err
		}
		data, err := json.Marshal(document)
		if err != nil {
			return nil, err
		}
		return decodeCodexUsage(data)
	}, nil
}

func decodeCodexUsage(data []byte) (*Usage, error) {
	var payload struct {
		PlanType  string `json:"plan_type"`
		RateLimit *struct {
			Primary   *codexWindow `json:"primary_window"`
			Secondary *codexWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
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

var copilotUsageURL = "https://api.github.com/copilot_internal/user"

type copilotQuotaSnapshot struct {
	Entitlement      *float64 `json:"entitlement"`
	OverageCount     *float64 `json:"overage_count"`
	PercentRemaining *float64 `json:"percent_remaining"`
	QuotaRemaining   *float64 `json:"quota_remaining"`
	Remaining        *float64 `json:"remaining"`
	Unlimited        bool     `json:"unlimited"`
	TimestampUTC     string   `json:"timestamp_utc"`
}

func FetchCopilot(ctx context.Context, token string) (*Usage, error) {
	var payload struct {
		Plan              string `json:"copilot_plan"`
		QuotaResetDate    string `json:"quota_reset_date"`
		QuotaResetDateUTC string `json:"quota_reset_date_utc"`
		QuotaSnapshots    *struct {
			PremiumInteractions *copilotQuotaSnapshot `json:"premium_interactions"`
		} `json:"quota_snapshots"`
	}
	if err := getJSON(ctx, http.MethodGet, copilotUsageURL, token, nil, copilot.Headers(), &payload); err != nil {
		return nil, err
	}
	u := &Usage{Plan: payload.Plan}
	if payload.QuotaSnapshots == nil || payload.QuotaSnapshots.PremiumInteractions == nil {
		return u, nil
	}
	snapshot := payload.QuotaSnapshots.PremiumInteractions
	overage := 0.0
	if snapshot.OverageCount != nil {
		overage = *snapshot.OverageCount
	}
	remaining := snapshot.QuotaRemaining
	if remaining == nil {
		remaining = snapshot.Remaining
	}
	var percent *float64
	switch {
	case snapshot.Unlimited:
		value := 0.0
		percent = &value
	case snapshot.Entitlement != nil && *snapshot.Entitlement > 0:
		entitlement := *snapshot.Entitlement
		used := 0.0
		calculated := false
		switch {
		case snapshot.PercentRemaining != nil:
			used = math.Round(max(0, entitlement*(1-*snapshot.PercentRemaining/100)))
			calculated = true
		case remaining != nil:
			used = max(0, entitlement-*remaining)
			calculated = true
		case overage > 0:
			used = entitlement
			calculated = true
		}
		if calculated {
			value := max(0, used+overage) / entitlement * 100
			percent = &value
		}
	case remaining != nil && snapshot.PercentRemaining != nil && *snapshot.PercentRemaining > 0 && *snapshot.PercentRemaining <= 100:
		limit := math.Round(*remaining / (*snapshot.PercentRemaining / 100))
		if limit > 0 {
			value := max(0, limit-*remaining+overage) / limit * 100
			percent = &value
		}
	}
	if percent == nil {
		return u, nil
	}
	window := Window{Name: "premium_requests", Percent: clampPct(*percent)}
	resetAt := payload.QuotaResetDateUTC
	if resetAt == "" {
		resetAt = payload.QuotaResetDate
	}
	if resetAt == "" {
		resetAt = snapshot.TimestampUTC
	}
	window.ResetsAt, _ = time.Parse(time.RFC3339, resetAt)
	u.Windows = append(u.Windows, window)
	return u, nil
}

func clampPct(v float64) int {
	return min(100, max(0, int(math.Round(v))))
}
