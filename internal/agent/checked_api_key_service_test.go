package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/stretchr/testify/require"
)

func TestCheckedAPIKeyServiceSaveAndInferenceHTTPS(t *testing.T) {
	root := t.TempDir()
	accountDir, dataDir, project := filepath.Join(root, "accounts"), filepath.Join(root, "data"), filepath.Join(root, "project")
	for _, dir := range []string{dataDir, project, filepath.Join(root, "config")} {
		require.NoError(t, os.MkdirAll(dir, 0o700))
	}
	for name, value := range map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountDir} {
		t.Setenv(name, value)
	}
	require.NoError(t, accounts.Save(t.Context(), "unrelated-fixture-namespace", accounts.Entry{ID: "unrelated", AccessToken: "synthetic-unrelated-token", Raw: json.RawMessage(`{"number":1.0}`)}))
	accountPath := filepath.Join(accountDir, "accounts.json")
	accountBefore, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	marker, counter := filepath.Join(root, "must-not-execute"), filepath.Join(root, "input-evaluations")
	literal := "synthetic-$(printf y > " + marker + ")-$CHECKED_SECRET"
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", counter, literal)
	type observedRequest struct{ method, path, authorization, extra string }
	var mu sync.Mutex
	var requests []observedRequest
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, observedRequest{r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Captured-Setting")})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/models":
			_, _ = fmt.Fprint(w, `{"data":[]}`)
		case "/v1/chat/completions":
			_, _ = fmt.Fprint(w, `{"id":"checked-fixture","object":"chat.completion","model":"main","choices":[{"message":{"role":"assistant","content":"checked key accepted"},"finish_reason":"stop"}]}`)
		default:
			http.Error(w, "unexpected fixture path", http.StatusNotFound)
		}
	}))
	t.Cleanup(host.Close)
	oldClient, oldTransport := http.DefaultClient, http.DefaultTransport
	http.DefaultClient, http.DefaultTransport = host.Client(), host.Client().Transport
	t.Cleanup(func() { http.DefaultClient, http.DefaultTransport = oldClient, oldTransport })
	document := map[string]any{
		"options": map[string]any{"disable_default_providers": true},
		"providers": map[string]any{"fixture": map[string]any{
			"type": "openai-compat", "base_url": host.URL + "/v1", "api_key": "synthetic-prior-key",
			"extra_headers": map[string]string{"X-Captured-Setting": "preserved"},
			"models":        []map[string]any{{"id": "main", "default_max_tokens": 100}, {"id": "small", "default_max_tokens": 50}},
		}},
		"models": map[string]any{"large": map[string]any{"provider": "fixture", "model": "main"}, "small": map[string]any{"provider": "fixture", "model": "small"}},
	}
	data, err := json.Marshal(document)
	require.NoError(t, err)
	configPath := filepath.Join(dataDir, "crux.json")
	require.NoError(t, os.WriteFile(configPath, data, 0o600))
	base := env.NewFromMap(map[string]string{
		"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": accountDir,
		"CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": dataDir,
		"CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(config.ProviderProfileIntegrated),
		"CHECKED_SECRET": "must-not-substitute",
	})
	store, err := config.LoadIsolated(project, filepath.Join(root, "workspace-data"), false, base)
	require.NoError(t, err)
	service := providerauth.New(store, "checked-key-service-fixture")
	status, err := service.Status(t.Context())
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("fixture")
	require.True(t, ok)
	models := store.RuntimeSnapshot().AgentModelState()
	checkRequest := providerauth.APIKeyCheckRequest{
		CheckID: strings.Repeat("a", 32), CredentialID: "provider.api_key", Source: source,
		Target: providerauth.Target{WorkspaceID: status.WorkspaceID, Owner: providerauth.PublicOwner(owner), Generation: status.Generation},
	}
	assertEvaluations := func(phase string) {
		t.Helper()
		value, readErr := os.ReadFile(counter)
		require.NoError(t, readErr, phase)
		require.Equal(t, "x", string(value), phase)
	}
	checked, err := service.CheckAPIKey(t.Context(), checkRequest)
	require.NoError(t, err)
	require.NoError(t, checked.Validate())
	require.NotNil(t, checked.CheckedTarget)
	require.Equal(t, config.ConnectionProbeHTTPResponse, checked.Probe.Kind)
	require.Equal(t, http.StatusOK, checked.Probe.HTTPStatus)
	require.True(t, checked.Probe.EnteredKeyInAuthorization)
	require.False(t, checked.Probe.AuthorizationOverridden)
	assertEvaluations("after first check")
	replayedCheck, err := service.CheckAPIKey(t.Context(), checkRequest)
	require.NoError(t, err)
	require.Equal(t, checked, replayedCheck)
	assertEvaluations("after check replay")
	wrongInput := checkRequest
	wrongInput.Source = "different-unchecked-key"
	_, err = service.CheckAPIKey(t.Context(), wrongInput)
	require.ErrorIs(t, err, providerauth.ErrOperationConflict)
	assertEvaluations("after conflicting check")
	saveRequest := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("b", 32), CheckID: checkRequest.CheckID, Target: *checked.CheckedTarget}
	saved, err := service.SaveAPIKey(t.Context(), saveRequest)
	require.NoError(t, err)
	require.NoError(t, saved.Outcome.ValidateAPIKeySave(saveRequest))
	require.Equal(t, providerauth.MutationProgress{ConfigSaved: true, RuntimePublished: true}, saved.Outcome.Progress)
	require.Equal(t, models, store.RuntimeSnapshot().AgentModelState())
	assertEvaluations("after save")
	snapshot, current := saved.RuntimeSnapshot()
	require.True(t, current)
	provider, ok := snapshot.Config().Providers.Get("fixture")
	require.True(t, ok)
	require.Equal(t, literal, provider.APIKey)
	require.Equal(t, source, provider.APIKeyTemplate)
	data, err = os.ReadFile(configPath)
	require.NoError(t, err)
	var persisted struct {
		Providers map[string]struct {
			APIKey string `json:"api_key"`
		} `json:"providers"`
	}
	require.NoError(t, json.Unmarshal(data, &persisted))
	require.Equal(t, source, persisted.Providers["fixture"].APIKey)
	coord := &coordinator{cfg: store}
	call := func(snapshot config.RuntimeSnapshot) {
		large, small, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, snapshot)
		require.NoError(t, err)
		require.Equal(t, "small", small.ModelCfg.Model)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		result, err := large.Model.Generate(ctx, fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("check retained credential")}})
		require.NoError(t, err)
		require.NotEmpty(t, result.Content)
	}
	call(snapshot)
	assertEvaluations("after first inference")
	replayedSave, err := service.SaveAPIKey(t.Context(), saveRequest)
	require.NoError(t, err)
	require.Equal(t, saved.Outcome, replayedSave.Outcome)
	assertEvaluations("after save replay")
	require.NoError(t, store.SetConfigField(config.ScopeGlobal, "options.disable_auto_summarize", true))
	assertEvaluations("after unrelated mutation")
	call(store.RuntimeSnapshot())
	assertEvaluations("after second inference")
	historicalSave, err := service.SaveAPIKey(t.Context(), saveRequest)
	require.NoError(t, err)
	require.True(t, historicalSave.Outcome.Superseded)
	require.Equal(t, checkRequest.CheckID, historicalSave.Outcome.CheckID)
	accountAfter, err := os.ReadFile(accountPath)
	require.NoError(t, err)
	require.Equal(t, accountBefore, accountAfter)
	evaluations, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(evaluations), "check replay, save, inference and unrelated COW never reevaluate the checked source")
	require.NoFileExists(t, marker)
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []observedRequest{
		{http.MethodGet, "/v1/models", "Bearer " + literal, "preserved"},
		{http.MethodPost, "/v1/chat/completions", "Bearer " + literal, "preserved"},
		{http.MethodPost, "/v1/chat/completions", "Bearer " + literal, "preserved"},
	}, requests)
}
