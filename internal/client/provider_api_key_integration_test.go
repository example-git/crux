package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/app"
	"github.com/example-git/crux/internal/backend"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	cruxlog "github.com/example-git/crux/internal/log"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/server"
	"github.com/stretchr/testify/require"
)

func TestProviderAPIKeyRegisteredRoutesCheckSaveAndReplay(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "AI_CLI_DIR"} {
		t.Setenv(key, root)
	}
	var probes atomic.Int32
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		require.Equal(t, "/configured/v1/models", r.URL.Path)
		require.Equal(t, "Bearer synthetic-$LITERAL-key", r.Header.Get("Authorization"))
		require.Equal(t, "captured", r.Header.Get("X-Configured"))
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer provider.Close()
	oldClient := http.DefaultClient
	http.DefaultClient = provider.Client()
	t.Cleanup(func() { http.DefaultClient = oldClient })
	global, project, settings := filepath.Join(root, "global"), filepath.Join(root, "project"), filepath.Join(root, "settings")
	for _, path := range []string{global, project, settings} {
		require.NoError(t, os.MkdirAll(path, 0o700))
	}
	document := map[string]any{"providers": map[string]any{"checked": map[string]any{"type": "openai-compat", "base_url": provider.URL + "/configured/v1", "api_key": "synthetic-old-key", "owner": map[string]any{"type": "custom", "construction": "openai-compat"}, "extra_headers": map[string]string{"X-Configured": "captured"}, "models": []map[string]any{{"id": "model", "name": "Model", "context_window": 8192, "default_max_tokens": 1024}}}}, "models": map[string]any{"large": map[string]any{"provider": "checked", "model": "model", "max_tokens": 1024, "provider_options": map[string]any{"zero": 0, "false": false, "empty": ""}}, "small": map[string]any{"provider": "checked", "model": "model", "max_tokens": 512}}}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	configPath := filepath.Join(global, "crux.json")
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace-data"), false, env.NewFromMap(map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": root, "CRUX_GLOBAL_CONFIG": settings, "CRUX_GLOBAL_DATA": global, "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileIntegrated)}))
	require.NoError(t, err)
	modelsBefore, err := json.Marshal(store.Config().Models)
	require.NoError(t, err)
	var commits atomic.Int32
	store.SetRuntimeGenerationPreparer(func(_ context.Context, snapshot config.RuntimeSnapshot) (config.RuntimeGenerationCandidate, error) {
		return config.RuntimeGenerationCandidate{Abort: func() {}, Commit: func() { commits.Add(1); require.Same(t, snapshot.Config(), store.Config()) }}, nil
	})
	appCtx, cancel := context.WithCancel(t.Context())
	a := app.NewForTest(appCtx)
	t.Cleanup(func() { cancel(); a.ShutdownForTest() })
	host := server.NewServer(store, "unix", "")
	ws := &backend.Workspace{ID: "checked-workspace", Path: project, App: a, Cfg: store}
	backend.InsertWorkspaceForTest(host.Backend(), ws)
	backend.SetWorkspaceShutdownFnForTest(ws, func() {})
	traceCtx, closeTrace, err := cruxlog.SetupTraffic(t.Context(), filepath.Join(root, "trace"), true)
	require.NoError(t, err)
	t.Cleanup(closeTrace)
	handler := host.Handler()
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r.WithContext(cruxlog.WithTrafficContext(r.Context(), traceCtx)))
	}))
	defer rpc.Close()
	sdk, err := NewClient(root, "tcp", strings.TrimPrefix(rpc.URL, "http://"))
	require.NoError(t, err)
	status, err := sdk.ProviderAuthentication(t.Context(), ws.ID)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("checked")
	require.True(t, ok)
	counter := filepath.Join(root, "input-evaluations")
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' 'synthetic-$LITERAL-key')", counter)
	request := providerauth.APIKeyCheckRequest{CheckID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: ws.ID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation}, CredentialID: "provider.api_key", Source: source}
	checked, err := sdk.CheckProviderAPIKey(traceCtx, ws.ID, request)
	require.NoError(t, err)
	require.NotNil(t, checked.Outcome.CheckedTarget)
	require.Equal(t, config.ConnectionProbeHTTPResponse, checked.Outcome.Probe.Kind)
	require.Equal(t, 200, checked.Outcome.Probe.HTTPStatus)
	checkReplay, err := sdk.CheckProviderAPIKey(t.Context(), ws.ID, request)
	require.NoError(t, err)
	require.Equal(t, checked, checkReplay)
	require.EqualValues(t, 1, probes.Load())
	save := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("b", 32), CheckID: request.CheckID, Target: *checked.Outcome.CheckedTarget}
	saved, err := sdk.SaveCheckedProviderAPIKey(t.Context(), ws.ID, save)
	require.NoError(t, err)
	require.NotNil(t, saved.Workspace)
	require.Equal(t, save.CheckID, saved.Outcome.CheckID)
	require.True(t, saved.Outcome.Progress.ConfigSaved)
	require.True(t, saved.Outcome.Progress.RuntimePublished)
	require.False(t, saved.Outcome.Progress.AccountsSaved)
	current, _ := store.Config().Providers.Get("checked")
	require.Equal(t, "synthetic-$LITERAL-key", current.APIKey)
	require.Equal(t, source, current.APIKeyTemplate)
	encoded, err := json.Marshal(saved)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "synthetic-")
	require.NotContains(t, string(encoded), source)
	require.NotContains(t, string(encoded), "account_namespace")
	written, err := os.ReadFile(configPath)
	require.NoError(t, err)
	var persisted config.Config
	require.NoError(t, json.Unmarshal(written, &persisted))
	diskProvider, _ := persisted.Providers.Get("checked")
	require.Equal(t, source, diskProvider.APIKey)
	replayed, err := sdk.SaveCheckedProviderAPIKey(t.Context(), ws.ID, save)
	require.NoError(t, err)
	require.Equal(t, saved, replayed)
	require.EqualValues(t, 1, commits.Load())
	require.EqualValues(t, 1, probes.Load())
	count, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(count))
	modelsAfter, err := json.Marshal(store.Config().Models)
	require.NoError(t, err)
	require.JSONEq(t, string(modelsBefore), string(modelsAfter))
	// Malformed secret input is rejected by the actual route and never logged.
	malformed, err := sdk.sendReq(traceCtx, http.MethodPost, "/workspaces/"+ws.ID+"/auth/api-key/check", nil, strings.NewReader(`{"source":"synthetic-malformed-secret","Source":"duplicate"}`), http.Header{"Content-Type": {"application/json"}})
	require.NoError(t, err)
	body, err := io.ReadAll(malformed.Body)
	require.NoError(t, err)
	require.NoError(t, malformed.Body.Close())
	require.Equal(t, http.StatusBadRequest, malformed.StatusCode)
	require.NotContains(t, string(body), "synthetic-malformed-secret")
	reader, err := cruxlog.OpenTrafficDatabaseReadOnly(traceCtx)
	require.NoError(t, err)
	defer reader.Close()
	require.Eventually(t, func() bool {
		var n int
		return reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_requests WHERE url LIKE '%/auth/api-key/check'`).Scan(&n) == nil && n >= 4
	}, 3*time.Second, 10*time.Millisecond)
	var payloads int
	require.NoError(t, reader.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM http_request_payloads p JOIN http_requests r ON p.request_id=r.id WHERE r.url LIKE '%/auth/api-key/check'`).Scan(&payloads))
	require.Zero(t, payloads)
	events, err := cruxlog.QueryTraffic(t.Context(), reader, cruxlog.TrafficQuery{Limit: 100, IncludeBody: true})
	require.NoError(t, err)
	for _, event := range events {
		require.NotContains(t, event.Body, source)
		require.NotContains(t, event.Body, "synthetic-malformed-secret")
	}
}
