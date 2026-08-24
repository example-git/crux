package usage

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

func TestManifestFetcherParsesPercentageWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("provider-beta"); got != "usage-v1" {
			t.Errorf("provider-beta = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		_, _ = w.Write([]byte(`{
			"short": {"utilization": 42.4, "resets_at": "2026-08-20T10:00:00Z"},
			"long": {"utilization": 12, "resets_at": "2026-08-24T00:00:00Z"},
			"optional": null
		}`))
	}))
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	operation := &providertransport.Operation{
		Endpoint: manifest.Endpoint{
			BaseURL: srv.URL, AllowedSchemes: []string{target.Scheme},
			AllowedHosts: []string{target.Hostname()},
		},
		Method: http.MethodGet,
		Headers: []manifest.HeaderRule{{
			Operation: "set", Name: "provider-beta",
			Value: &manifest.Template{Kind: "literal", Value: "usage-v1"},
		}},
	}
	fetch, err := ManifestFetcher(operation, manifest.UsagePolicy{
		Source: "operation", Fallback: "unavailable",
		Windows: []manifest.WindowMap{
			{ID: "short", UsedPointer: "/short/utilization", ResetPointer: "/short/resets_at", ResetFormat: "rfc3339"},
			{ID: "long", UsedPointer: "/long/utilization", ResetPointer: "/long/resets_at", ResetFormat: "rfc3339"},
			{ID: "optional", UsedPointer: "/optional/utilization"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := fetch(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 2 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	if u.Windows[0].Name != "short" || u.Windows[0].Percent != 42 {
		t.Errorf("short window = %+v", u.Windows[0])
	}
	if u.Windows[1].Name != "long" || u.Windows[1].Percent != 12 {
		t.Errorf("long window = %+v", u.Windows[1])
	}
	if u.Windows[0].ResetsAt.IsZero() {
		t.Error("resets_at not parsed")
	}
}

func TestManifestFetcherParsesArithmeticAndResetFormats(t *testing.T) {
	now := time.Now()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"remaining": 25, "limit": 100,
			"over": 120,
			"unix": 1770000000,
			"millis": 1770000000123,
			"duration": 60
		}`))
	}))
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := ManifestFetcher(&providertransport.Operation{
		Endpoint: manifest.Endpoint{BaseURL: srv.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}},
		Method:   http.MethodGet,
	}, manifest.UsagePolicy{Source: "operation", Fallback: "unavailable", Windows: []manifest.WindowMap{
		{ID: "remaining", RemainingPointer: "/remaining", LimitPointer: "/limit", ResetPointer: "/unix", ResetFormat: "unix-seconds"},
		{ID: "over", UsedPointer: "/over", ResetPointer: "/millis", ResetFormat: "unix-milliseconds"},
		{ID: "duration", UsedPointer: "/remaining", ResetPointer: "/duration", ResetFormat: "duration-seconds"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetch(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 3 {
		t.Fatalf("windows = %+v", result.Windows)
	}
	if result.Windows[0].Percent != 75 || result.Windows[0].ResetsAt.Unix() != 1770000000 {
		t.Errorf("remaining window = %+v", result.Windows[0])
	}
	if result.Windows[1].Percent != 100 || result.Windows[1].ResetsAt.UnixMilli() != 1770000000123 {
		t.Errorf("over window = %+v", result.Windows[1])
	}
	if delta := result.Windows[2].ResetsAt.Sub(now); delta < 55*time.Second || delta > 65*time.Second {
		t.Errorf("duration reset delta = %v", delta)
	}
}

func TestFetchCodexParsesWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"plan_type": "plus",
			"rate_limit": {
				"primary_window": {"used_percent": 55.6, "limit_window_seconds": 18000, "reset_at": 1770000000},
				"secondary_window": {"used_percent": 10, "limit_window_seconds": 604800, "reset_at": 1770600000}
			}
		}`))
	}))
	defer srv.Close()
	old := codexUsageURL
	codexUsageURL = srv.URL
	defer func() { codexUsageURL = old }()

	u, err := FetchCodex(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "plus" {
		t.Errorf("plan = %q", u.Plan)
	}
	if len(u.Windows) != 2 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	if u.Windows[0].Name != "5h" || u.Windows[0].Percent != 56 {
		t.Errorf("primary window = %+v", u.Windows[0])
	}
	if u.Windows[1].Name != "weekly" || u.Windows[1].Percent != 10 {
		t.Errorf("secondary window = %+v", u.Windows[1])
	}
	if u.Windows[0].ResetsAt.Unix() != 1770000000 {
		t.Errorf("reset_at = %v", u.Windows[0].ResetsAt)
	}
}

func TestFetchCodexFallbackLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{
			"rate_limit": {
				"primary_window": {"used_percent": 5},
				"secondary_window": {"used_percent": 7, "limit_window_seconds": 12345}
			}
		}`))
	}))
	defer srv.Close()
	old := codexUsageURL
	codexUsageURL = srv.URL
	defer func() { codexUsageURL = old }()

	u, err := FetchCodex(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	if len(u.Windows) != 2 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	if u.Windows[0].Name != "usage" {
		t.Errorf("primary fallback = %q, want usage", u.Windows[0].Name)
	}
	if u.Windows[1].Name != "secondary usage" {
		t.Errorf("secondary fallback = %q, want secondary usage", u.Windows[1].Name)
	}
}

func TestCodexWindowLabel(t *testing.T) {
	tests := []struct {
		seconds int64
		want    string
	}{
		{18000, "5h"}, // exactly 5h
		{17200, "5h"}, // within 5%
		{86400, "daily"},
		{604800, "weekly"},
		{590000, "weekly"},   // within 5%
		{2592000, "monthly"}, // 30 days
		{31536000, "annual"}, // 365 days
		{12345, ""},          // no match
		{0, ""},
		{-1, ""},
	}
	for _, tt := range tests {
		if got := codexWindowLabel(tt.seconds); got != tt.want {
			t.Errorf("codexWindowLabel(%d) = %q, want %q", tt.seconds, got, tt.want)
		}
	}
}

func TestFetchGeminiParsesBuckets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/assist":
			_, _ = w.Write([]byte(`{"currentTier":{"name":"free"},"cloudaicompanionProject":"proj"}`))
		default:
			_, _ = w.Write([]byte(`{"buckets":[
				{"modelId":"gemini-3-pro","remainingAmount":"75","remainingFraction":0.75,"resetTime":"2026-08-21T00:00:00Z"},
				{"modelId":"gemini-3-pro-high","remainingAmount":"40","remainingFraction":0.40,"resetTime":"2026-08-20T12:00:00Z"},
				{"modelId":"gemini-3-flash","remainingFraction":0.9},
				{"modelId":"","remainingFraction":0.1}
			]}`))
		}
	}))
	defer srv.Close()
	oldAssist, oldQuota := geminiLoadCodeAssistURL, geminiUserQuotaURL
	geminiLoadCodeAssistURL = srv.URL + "/assist"
	geminiUserQuotaURL = srv.URL + "/quota"
	defer func() { geminiLoadCodeAssistURL, geminiUserQuotaURL = oldAssist, oldQuota }()

	u, err := FetchGemini(context.Background(), "ya29.tok")
	if err != nil {
		t.Fatal(err)
	}
	if u.Plan != "free" {
		t.Errorf("plan = %q", u.Plan)
	}
	byName := map[string]Window{}
	for _, w := range u.Windows {
		byName[w.Name] = w
	}
	if len(byName) != 2 {
		t.Fatalf("windows = %+v", u.Windows)
	}
	// pro buckets merged: max utilization (60%), earliest reset.
	pro := byName["pro"]
	if pro.Percent != 60 {
		t.Errorf("pro percent = %d, want 60", pro.Percent)
	}
	if pro.ResetsAt.UTC().Hour() != 12 {
		t.Errorf("pro reset = %v, want earliest (12:00)", pro.ResetsAt)
	}
	if flash := byName["flash"]; flash.Percent != 10 {
		t.Errorf("flash percent = %d, want 10", flash.Percent)
	}
}
