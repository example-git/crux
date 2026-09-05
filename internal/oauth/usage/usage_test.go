package usage

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
)

type usageRoundTripFunc func(*http.Request) (*http.Response, error)

func (roundTrip usageRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return roundTrip(request)
}

func TestUsageRequestsRejectOwnerReplacementBeforeDispatch(t *testing.T) {
	originalClientTransport := http.DefaultClient.Transport
	originalTransport := http.DefaultTransport
	var dispatched atomic.Int64
	transport := usageRoundTripFunc(func(*http.Request) (*http.Response, error) {
		dispatched.Add(1)
		return nil, errors.New("unexpected dispatch")
	})
	http.DefaultClient.Transport = transport
	http.DefaultTransport = transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = originalClientTransport
		http.DefaultTransport = originalTransport
	})
	ctx := providertransport.ContextWithOwnerValidator(t.Context(), func() error {
		return errors.New("owner changed")
	})

	err := getJSON(ctx, http.MethodGet, "https://provider.example.invalid/usage", "token", nil, nil, &map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("getJSON() error = %v", err)
	}
	operation, err := compileManifestUsageOperation(&providertransport.Operation{
		ID:       "quota",
		Endpoint: manifest.Endpoint{BaseURL: "https://provider.example.invalid", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"provider.example.invalid"}},
		Method:   http.MethodGet,
		ClientIdentity: &manifest.ResolvedClientIdentity{
			CacheKey: "owner-bound-usage", LatestURL: "https://provider.example.invalid/version",
			FallbackVersion: "1.0.0", VersionPattern: `^\d+\.\d+\.\d+$`, UserAgentFormat: "provider/{version}",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = operation.execute(ctx, "token", providertransport.TemplateValues{})
	if err == nil || !strings.Contains(err.Error(), "owner changed") {
		t.Fatalf("execute() error = %v", err)
	}
	if dispatched.Load() != 0 {
		t.Fatalf("dispatched = %d", dispatched.Load())
	}
}

func TestManifestFetcherUsesWrappedDefaultTransport(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte(`{"used":25}`))
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	called := false
	http.DefaultTransport = usageRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		called = true
		return original.RoundTrip(request)
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	fetch, err := ManifestFetcher(map[string]*providertransport.Operation{"quota": {
		ID: "quota", Endpoint: manifest.Endpoint{BaseURL: server.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}},
		Method: http.MethodGet, ConnectTimeout: time.Second,
	}}, manifest.UsagePolicy{
		Operation: "quota", Source: "operation", Fallback: "unavailable",
		Windows: []manifest.WindowMap{{ID: "usage", UsedPointer: "/used"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetch(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if !called || len(result.Windows) != 1 || result.Windows[0].Percent != 25 {
		t.Fatalf("called=%v result=%+v", called, result)
	}
}

func TestFetchForOwnerRejectsReplacementAfterNetworkSideEffect(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	requireNoError := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	requireNoError(accounts.Save(t.Context(), "usage-owner", accounts.Entry{ID: "account", AccessToken: "token"}))
	active := true
	called := false
	validate := func() error {
		if !active {
			return errors.New("owner changed")
		}
		return nil
	}
	fetcher := func(context.Context, string) (*Usage, error) {
		called = true
		active = false
		return &Usage{Plan: "stale"}, nil
	}

	result, err := FetchForOwner(t.Context(), "provider", "usage-owner", fetcher, nil, validate)
	if err == nil || !called || result != nil {
		t.Fatalf("result=%v called=%v err=%v", result, called, err)
	}
}

func TestFetchWithTokenForOwnerRejectsReplacementAfterNetworkSideEffect(t *testing.T) {
	active := true
	validate := func() error {
		if !active {
			return errors.New("credential changed")
		}
		return nil
	}
	fetcher := func(_ context.Context, token string) (*Usage, error) {
		if token != "configuration-token" {
			t.Fatalf("token = %q", token)
		}
		active = false
		return &Usage{Plan: "stale"}, nil
	}

	result, err := FetchWithTokenForOwner(t.Context(), "provider", "configuration-token", fetcher, validate)
	if err == nil || result != nil {
		t.Fatalf("result=%v err=%v", result, err)
	}
}

func TestManifestFetcherParsesPercentageWindows(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("provider-beta"); got != "usage-v1" {
			t.Errorf("provider-beta = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer tok" {
			t.Errorf("auth = %q", got)
		}
		if got := r.Header.Get("User-Agent"); got != "synthetic/1.2.3" {
			t.Errorf("user agent = %q", got)
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
		Headers: []manifest.HeaderRule{
			{
				Operation: "set", Name: "provider-beta",
				Value: &manifest.Template{Kind: "literal", Value: "usage-v1"},
			},
			{
				Operation: "set", Name: "User-Agent",
				Value: &manifest.Template{Kind: "context", Ref: "client.user_agent"},
			},
		},
		ClientIdentity: &manifest.ResolvedClientIdentity{
			CacheKey: "synthetic-usage", FallbackVersion: "1.2.3",
			VersionPattern: `^\d+\.\d+\.\d+$`, UserAgentFormat: "synthetic/{version}",
		},
	}
	fetch, err := ManifestFetcher(map[string]*providertransport.Operation{"quota": operation}, manifest.UsagePolicy{
		Operation: "quota", Source: "operation", Fallback: "unavailable",
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

func TestManifestFetcherResolvesDynamicOperationIdentity(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("SYNTHETIC_DYNAMIC_VERSION", "")
	releasePath := "/manifests/" + runtime.GOOS + "_" + runtime.GOARCH + ".json"
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case releasePath:
			_, _ = response.Write([]byte(`{"version":"9.8.7"}`))
		case "/usage":
			want := "dynamic/9.8.7 " + runtime.GOOS + "/" + runtime.GOARCH
			if got := request.Header.Get("User-Agent"); got != want {
				t.Errorf("user agent = %q, want %q", got, want)
			}
			_, _ = response.Write([]byte(`{"used":25}`))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	fetch, err := ManifestFetcher(map[string]*providertransport.Operation{"quota": {
		ID: "quota",
		Endpoint: manifest.Endpoint{
			BaseURL: server.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()},
		},
		Method: http.MethodGet, Path: "/usage", ConnectTimeout: time.Second,
		Headers: []manifest.HeaderRule{{
			Operation: "set", Name: "User-Agent", Value: &manifest.Template{Kind: "context", Ref: "client.user_agent"}, Protected: true,
		}},
		ClientIdentity: &manifest.ResolvedClientIdentity{
			Environment: "SYNTHETIC_DYNAMIC_VERSION",
			LatestURL:   server.URL + "/manifests/{os}_{arch}.json", VersionPointer: "/version",
			CacheKey: "synthetic-dynamic-usage", FallbackVersion: "1.0.0", VersionPattern: `^\d+\.\d+\.\d+$`,
			UserAgentFormat: "dynamic/{version} {os}/{arch}", ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024,
		},
	}}, manifest.UsagePolicy{
		Operation: "quota", Source: "operation", Fallback: "unavailable",
		Windows: []manifest.WindowMap{{ID: "usage", UsedPointer: "/used"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetch(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Windows) != 1 || result.Windows[0].Percent != 25 {
		t.Fatalf("result = %+v", result)
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
	fetch, err := ManifestFetcher(map[string]*providertransport.Operation{"quota": {
		Endpoint: manifest.Endpoint{BaseURL: srv.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}},
		Method:   http.MethodGet,
	}}, manifest.UsagePolicy{Operation: "quota", Source: "operation", Fallback: "unavailable", Windows: []manifest.WindowMap{
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

func TestFetchCopilotParsesPremiumRequests(t *testing.T) {
	t.Setenv("COPILOT_ADVERTISE_MODE", "vscode")
	t.Setenv("COPILOT_VSCODE_EXTENSION_VERSION", "1.2.3-test")
	t.Setenv("COPILOT_VSCODE_INTEGRATION_ID", "test-integration")
	t.Setenv("COPILOT_VSCODE_EDITOR_VERSION", "test-editor")
	t.Setenv("COPILOT_VSCODE_EDITOR_PLUGIN_VERSION", "test-plugin")
	for _, test := range []struct {
		name        string
		payload     string
		wantPlan    string
		wantPercent int
		wantReset   time.Time
	}{
		{
			name: "metered with overage",
			payload: `{
				"copilot_plan": "individual",
				"quota_reset_date_utc": "2026-09-01T00:00:00Z",
				"quota_snapshots": {"premium_interactions": {
					"entitlement": 300,
					"overage_count": 5,
					"percent_remaining": 75,
					"quota_remaining": 225
				}}
			}`,
			wantPlan:    "individual",
			wantPercent: 27,
			wantReset:   time.Date(2026, time.September, 1, 0, 0, 0, 0, time.UTC),
		},
		{
			name: "unlimited",
			payload: `{
				"copilot_plan": "business",
				"quota_snapshots": {"premium_interactions": {
					"unlimited": true,
					"timestamp_utc": "2026-09-02T00:00:00Z"
				}}
			}`,
			wantPlan:    "business",
			wantPercent: 0,
			wantReset:   time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %q", r.Method)
				}
				if got := r.Header.Get("Authorization"); got != "Bearer github-oauth-token" {
					t.Errorf("authorization = %q", got)
				}
				if got := r.Header.Get("Accept"); got != "application/json" {
					t.Errorf("accept = %q", got)
				}
				if got := r.Header.Get("Copilot-Integration-Id"); got != "test-integration" {
					t.Errorf("integration ID = %q", got)
				}
				if got := r.Header.Get("User-Agent"); got != "GitHubCopilotChat/1.2.3-test" {
					t.Errorf("user agent = %q", got)
				}
				if got := r.Header.Get("Editor-Version"); got != "test-editor" {
					t.Errorf("editor version = %q", got)
				}
				if got := r.Header.Get("Editor-Plugin-Version"); got != "test-plugin" {
					t.Errorf("editor plugin version = %q", got)
				}
				_, _ = w.Write([]byte(test.payload))
			}))
			defer srv.Close()
			old := copilotUsageURL
			copilotUsageURL = srv.URL
			defer func() { copilotUsageURL = old }()

			u, err := FetchCopilot(t.Context(), "github-oauth-token")
			if err != nil {
				t.Fatal(err)
			}
			if u.Plan != test.wantPlan {
				t.Errorf("plan = %q, want %q", u.Plan, test.wantPlan)
			}
			if len(u.Windows) != 1 {
				t.Fatalf("windows = %+v", u.Windows)
			}
			if u.Windows[0].Name != "premium_requests" || u.Windows[0].Percent != test.wantPercent {
				t.Errorf("premium window = %+v", u.Windows[0])
			}
			if !u.Windows[0].ResetsAt.Equal(test.wantReset) {
				t.Errorf("reset = %v, want %v", u.Windows[0].ResetsAt, test.wantReset)
			}
		})
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

func TestManifestFetcherRunsSetupPipeline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer exact-token" {
			t.Errorf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.URL.Path {
		case "/setup":
			if got := providertransport.JSONPointer(body, "/metadata/client"); got != "synthetic-client" {
				t.Errorf("setup client = %v", got)
			}
			_, _ = w.Write([]byte(`{"project":{"id":"project-one"},"plans":{"preferred":{"name":"Premium"},"fallback":{"name":"Free"}}}`))
		case "/summary":
			if got := providertransport.JSONPointer(body, "/project"); got != "project-one" {
				t.Errorf("summary project = %v", got)
			}
			_, _ = w.Write([]byte(`{"groups":[{"buckets":[{"remaining_fraction":0.9,"reset_at":"2026-09-07T00:00:00Z"},{"remaining_fraction":0.25,"reset_at":"2026-08-31T10:00:00Z"}]}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := manifest.Endpoint{BaseURL: srv.URL, AllowedSchemes: []string{target.Scheme}, AllowedHosts: []string{target.Hostname()}}
	operations := map[string]*providertransport.Operation{
		"setup": {
			ID: "setup", Endpoint: endpoint, Method: http.MethodPost, Path: "/setup",
			RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{
				Operation: "set", Path: "/metadata/client", Value: &manifest.Template{Kind: "literal", Value: "synthetic-client"},
			}}},
		},
		"summary": {
			ID: "summary", Endpoint: endpoint, Method: http.MethodPost, Path: "/summary",
			RequestTransform: &manifest.JSONPipeline{MaxOperations: 1, Operations: []manifest.JSONOperation{{
				Operation: "set", Path: "/project", Value: &manifest.Template{Kind: "context", Ref: "usage.project"},
			}}},
		},
	}
	fetch, err := ManifestFetcher(operations, manifest.UsagePolicy{
		Setup: []manifest.UsageSetup{{
			Operation:    "setup",
			Extract:      []manifest.UsageContextExtraction{{Context: "usage.project", Pointer: "/project/id"}},
			PlanPointers: []string{"/plans/preferred/name", "/plans/fallback/name"},
		}},
		Operation: "summary", Source: "operation", Fallback: "unavailable",
		Windows: []manifest.WindowMap{
			{ID: "weekly", RemainingFractionPointer: "/groups/0/buckets/0/remaining_fraction", ResetPointer: "/groups/0/buckets/0/reset_at", ResetFormat: "rfc3339"},
			{ID: "short", RemainingFractionPointer: "/groups/0/buckets/1/remaining_fraction", ResetPointer: "/groups/0/buckets/1/reset_at", ResetFormat: "rfc3339"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetch(t.Context(), "exact-token")
	if err != nil {
		t.Fatal(err)
	}
	if result.Plan != "Premium" {
		t.Errorf("plan = %q", result.Plan)
	}
	if len(result.Windows) != 2 {
		t.Fatalf("windows = %+v", result.Windows)
	}
	if result.Windows[0].Name != "weekly" || result.Windows[0].Percent != 10 {
		t.Errorf("weekly window = %+v", result.Windows[0])
	}
	if result.Windows[1].Name != "short" || result.Windows[1].Percent != 75 {
		t.Errorf("short window = %+v", result.Windows[1])
	}
	if result.Windows[0].ResetsAt.IsZero() || result.Windows[1].ResetsAt.IsZero() {
		t.Errorf("reset timestamps = %+v", result.Windows)
	}
}
