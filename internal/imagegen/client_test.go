package imagegen

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/oauth"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
)

func TestDoJSONUsesAPIKeyAuthAndOpenAIBase(t *testing.T) {
	var gotAuth, gotContentType, gotAccountID, gotPath string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotContentType = r.Header.Get("Content-Type")
		gotAccountID = r.Header.Get("ChatGPT-Account-ID")
		gotPath = r.URL.Path
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	openAIBaseURLOverride = srv.URL
	defer func() { openAIBaseURLOverride = "" }()

	c := NewClient()
	resp, err := c.doJSON(context.Background(), resolvedAuth{mode: AuthAPIKey, token: "sk-test"}, "images/generations", GenerateRequest{
		Prompt: "a red circle",
		Model:  "gpt-image-1",
	})
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if resp.AuthMode != AuthAPIKey {
		t.Errorf("AuthMode = %v, want AuthAPIKey", resp.AuthMode)
	}
	if len(resp.Data) != 1 || resp.Data[0].B64JSON != "aGVsbG8=" {
		t.Errorf("unexpected response data: %+v", resp.Data)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer sk-test")
	}
	if gotAccountID != "" {
		t.Errorf("ChatGPT-Account-ID should be empty for API key auth, got %q", gotAccountID)
	}
	if gotContentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", gotContentType)
	}
	if gotPath != "/images/generations" {
		t.Errorf("path = %q, want /images/generations", gotPath)
	}
	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if decoded["prompt"] != "a red circle" || decoded["model"] != "gpt-image-1" {
		t.Errorf("unexpected request body: %+v", decoded)
	}
}

func TestDoJSONUsesCodexBearerAndAccountHeader(t *testing.T) {
	var gotAuth, gotAccountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccountID = r.Header.Get("ChatGPT-Account-ID")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	codexBaseURLOverride = srv.URL
	defer func() { codexBaseURLOverride = "" }()

	c := NewClient()
	resp, err := c.doJSON(context.Background(), resolvedAuth{mode: AuthCodex, token: "codex-token", accountID: "acct-123"}, "images/generations", GenerateRequest{
		Prompt: "a blue square",
		Model:  "gpt-image-2",
	})
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if resp.AuthMode != AuthCodex {
		t.Errorf("AuthMode = %v, want AuthCodex", resp.AuthMode)
	}
	if gotAuth != "Bearer codex-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer codex-token")
	}
	if gotAccountID != "acct-123" {
		t.Errorf("ChatGPT-Account-ID = %q, want %q", gotAccountID, "acct-123")
	}
}

func TestGenerateCodexFansOutRequestedCount(t *testing.T) {
	bodies := make(chan map[string]any, 3)
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodies <- body
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	codexBaseURLOverride = srv.URL
	defer func() { codexBaseURLOverride = "" }()

	var authResolutions atomic.Int64
	var ownerValidations atomic.Int64
	owner := providerregistry.RegistrationOwner{ProviderID: "codex-test"}
	client := NewClient()
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		authResolutions.Add(1)
		return resolvedAuth{
			mode: AuthCodex, token: "codex-token", accountID: "account", owner: owner,
			ownerValidator: func(providerregistry.RegistrationOwner) error {
				ownerValidations.Add(1)
				return nil
			},
		}, nil
	}
	type generateResult struct {
		response *Response
		err      error
	}
	completed := make(chan generateResult, 1)
	go func() {
		response, err := client.Generate(context.Background(), GenerateRequest{
			Prompt: "three paper foxes",
			N:      3,
		})
		completed <- generateResult{response: response, err: err}
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("Codex image requests were not dispatched concurrently")
		}
	}
	close(release)
	result := <-completed
	if result.err != nil {
		t.Fatalf("Generate: %v", result.err)
	}
	if got := authResolutions.Load(); got != 1 {
		t.Fatalf("credential resolutions = %d, want 1", got)
	}
	if got := ownerValidations.Load(); got != 3 {
		t.Fatalf("owner validations = %d, want 3", got)
	}
	if len(result.response.Data) != 3 {
		t.Fatalf("response images = %d, want 3", len(result.response.Data))
	}
	if result.response.Model != defaultCodexModel {
		t.Fatalf("model = %q, want %q", result.response.Model, defaultCodexModel)
	}
	for range 3 {
		body := <-bodies
		if _, exists := body["n"]; exists {
			t.Fatalf("Codex generation request included n: %+v", body)
		}
		if body["prompt"] != "three paper foxes" || body["model"] != defaultCodexModel {
			t.Fatalf("unexpected Codex generation body: %+v", body)
		}
	}
}

func TestDoCodexImageRequestsPreservesSuccessfulVariants(t *testing.T) {
	var calls atomic.Int64
	release := make(chan struct{})
	started := make(chan struct{}, 3)
	type result struct {
		response *Response
		err      error
	}
	completed := make(chan result, 1)
	go func() {
		response, err := doCodexImageRequests(t.Context(), 3, func(context.Context) (*Response, error) {
			call := calls.Add(1)
			started <- struct{}{}
			if call == 1 {
				return nil, errors.New("one request failed")
			}
			<-release
			return &Response{Data: []ImageData{{B64JSON: "aGVsbG8="}}, AuthMode: AuthCodex}, nil
		})
		completed <- result{response: response, err: err}
	}()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("Codex image requests did not all start after one failed")
		}
	}
	close(release)
	resultValue := <-completed
	if resultValue.err != nil {
		t.Fatalf("doCodexImageRequests: %v", resultValue.err)
	}
	if len(resultValue.response.Data) != 2 || len(resultValue.response.Failures) != 1 {
		t.Fatalf("response = %+v", resultValue.response)
	}
	seen := make(map[int]bool, 3)
	for _, image := range resultValue.response.Data {
		seen[image.Variant] = true
	}
	for _, failure := range resultValue.response.Failures {
		seen[failure.Variant] = true
		if !strings.Contains(failure.Error, "one request failed") {
			t.Fatalf("failure = %+v", failure)
		}
	}
	if !seen[1] || !seen[2] || !seen[3] {
		t.Fatalf("variant identities = %+v", seen)
	}
}

func TestEditCodexFansOutRequestedCount(t *testing.T) {
	bodies := make(chan map[string]any, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		bodies <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	codexBaseURLOverride = srv.URL
	defer func() { codexBaseURLOverride = "" }()

	var authResolutions atomic.Int64
	var ownerValidations atomic.Int64
	owner := providerregistry.RegistrationOwner{ProviderID: "codex-test"}
	client := NewClient()
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		authResolutions.Add(1)
		return resolvedAuth{
			mode: AuthCodex, token: "codex-token", accountID: "account", owner: owner,
			ownerValidator: func(providerregistry.RegistrationOwner) error {
				ownerValidations.Add(1)
				return nil
			},
		}, nil
	}
	response, err := client.Edit(context.Background(), EditRequest{
		Images: []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: []byte("image")}},
		Prompt: "three background variants",
		N:      3,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if got := authResolutions.Load(); got != 1 {
		t.Fatalf("credential resolutions = %d, want 1", got)
	}
	if got := ownerValidations.Load(); got != 3 {
		t.Fatalf("owner validations = %d, want 3", got)
	}
	if len(response.Data) != 3 {
		t.Fatalf("response images = %d, want 3", len(response.Data))
	}
	for range 3 {
		body := <-bodies
		if _, exists := body["n"]; exists {
			t.Fatalf("Codex edit request included n: %+v", body)
		}
		images, ok := body["images"].([]any)
		if !ok || len(images) != 1 {
			t.Fatalf("unexpected Codex edit images: %+v", body["images"])
		}
	}
}

func TestGenerateAPIKeyKeepsRequestedCountInOneRequest(t *testing.T) {
	var requests atomic.Int64
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="},{"b64_json":"aGVsbG8="},{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()

	client := NewClient()
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		return resolvedAuth{mode: AuthAPIKey, token: "api-key", baseURL: srv.URL}, nil
	}
	response, err := client.Generate(context.Background(), GenerateRequest{
		Prompt: "three paper foxes",
		N:      3,
	})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
	if body["n"] != float64(3) {
		t.Fatalf("request n = %#v, want 3", body["n"])
	}
	if len(response.Data) != 3 {
		t.Fatalf("response images = %d, want 3", len(response.Data))
	}
}

func TestGenerateAPIKeyReportsMissingVariantAsPartialSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"created":1,"data":[{"b64_json":"Zmlyc3Q="},{"b64_json":"c2Vjb25k"}]}`))
	}))
	defer server.Close()

	client := NewClient()
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		return resolvedAuth{mode: AuthAPIKey, token: "api-key", baseURL: server.URL}, nil
	}
	response, err := client.Generate(t.Context(), GenerateRequest{Prompt: "three variants", N: 3})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Data) != 2 || response.Data[0].Variant != 1 || response.Data[1].Variant != 2 {
		t.Fatalf("successful variants = %+v", response.Data)
	}
	if len(response.Failures) != 1 || response.Failures[0].Variant != 3 {
		t.Fatalf("failed variants = %+v", response.Failures)
	}
}

func TestEditUsesMultipartForAPIKeyAuth(t *testing.T) {
	var gotContentType string
	var gotFields map[string][]string
	var gotFileBytes []byte
	var gotFileField string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotContentType = r.Header.Get("Content-Type")
		mediaType, params, err := mime.ParseMediaType(gotContentType)
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Errorf("unexpected content type: %q (%v)", gotContentType, err)
			return
		}
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			t.Errorf("parse multipart form: %v", err)
			return
		}
		gotFields = r.MultipartForm.Value
		for field, files := range r.MultipartForm.File {
			gotFileField = field
			f, err := files[0].Open()
			if err != nil {
				t.Errorf("open uploaded file: %v", err)
				return
			}
			buf := make([]byte, files[0].Size)
			_, _ = f.Read(buf)
			gotFileBytes = buf
		}
		_ = params
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	openAIBaseURLOverride = srv.URL
	defer func() { openAIBaseURLOverride = "" }()

	c := NewClient()
	imgData := []byte("fake-png-bytes")
	resp, err := c.doEditMultipart(context.Background(), resolvedAuth{mode: AuthAPIKey, token: "sk-test"}, EditRequest{
		Prompt: "change the background",
		Model:  "gpt-image-1",
		N:      3,
		Images: []EditImage{{Filename: "input.png", MIMEType: "image/png", Data: imgData}},
	})
	if err != nil {
		t.Fatalf("doEditMultipart: %v", err)
	}
	if resp.AuthMode != AuthAPIKey {
		t.Errorf("AuthMode = %v, want AuthAPIKey", resp.AuthMode)
	}
	if gotFields["prompt"] == nil || gotFields["prompt"][0] != "change the background" {
		t.Errorf("unexpected prompt field: %+v", gotFields["prompt"])
	}
	if gotFields["model"] == nil || gotFields["model"][0] != "gpt-image-1" {
		t.Errorf("unexpected model field: %+v", gotFields["model"])
	}
	if gotFields["n"] == nil || gotFields["n"][0] != "3" {
		t.Errorf("unexpected n field: %+v", gotFields["n"])
	}
	if gotFileField != "image[]" {
		t.Errorf("file field = %q, want image[]", gotFileField)
	}
	if string(gotFileBytes) != string(imgData) {
		t.Errorf("uploaded file bytes mismatch: got %q want %q", gotFileBytes, imgData)
	}
}

func TestEditUsesInlineDataURLForCodexAuth(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(body)
		gotBody = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"created":1,"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	codexBaseURLOverride = srv.URL
	defer func() { codexBaseURLOverride = "" }()

	c := NewClient()
	imgData := []byte("fake-png-bytes")
	resp, err := c.doJSON(context.Background(), resolvedAuth{mode: AuthCodex, token: "codex-token"}, "images/edits", codexEditPayload{
		Prompt: "change the background",
		Model:  "gpt-image-2",
		Images: []imageURL{{ImageURL: dataURL("image/png", imgData)}},
	})
	if err != nil {
		t.Fatalf("doJSON: %v", err)
	}
	if resp.AuthMode != AuthCodex {
		t.Errorf("AuthMode = %v, want AuthCodex", resp.AuthMode)
	}
	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	images, ok := decoded["images"].([]any)
	if !ok || len(images) != 1 {
		t.Fatalf("unexpected images field: %+v", decoded["images"])
	}
	entry, ok := images[0].(map[string]any)
	if !ok {
		t.Fatalf("unexpected image entry: %+v", images[0])
	}
	wantPrefix := "data:image/png;base64,"
	got, _ := entry["image_url"].(string)
	if !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("image_url = %q, want prefix %q", got, wantPrefix)
	}
	decodedImg, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(got, wantPrefix))
	if err != nil {
		t.Fatalf("decode embedded image: %v", err)
	}
	if string(decodedImg) != string(imgData) {
		t.Errorf("embedded image bytes mismatch: got %q want %q", decodedImg, imgData)
	}
}

func TestDefaultModelForAuthMode(t *testing.T) {
	if got := defaultModelFor(AuthCodex); got != "gpt-image-2" {
		t.Errorf("defaultModelFor(AuthCodex) = %q, want gpt-image-2", got)
	}
	if got := defaultModelFor(AuthAPIKey); got != "gpt-image-1" {
		t.Errorf("defaultModelFor(AuthAPIKey) = %q, want gpt-image-1", got)
	}
}

func TestGenerateRefreshesExpiredCodexAccountStandalone(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "")
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "client-test")
	t.Setenv("CODEX_VERSION", "1.0.0")

	var workspaceRefreshes atomic.Int64
	workspaceRefresher := func(context.Context, string) (*oauth.Token, error) {
		workspaceRefreshes.Add(1)
		return &oauth.Token{AccessToken: "workspace-token"}, nil
	}
	accounts.PublishProviders([]accounts.ProviderRegistration{{
		ProviderID: accountProvider,
		Namespace:  accountProvider,
		Refresher:  workspaceRefresher,
	}})
	t.Cleanup(func() { accounts.PublishProviders(nil) })

	ctx := context.Background()
	err := accounts.Save(ctx, accountProvider, accounts.Entry{
		ID:           "user@example.com",
		DisplayName:  "user@example.com",
		AccessToken:  "expired-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
		Raw:          json.RawMessage(`{"account_id":"acct-refresh"}`),
	})
	if err != nil {
		t.Fatalf("save expired account: %v", err)
	}

	originalDefaultClient := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "auth.openai.com" || r.URL.Path != "/oauth/token" {
			t.Fatalf("unexpected refresh request URL: %s", r.URL)
		}
		return jsonHTTPResponse(http.StatusOK, `{"access_token":"fresh-token","refresh_token":"next-refresh","expires_in":3600}`), nil
	})}
	defer func() { http.DefaultClient = originalDefaultClient }()

	var authorization, accountID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		accountID = r.Header.Get("ChatGPT-Account-ID")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	codexBaseURLOverride = srv.URL
	defer func() { codexBaseURLOverride = "" }()

	resp, err := NewClient().Generate(ctx, GenerateRequest{Prompt: "test", N: 1})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.AuthMode != AuthCodex {
		t.Fatalf("AuthMode = %v, want AuthCodex", resp.AuthMode)
	}
	if authorization != "Bearer fresh-token" {
		t.Errorf("Authorization = %q, want fresh token", authorization)
	}
	if accountID != "acct-refresh" {
		t.Errorf("ChatGPT-Account-ID = %q, want acct-refresh", accountID)
	}
	if got := workspaceRefreshes.Load(); got != 0 {
		t.Fatalf("workspace refresher calls = %d, want 0", got)
	}
	namespace, refresher, ok := accounts.ProviderSnapshot(accountProvider)
	if !ok || namespace != accountProvider || refresher == nil {
		t.Fatalf("workspace refresher snapshot = (%q, %v, %t), want preserved", namespace, refresher, ok)
	}
	workspaceToken, err := refresher(ctx, "workspace-refresh")
	if err != nil {
		t.Fatalf("workspace refresher: %v", err)
	}
	if workspaceToken.AccessToken != "workspace-token" || workspaceRefreshes.Load() != 1 {
		t.Fatalf("workspace refresher changed: token=%q calls=%d", workspaceToken.AccessToken, workspaceRefreshes.Load())
	}
}

func TestStandaloneRefreshDoesNotJoinGlobalRefresher(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "")
	t.Setenv("CODEX_OAUTH_CLIENT_ID", "client-test")
	t.Setenv("CODEX_VERSION", "1.0.0")

	ctx := t.Context()
	if err := accounts.Save(ctx, accountProvider, accounts.Entry{
		ID:           "user@example.com",
		AccessToken:  "expired-token",
		RefreshToken: "refresh-token",
		ExpiresAt:    time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("save expired account: %v", err)
	}

	workspaceStarted := make(chan struct{})
	releaseWorkspace := make(chan struct{})
	var workspaceRefreshes atomic.Int64
	accounts.PublishProviders([]accounts.ProviderRegistration{{
		ProviderID: accountProvider,
		Namespace:  accountProvider,
		Refresher: func(context.Context, string) (*oauth.Token, error) {
			workspaceRefreshes.Add(1)
			close(workspaceStarted)
			<-releaseWorkspace
			return &oauth.Token{
				AccessToken:  "workspace-token",
				RefreshToken: "workspace-refresh-next",
				ExpiresAt:    time.Now().Add(time.Hour).Unix(),
			}, nil
		},
	}})
	t.Cleanup(func() { accounts.PublishProviders(nil) })

	originalDefaultClient := http.DefaultClient
	var coreRefreshes atomic.Int64
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "auth.openai.com" || r.URL.Path != "/oauth/token" {
			t.Fatalf("unexpected refresh request URL: %s", r.URL)
		}
		coreRefreshes.Add(1)
		return jsonHTTPResponse(http.StatusOK, `{"access_token":"fresh-token","refresh_token":"next-refresh","expires_in":3600}`), nil
	})}
	defer func() { http.DefaultClient = originalDefaultClient }()

	globalDone := make(chan error, 1)
	go func() {
		_, err := accounts.AccessToken(ctx, accountProvider)
		globalDone <- err
	}()
	select {
	case <-workspaceStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace refresh did not start")
	}

	type authResult struct {
		auth resolvedAuth
		err  error
	}
	authDone := make(chan authResult, 1)
	go func() {
		auth, err := resolveAuth(ctx)
		authDone <- authResult{auth: auth, err: err}
	}()

	var result authResult
	timedOut := false
	select {
	case result = <-authDone:
	case <-time.After(5 * time.Second):
		timedOut = true
	}
	close(releaseWorkspace)
	globalErr := <-globalDone
	if timedOut {
		t.Fatal("standalone authentication joined the workspace refresher")
	}
	if result.err != nil {
		t.Fatalf("resolveAuth: %v", result.err)
	}
	if globalErr != nil {
		t.Fatalf("global refresh: %v", globalErr)
	}
	if result.auth.mode != AuthCodex || result.auth.token != "fresh-token" {
		t.Fatalf("standalone auth = (%v, %q), want core Codex fresh token", result.auth.mode, result.auth.token)
	}
	if got := coreRefreshes.Load(); got != 1 {
		t.Fatalf("core refresher calls = %d, want 1", got)
	}
	if got := workspaceRefreshes.Load(); got != 1 {
		t.Fatalf("workspace refresher calls = %d, want only its initiating call", got)
	}
	namespace, refresher, ok := accounts.ProviderSnapshot(accountProvider)
	if !ok || namespace != accountProvider || refresher == nil {
		t.Fatalf("workspace refresher snapshot = (%q, %v, %t), want preserved", namespace, refresher, ok)
	}
}

func TestGenerateFallsBackToAPIKeyForUnusableCodexAccount(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "sk-fallback")
	ctx := context.Background()
	if err := accounts.Save(ctx, accountProvider, accounts.Entry{
		ID:          "expired",
		AccessToken: "expired-token",
		ExpiresAt:   time.Now().Add(-time.Hour).UnixMilli(),
	}); err != nil {
		t.Fatalf("save expired account: %v", err)
	}

	var authorization string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"b64_json":"aGVsbG8="}]}`))
	}))
	defer srv.Close()
	openAIBaseURLOverride = srv.URL
	defer func() { openAIBaseURLOverride = "" }()

	resp, err := NewClient().Generate(ctx, GenerateRequest{Prompt: "test", N: 1})
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if resp.AuthMode != AuthAPIKey || authorization != "Bearer sk-fallback" {
		t.Fatalf("fallback auth = (%v, %q), want API key", resp.AuthMode, authorization)
	}
}

func TestInvalidRequestsMakeNoHTTPRequest(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv(openAIAPIKeyEnv, "sk-test")
	var requests atomic.Int64
	client := &Client{HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return jsonHTTPResponse(http.StatusOK, `{"data":[]}`), nil
	})}}

	generateCases := []GenerateRequest{
		{Prompt: " ", N: 1},
		{Prompt: "test", N: 0},
		{Prompt: "test", N: 11},
		{Prompt: "test", N: 1, Quality: "ultra"},
		{Prompt: "test", N: 1, Background: "green"},
		{Prompt: "test", N: 1, Size: "1023x1024"},
		{Prompt: "test", N: 1, Size: "4096x4096"},
	}
	for _, req := range generateCases {
		if _, err := client.Generate(context.Background(), req); err == nil {
			t.Errorf("Generate(%+v) succeeded, want validation error", req)
		}
	}
	if _, err := client.Edit(context.Background(), EditRequest{Prompt: "test", N: 1}); err == nil {
		t.Error("Edit without images succeeded, want validation error")
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestImageOwnerRevalidatesBeforeRedirectedTransportRequest(t *testing.T) {
	var requests atomic.Int64
	var validations atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		if request.URL.Path == "/images/generations" {
			http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		t.Fatal("redirected image request reached transport after owner replacement")
	}))
	defer server.Close()
	owner := providerregistry.RegistrationOwner{ProviderID: "openai"}
	client := NewClient()
	client.authResolver = func(context.Context) (resolvedAuth, error) {
		return resolvedAuth{
			mode: AuthAPIKey, token: "api-key", baseURL: server.URL, owner: owner,
			ownerValidator: func(providerregistry.RegistrationOwner) error {
				if validations.Add(1) > 1 {
					return errors.New("active owner changed")
				}
				return nil
			},
		}, nil
	}

	_, err := client.Generate(t.Context(), GenerateRequest{Prompt: "redirected image", N: 1})
	if err == nil || !strings.Contains(err.Error(), "active owner changed") {
		t.Fatalf("Generate error = %v, want active owner change", err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("transport requests = %d, want 1", got)
	}
	if got := validations.Load(); got != 2 {
		t.Fatalf("owner validations = %d, want 2", got)
	}
}

func TestResponseLimitExactAndOverflow(t *testing.T) {
	body := []byte(`{"data":[]}`)
	for _, tc := range []struct {
		name    string
		body    []byte
		wantErr bool
	}{
		{name: "exact limit", body: body},
		{name: "one byte over", body: append(append([]byte(nil), body...), ' '), wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &Client{
				MaxResponseBytes: int64(len(body)),
				HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(string(tc.body))),
					}, nil
				})},
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://image.test", nil)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.send(req, resolvedAuth{mode: AuthAPIKey})
			if tc.wantErr {
				if !errors.Is(err, ErrResponseTooLarge) {
					t.Fatalf("error = %v, want ErrResponseTooLarge", err)
				}
			} else if err != nil {
				t.Fatalf("exact-limit response failed: %v", err)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func jsonHTTPResponse(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestAPIErrorReportsAuthSource(t *testing.T) {
	err := &APIError{AuthMode: AuthAPIKey, StatusCode: 401, Body: "unauthorized"}
	if !strings.Contains(err.Error(), "OpenAI API key") {
		t.Errorf("expected error to mention OpenAI API key, got: %v", err)
	}
	err = &APIError{AuthMode: AuthCodex, StatusCode: 401, Body: "unauthorized"}
	if !strings.Contains(err.Error(), "Codex account") {
		t.Errorf("expected error to mention Codex account, got: %v", err)
	}
}
