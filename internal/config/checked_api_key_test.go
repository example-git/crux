package config

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

type checkedAPIKeyFixture struct {
	store      *ConfigStore
	owner      providerregistry.RegistrationOwner
	root, path string
}

func newCheckedAPIKeyFixture(t *testing.T, endpoint string, scope Scope) checkedAPIKeyFixture {
	t.Helper()
	root := t.TempDir()
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": string(ProviderProfileIntegrated), "CRUX_DISABLE_AUTO_MEMORY": "true", "SHOULD_NOT_EXPAND": "wrong"}
	t.Setenv("AI_CLI_DIR", values["AI_CLI_DIR"])
	for _, dir := range []string{"config", "data", "workspace-data"} {
		require.NoError(t, os.MkdirAll(filepath.Join(root, dir), 0700))
	}
	source := fmt.Sprintf(`{"providers":{"checked":{"type":"openai-compat","base_url":%q,"models":[{"id":"main"},{"id":"small"}],"extra_headers":{"X-Checked":"literal"}},"unrelated":{"type":"openai-compat","base_url":"https://example.invalid/v1","api_key":"synthetic-other","models":[{"id":"other"}]}},"models":{"large":{"provider":"checked","model":"main","max_tokens":123},"small":{"provider":"checked","model":"small","max_tokens":45}}}`, endpoint)
	require.NoError(t, os.WriteFile(filepath.Join(root, "config", "crux.json"), []byte(source), 0600))
	path := filepath.Join(root, "data", "crux.json")
	if scope == ScopeWorkspace {
		path = filepath.Join(root, "workspace-data", "crux.json")
	}
	require.NoError(t, os.WriteFile(path, []byte(`{"providers":{"checked":{"api_key":"synthetic-old"}},"unknown":{"number":9007199254740993}}`), 0600))
	store, err := LoadIsolated(root, filepath.Join(root, "workspace-data"), false, env.NewFromMap(values))
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("checked")
	require.True(t, ok)
	return checkedAPIKeyFixture{store, owner, root, path}
}
func (f checkedAPIKeyFixture) capture(t *testing.T) AuthenticationCapture {
	t.Helper()
	c, err := f.store.CaptureAuthentication(t.Context())
	require.NoError(t, err)
	return c
}
func checkedAPIKeyHTTP(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	host := httptest.NewTLSServer(handler)
	t.Cleanup(host.Close)
	prior := http.DefaultClient
	http.DefaultClient = host.Client()
	t.Cleanup(func() { http.DefaultClient = prior })
	return host
}

func TestCheckedAPIKeyCheckSaveLiteralAndEndpoint(t *testing.T) {
	for _, scope := range []Scope{ScopeGlobal, ScopeWorkspace} {
		t.Run(fmt.Sprint(scope), func(t *testing.T) {
			var requests atomic.Int32
			var headers []string
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				headers = append(headers, r.Header.Get("Authorization"))
				require.Equal(t, "literal", r.Header.Get("X-Checked"))
				_, _ = w.Write([]byte(`{"data":[]}`))
			})
			counterRoot := t.TempDir()
			endpointCounter := filepath.Join(counterRoot, "endpoint")
			keyCounter := filepath.Join(counterRoot, "key")
			marker := filepath.Join(counterRoot, "must-not-run")
			endpoint := fmt.Sprintf("$(if test -e '%s'; then printf 'https://changed.invalid'; else printf '%%s' '%s'; fi; printf x >> '%s')", endpointCounter, host.URL, endpointCounter)
			f := newCheckedAPIKeyFixture(t, endpoint, scope)
			require.NoError(t, os.Remove(endpointCounter)) // Exclude ordinary initial loader evaluation.
			literal := "synthetic-$(printf y > " + marker + ")-$SHOULD_NOT_EXPAND"
			source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", keyCounter, literal)
			require.NoError(t, accounts.Save(t.Context(), "unrelated-test", accounts.Entry{ID: "kept", AccessToken: "kept-token", Raw: json.RawMessage(`{"n":1.0}`)}))
			before := f.capture(t)
			oldModels, oldAgents := before.runtime.config.Models, before.runtime.config.Agents
			prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", source)
			require.NoError(t, err)
			require.Equal(t, ConnectionProbeHTTPResponse, prepared.ProbeResult().Kind)
			require.Equal(t, 200, prepared.ProbeResult().HTTPStatus)
			require.EqualValues(t, 1, requests.Load())
			var candidate *Config
			var commits, aborts int
			f.store.SetRuntimeGenerationPreparer(func(ctx context.Context, snapshot RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
				candidate = snapshot.Config()
				provider, _ := candidate.Providers.Get("checked")
				key, err := snapshot.ResolveProviderAPIKey(provider)
				require.NoError(t, err)
				require.Equal(t, literal, key)
				url, err := snapshot.ResolveProviderEndpoint(provider)
				require.NoError(t, err)
				require.Equal(t, host.URL, url)
				return RuntimeGenerationCandidate{Commit: func() { commits++ }, Abort: func() { aborts++ }}, nil
			})
			result, err := f.store.SaveCheckedAPIKey(t.Context(), scope, prepared)
			require.NoError(t, err)
			require.True(t, result.ConfigSaved)
			require.True(t, result.RuntimePublished)
			require.False(t, result.AccountsSaved)
			require.False(t, result.AccountRefreshed)
			require.True(t, before.accounts.SameObservation(result.After.accounts))
			require.Same(t, candidate, f.store.Config())
			require.Equal(t, 1, commits)
			require.Zero(t, aborts)
			require.Equal(t, oldModels, f.store.Config().Models)
			require.Equal(t, oldAgents, f.store.Config().Agents)
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			require.Equal(t, source, gjson.GetBytes(data, "providers.checked.api_key").String())
			require.Equal(t, "9007199254740993", gjson.GetBytes(data, "unknown.number").Raw)
			rawBase, err := os.ReadFile(filepath.Join(f.root, "config", "crux.json"))
			require.NoError(t, err)
			require.Equal(t, endpoint, gjson.GetBytes(rawBase, "providers.checked.base_url").String())
			proposal, err := f.store.CollectRemoteRuntimeForAuthentication(t.Context(), result.After, 1, nil)
			require.NoError(t, err)
			require.Equal(t, literal, proposal.Credentials[0].APIKey)
			require.Nil(t, proposal.Credentials[0].Account)
			require.Equal(t, host.URL, proposal.Providers[0].Config.BaseURL)
			// A subsequent production probe uses both retained literals, not either source.
			provider, _ := f.store.Config().Providers.Get("checked")
			require.NoError(t, provider.TestConnection(t.Context(), result.After.runtime.resolver, func() error { return nil }))
			require.Equal(t, []string{"Bearer " + literal, "Bearer " + literal}, headers)
			for _, path := range []string{keyCounter, endpointCounter} {
				data, err := os.ReadFile(path)
				require.NoError(t, err)
				require.Equal(t, "x", string(data))
			}
			require.NoFileExists(t, marker)
			_, err = f.store.SaveCheckedAPIKey(t.Context(), scope, prepared)
			require.Error(t, err)
			require.EqualValues(t, 2, requests.Load())
			fresh := f.capture(t)
			require.True(t, result.After.SameObservation(fresh))
		})
	}
}

func TestCheckedAPIKeyRejectsStaleInputWithoutWrites(t *testing.T) {
	for _, change := range []string{"same-account-save", "inactive-account-addition", "source-file", "publication", "scope-shadow", "oauth-inheritance"} {
		t.Run(change, func(t *testing.T) {
			host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
			f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
			entry := accounts.Entry{ID: "kept", AccessToken: "token"}
			require.NoError(t, accounts.Save(t.Context(), "test", entry))
			if change == "scope-shadow" || change == "oauth-inheritance" {
				data := `{"providers":{"checked":{"api_key":"higher"}}}`
				if change == "oauth-inheritance" {
					data = `{"providers":{"checked":{"oauth":{"access_token":"old-oauth"}}}}`
				}
				require.NoError(t, os.WriteFile(filepath.Join(f.root, "workspace-data", "crux.json"), []byte(data), 0600))
				require.NoError(t, f.store.ReloadFromDisk(t.Context()))
			}
			before := f.capture(t)
			prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", "synthetic-new")
			require.NoError(t, err)
			switch change {
			case "same-account-save":
				require.NoError(t, accounts.Save(t.Context(), "test", entry))
			case "inactive-account-addition":
				require.NoError(t, accounts.SaveWithoutActivating(t.Context(), "test", accounts.Entry{ID: "other", AccessToken: "other"}))
			case "source-file":
				file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
				require.NoError(t, err)
				_, err = file.WriteString("\n")
				require.NoError(t, err)
				require.NoError(t, file.Close())
			case "publication":
				require.NoError(t, f.store.SetCompactMode(ScopeGlobal, true))
			}
			data, err := os.ReadFile(f.path)
			require.NoError(t, err)
			result, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, prepared)
			require.Error(t, err)
			require.False(t, result.ConfigSaved)
			require.False(t, result.RuntimePublished)
			require.False(t, result.AccountsSaved)
			after, readErr := os.ReadFile(f.path)
			require.NoError(t, readErr)
			require.Equal(t, data, after)
		})
	}
}

func TestCheckedAPIKeyCancellationAndPrivateEvidence(t *testing.T) {
	var requests atomic.Int32
	var status atomic.Int32
	status.Store(401)
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { requests.Add(1); w.WriteHeader(int(status.Load())) })
	f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
	before := f.capture(t)
	failed, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", "synthetic-secret")
	require.Error(t, err)
	require.Equal(t, 401, failed.ProbeResult().HTTPStatus)
	_, err = f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, failed)
	require.Error(t, err)
	require.NoError(t, (CheckedAPIKeyPreparation{}).ProbeResult().Validate())
	for _, value := range []any{failed, CheckedAPIKeyPreparation{}} {
		_, err := json.Marshal(value)
		require.Error(t, err)
		for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d"} {
			require.NotContains(t, fmt.Sprintf(verb, value), "synthetic-secret")
		}
	}
	status.Store(200)
	valid, err := f.store.PrepareCheckedAPIKey(t.Context(), before, f.owner, "provider.api_key", "synthetic-valid")
	require.NoError(t, err)
	for _, method := range []string{"prepare", "save"} {
		t.Run(method, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			f.store.writeMu.Lock()
			done := make(chan error, 1)
			go func() {
				if method == "prepare" {
					_, err := f.store.PrepareCheckedAPIKey(ctx, before, f.owner, "provider.api_key", "secret")
					done <- err
				} else {
					copy := valid
					_, err := f.store.SaveCheckedAPIKey(ctx, ScopeGlobal, copy)
					done <- err
				}
			}()
			cancel()
			select {
			case err := <-done:
				require.Error(t, err)
			case <-time.After(time.Second):
				t.Fatal("canceled admission waited for lock")
			}
			f.store.writeMu.Unlock()
		})
	}
	require.EqualValues(t, 2, requests.Load())
}

func TestCheckedAPIKeyLatePublicationKeepsProgressWithoutAfter(t *testing.T) {
	host := checkedAPIKeyHTTP(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) })
	f := newCheckedAPIKeyFixture(t, host.URL, ScopeGlobal)
	prepared, err := f.store.PrepareCheckedAPIKey(t.Context(), f.capture(t), f.owner, "provider.api_key", "synthetic-new")
	require.NoError(t, err)
	f.store.SetRuntimeGenerationPreparer(func(context.Context, RuntimeSnapshot) (RuntimeGenerationCandidate, error) {
		return RuntimeGenerationCandidate{Commit: func() {
			file, err := os.OpenFile(f.path, os.O_APPEND|os.O_WRONLY, 0600)
			require.NoError(t, err)
			_, err = file.WriteString("\n")
			require.NoError(t, err)
			require.NoError(t, file.Close())
		}, Abort: func() {}}, nil
	})
	result, err := f.store.SaveCheckedAPIKey(t.Context(), ScopeGlobal, prepared)
	require.Error(t, err)
	require.True(t, result.ConfigSaved, "save error: %v", err)
	require.True(t, result.RuntimePublished, "save error: %v", err)
	_, ok := result.RuntimeSnapshot()
	require.False(t, ok)
	require.False(t, result.AccountsSaved)
}
