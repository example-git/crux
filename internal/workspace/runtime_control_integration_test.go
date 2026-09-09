package workspace_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/client"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/connection"
	"github.com/example-git/crux/internal/oauth/accounts"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/server"
	"github.com/example-git/crux/internal/workspace"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestRuntimeControlsThroughTLS(t *testing.T) {
	for _, adapter := range []string{"native-responses", "generic-json"} {
		t.Run(adapter, func(t *testing.T) { testRuntimeControlsThroughTLS(t, adapter) })
	}
}

func testRuntimeControlsThroughTLS(t *testing.T, adapter string) {
	xdgIsolate(t)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_PROVIDER_PROFILE", "plugin-compat")
	t.Setenv("CRUX_DISABLE_AUTO_MEMORY", "true")
	serverConfig := filepath.Join(os.Getenv("CRUX_GLOBAL_CONFIG"), "crux.json")
	serverBytes := []byte(`{"options":{"analysis_effort":"high"}}`)
	require.NoError(t, os.WriteFile(serverConfig, serverBytes, 0600))
	serverCode, err := connection.EnsureServerIdentity(t.Context())
	require.NoError(t, err)
	identity, err := connection.NewClientIdentity("controls-client")
	require.NoError(t, err)
	require.NoError(t, connection.AuthorizeClient(t.Context(), "controls-client", identity.Certificate))
	tlsConfig, err := connection.ServerTLSConfig(t.Context())
	require.NoError(t, err)
	s := server.NewServer(nil, "tcp", "127.0.0.1:0")
	require.NoError(t, s.EnableNetworkAuth(t.Context()))
	var reject, loseAck atomic.Bool
	var publications atomic.Int32
	handler := s.Handler()
	remote := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/runtime") {
			publications.Add(1)
			if reject.Swap(false) {
				http.Error(w, "synthetic rejected control publication", http.StatusBadRequest)
				return
			}
			if loseAck.Swap(false) {
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, r)
				assert.Equal(t, http.StatusOK, recorder.Code)
				conn, _, err := w.(http.Hijacker).Hijack()
				if assert.NoError(t, err) {
					_ = conn.Close()
				}
				return
			}
		}
		handler.ServeHTTP(w, r)
	}))
	remote.TLS = tlsConfig
	remote.StartTLS()
	t.Cleanup(func() { remote.Close(); _ = s.Close() })

	var requestMu sync.Mutex
	var requests []string
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adapter == "native-responses" {
			assert.Equal(t, "/v1/responses", r.URL.Path)
			assert.Equal(t, "Bearer synthetic-controls-access", r.Header.Get("Authorization"))
		} else {
			assert.Equal(t, "/v1/generate", r.URL.Path)
			assert.Empty(t, r.Header.Get("Authorization"))
		}
		data, err := io.ReadAll(r.Body)
		if !assert.NoError(t, err) {
			return
		}
		requestMu.Lock()
		requests = append(requests, string(data))
		id := fmt.Sprintf("control_fixture_%d", len(requests))
		requestMu.Unlock()
		if adapter == "native-responses" {
			writeRefreshFixtureSSE(w, id)
		} else {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"text":"verified remote refresh"}`)
		}
	}))
	t.Cleanup(provider.Close)
	targetURL, err := url.Parse(provider.URL)
	require.NoError(t, err)
	previousTransport := http.DefaultTransport
	http.DefaultTransport = refreshFixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != targetURL.Scheme || r.URL.Host != targetURL.Host {
			return nil, fmt.Errorf("unexpected runtime-control destination %s", r.URL.Host)
		}
		return provider.Client().Transport.RoundTrip(r)
	})
	t.Cleanup(func() { http.DefaultTransport = previousTransport })
	clientHome, configDir, dataDir, cacheDir := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("HOME", clientHome)
	t.Setenv("USERPROFILE", clientHome)
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("CRUX_GLOBAL_CONFIG", configDir)
	t.Setenv("CRUX_GLOBAL_DATA", dataDir)
	t.Setenv("CRUX_CACHE_DIR", cacheDir)
	fixture := installRuntimeControlsFixture(t, adapter, provider.URL, dataDir, cacheDir)
	providerConfig := map[string]any{
		"plugin":           map[string]any{"id": fixture.ID, "version": fixture.Version},
		"provider_options": map[string]any{"vendor.mode": "provider"},
	}
	if adapter == "native-responses" {
		providerConfig["api_key"] = "synthetic-controls-access"
		providerConfig["configuration"] = map[string]any{"oauth_client_id": "synthetic-client"}
		require.NoError(t, accounts.Save(t.Context(), fixture.Provider.AccountNamespace, accounts.Entry{ID: "selected", AccessToken: "synthetic-controls-access", RefreshToken: "synthetic-controls-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}))
	}
	encoded, err := json.Marshal(map[string]any{
		"providers": map[string]any{fixture.Provider.ID: providerConfig},
		"models": map[string]any{
			"large": map[string]any{"provider": fixture.Provider.ID, "model": fixture.Provider.DefaultLargeModel},
			"small": map[string]any{"provider": fixture.Provider.ID, "model": fixture.Provider.DefaultSmallModel, "provider_options": map[string]any{"vendor.mode": "small"}},
		},
	})
	require.NoError(t, err)
	clientConfig := filepath.Join(configDir, "crux.json")
	require.NoError(t, os.WriteFile(clientConfig, encoded, 0600))
	local, err := config.Load(t.TempDir(), t.TempDir(), false)
	require.NoError(t, err)
	owner, ok := local.Config().ProviderOwner(fixture.Provider.ID)
	require.True(t, ok)
	proposal, err := local.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	c, err := client.NewAuthenticatedClient(t.TempDir(), connection.Connection{Address: "tcp://" + strings.TrimPrefix(remote.URL, "https://"), ServerCertificate: serverCode, Client: identity})
	require.NoError(t, err)
	c.SetLocalRuntimeStore(local)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := workspace.NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := s.Backend().GetWorkspace(created.ID)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	events, err := c.SubscribeEvents(ctx, created.ID)
	require.NoError(t, err)
	resolve := func(id string) config.RuntimeControlState {
		t.Helper()
		state, err := w.RuntimeControlState(ctx, config.ScopeGlobal, config.RuntimeControlTarget{Owner: owner, ControlID: id, Selection: config.RuntimeControlSelection{ModelType: config.SelectedModelTypeLarge, ModelID: fixture.Provider.DefaultLargeModel}})
		require.NoError(t, err)
		require.NotEmpty(t, state.Target.DescriptorDigest)
		return state
	}
	mode := resolve("vendor.mode").Target
	effort := resolve("reasoning_effort").Target
	if adapter == "native-responses" {
		inherited := resolve("vendor.model-default")
		require.True(t, inherited.RuntimeDependent)
		require.Equal(t, `"unused-model-fallback"`, string(inherited.Effective.Value))
		fromRequest := resolve("vendor.output-tokens")
		require.True(t, fromRequest.RuntimeDependent)
		require.False(t, fromRequest.Effective.Present)
	} else {
		require.True(t, resolve("vendor.label").RuntimeDependent)
	}
	smallEffort := effort
	smallEffort.Selection = config.RuntimeControlSelection{ModelType: config.SelectedModelTypeSmall, ModelID: fixture.Provider.DefaultSmallModel}
	set := func(target config.RuntimeControlTarget, raw string) config.RuntimeControlState {
		t.Helper()
		state, err := w.SetRuntimeControl(ctx, config.ScopeGlobal, target, json.RawMessage(raw))
		require.NoError(t, err)
		require.True(t, state.ScopedKnown)
		require.True(t, state.Scoped.Present)
		require.True(t, config.RuntimeControlJSONEqual(json.RawMessage(raw), state.Scoped.Value))
		require.True(t, config.RuntimeControlJSONEqual(json.RawMessage(raw), state.Effective.Value))
		return state
	}
	wantLabel := `""`
	run := func(phase, wantMode, wantEffort string, extras bool) {
		t.Helper()
		require.NoError(t, w.UpdateAgentModel(ctx, w.Config().AgentModelState()))
		session, err := c.CreateSession(ctx, created.ID, "Controls "+phase)
		require.NoError(t, err)
		marker := "runtime-control-" + phase
		require.NoError(t, c.SendMessageWithPermissionMode(ctx, created.ID, session.ID, marker, marker+": Return the fixture response.", proto.AgentPermissionDeny))
		awaitRefreshFixtureRun(t, ctx, events, w, marker)
		requestMu.Lock()
		defer requestMu.Unlock()
		var matching []string
		for _, request := range requests {
			if strings.Contains(request, marker) && gjson.Get(request, "model").String() == fixture.Provider.DefaultLargeModel {
				matching = append(matching, request)
			}
		}
		require.Len(t, matching, 1)
		require.Equal(t, wantMode, gjson.Get(matching[0], "vendor.mode").String())
		require.Equal(t, wantEffort, gjson.Get(matching[0], "reasoning.effort").String())
		if adapter == "native-responses" {
			require.Greater(t, gjson.Get(matching[0], "max_output_tokens").Int(), int64(0), "the SDK supplies a field even without a configured control fallback")
		}
		if extras {
			require.Equal(t, "false", gjson.Get(matching[0], "vendor.enabled").Raw)
			require.Equal(t, "0", gjson.Get(matching[0], "vendor.count").Raw)
			require.Equal(t, wantLabel, gjson.Get(matching[0], "vendor.label").Raw)
		}
	}
	assertSmall := func(phase, wantEffort string) {
		t.Helper()
		marker := "runtime-control-" + phase
		require.Eventually(t, func() bool {
			requestMu.Lock()
			defer requestMu.Unlock()
			for _, request := range requests {
				if strings.Contains(request, marker) && gjson.Get(request, "model").String() == fixture.Provider.DefaultSmallModel {
					return gjson.Get(request, "vendor.mode").String() == "small" && gjson.Get(request, "reasoning.effort").String() == wantEffort
				}
			}
			return false
		}, 5*time.Second, 10*time.Millisecond, "auxiliary request must preserve its own mode and effective reasoning value")
	}
	run("inherited-provider", "provider", "medium", false)
	set(mode, `"selected"`)
	run("model-selected", "selected", "medium", false)
	removed, err := w.RemoveRuntimeControl(ctx, config.ScopeGlobal, mode)
	require.NoError(t, err)
	require.True(t, removed.ScopedKnown)
	require.False(t, removed.Scoped.Present)
	require.Equal(t, `"provider"`, string(removed.Effective.Value))
	require.Equal(t, "provider", removed.Source.Kind)
	run("model-removed", "provider", "medium", false)
	set(resolve("vendor.enabled").Target, "false")
	set(resolve("vendor.count").Target, "0")
	set(resolve("vendor.label").Target, `""`)
	run("explicit-empty-primitives", "provider", "medium", true)
	require.NoError(t, w.SetConfigField(config.ScopeGlobal, "options.analysis_effort", "low"))
	before, err := os.ReadFile(clientConfig)
	require.NoError(t, err)
	beforeCalls := publications.Load()
	_, err = w.SetRuntimeControl(ctx, config.ScopeGlobal, effort, json.RawMessage(`"high"`))
	require.Error(t, err)
	after, err := os.ReadFile(clientConfig)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, beforeCalls, publications.Load())
	run("global-low", "provider", "low", true)
	assertSmall("global-low", "low")
	set(smallEffort, `"high"`)
	run("small-explicit-high", "provider", "low", true)
	assertSmall("small-explicit-high", "high")
	_, err = w.RemoveRuntimeControl(ctx, config.ScopeGlobal, smallEffort)
	require.NoError(t, err)
	run("small-inherited-low", "provider", "low", true)
	assertSmall("small-inherited-low", "low")
	require.NoError(t, w.RemoveConfigField(config.ScopeGlobal, "options.analysis_effort"))
	set(effort, `"high"`)
	run("model-high", "provider", "high", true)

	before, err = os.ReadFile(clientConfig)
	require.NoError(t, err)
	beforeCalls = publications.Load()
	accepted := receiver.Cfg.RemoteAuthority()
	badOwner, badModel, badDescriptor := mode, mode, mode
	badOwner.Owner.ManifestVersion = "different"
	badModel.Selection.ModelID = "different"
	badDescriptor.DescriptorDigest = strings.Repeat("0", 64)
	for _, invalid := range []config.RuntimeControlTarget{badOwner, badModel, badDescriptor} {
		_, err := w.SetRuntimeControl(ctx, config.ScopeGlobal, invalid, json.RawMessage(`"selected"`))
		require.Error(t, err)
	}
	_, err = w.SetRuntimeControl(ctx, config.ScopeGlobal, mode, json.RawMessage(`"invalid"`))
	require.Error(t, err)
	_, err = w.SetRuntimeControl(ctx, config.ScopeGlobal, resolve("vendor.count").Target, json.RawMessage("9007199254740993"))
	require.Error(t, err, "persistence must reject a number that the configuration loader cannot preserve")
	after, err = os.ReadFile(clientConfig)
	require.NoError(t, err)
	require.Equal(t, before, after)
	require.Equal(t, beforeCalls, publications.Load())
	require.Equal(t, accepted, receiver.Cfg.RemoteAuthority())
	reject.Store(true)
	_, err = w.SetRuntimeControl(ctx, config.ScopeGlobal, mode, json.RawMessage(`"selected"`))
	require.ErrorContains(t, err, "client state saved")
	require.Equal(t, accepted, receiver.Cfg.RemoteAuthority())
	beforeCalls = publications.Load()
	_, err = w.RuntimeControlState(ctx, config.ScopeGlobal, mode)
	require.ErrorContains(t, err, "not acknowledged")
	require.Equal(t, beforeCalls, publications.Load(), "resolve must not publish pending local state")
	run("rejected-publication", "provider", "high", true)
	loseAck.Store(true)
	set(mode, `"selected"`)
	run("lost-ack-reconciled", "selected", "high", true)
	if adapter == "generic-json" {
		inherited, err := w.RemoveRuntimeControl(ctx, config.ScopeGlobal, resolve("vendor.label").Target)
		require.NoError(t, err)
		require.True(t, inherited.RuntimeDependent)
		require.Equal(t, `"default"`, string(inherited.Effective.Value))
		wantLabel = `"transformed"`
		run("request-transform-inheritance", "selected", "high", true)
	}
	require.Equal(t, "small", local.Config().Models[config.SelectedModelTypeSmall].ProviderOptions["vendor.mode"])
	discovery, err := c.GetWorkspace(ctx, created.ID)
	require.NoError(t, err)
	public, err := json.Marshal(discovery)
	require.NoError(t, err)
	require.NotContains(t, string(public), "synthetic-controls")
	after, err = os.ReadFile(serverConfig)
	require.NoError(t, err)
	require.Equal(t, serverBytes, after)
}

func installRuntimeControlsFixture(t *testing.T, adapter, endpoint, dataDir, cacheDir string) manifest.Manifest {
	t.Helper()
	example := "minimal.plugin"
	if adapter == "native-responses" {
		example = "responses-oauth.plugin"
	}
	source := filepath.Join(t.TempDir(), "controls.plugin")
	require.NoError(t, os.MkdirAll(source, 0700))
	require.NoError(t, os.CopyFS(source, os.DirFS(filepath.Join("..", "..", "docs", "provider-plugins", "examples", example))))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	target, err := url.Parse(endpoint)
	require.NoError(t, err)
	for i := range value.Capabilities.Endpoints {
		item := &value.Capabilities.Endpoints[i]
		item.BaseURL = endpoint
		if item.ID != "api" {
			item.BaseURL += "/" + item.ID
		}
		item.AllowedHosts, item.AllowedSchemes = []string{target.Hostname()}, []string{target.Scheme}
	}
	if adapter == "generic-json" {
		small := value.Models[0]
		small.ID, small.Name = "echo-small", "Echo Small"
		value.Models = append(value.Models, small)
		value.Provider.DefaultSmallModel = small.ID
	}
	flagScope := "model"
	if adapter == "native-responses" {
		flagScope = "provider"
	}
	value.Capabilities.RuntimeControls = []manifest.RuntimeControl{
		{ID: "vendor.mode", Label: "Vendor mode", Type: "enum", Values: []string{"base", "provider", "selected", "small"}, Default: "base", Scope: "model", RequestPath: "/vendor/mode"},
		{ID: "reasoning_effort", Label: "Reasoning effort", Type: "enum", Values: []string{"low", "medium", "high"}, Default: "medium", Scope: "model", RequestPath: "/reasoning/effort"},
		{ID: "vendor.enabled", Label: "Enabled", Type: "boolean", Default: true, Scope: flagScope, RequestPath: "/vendor/enabled"},
		{ID: "vendor.count", Label: "Count", Type: "integer", Default: 1, Scope: "model", RequestPath: "/vendor/count"},
		{ID: "vendor.label", Label: "Label", Type: "string", Default: "default", Scope: "model", RequestPath: "/vendor/label"},
	}
	if adapter == "native-responses" {
		value.Capabilities.RuntimeControls = append(value.Capabilities.RuntimeControls, manifest.RuntimeControl{ID: "vendor.model-default", Label: "Model fallback", Type: "string", Default: "unused-model-fallback", Scope: "model", RequestPath: "/model"})
		value.Capabilities.RuntimeControls = append(value.Capabilities.RuntimeControls, manifest.RuntimeControl{ID: "vendor.output-tokens", Label: "Output token fallback", Type: "integer", Scope: "model", RequestPath: "/max_output_tokens"})
	} else {
		value.Capabilities.JSONTransforms = map[string]manifest.JSONPipeline{"label-transform": {MaxOperations: 1, Operations: []manifest.JSONOperation{{Operation: "set", Path: "/vendor/label", Value: &manifest.Template{Kind: "literal", Value: "transformed"}}}}}
		value.Capabilities.Operations[0].RequestTransform = "label-transform"
	}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0600))
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(dataDir, cacheDir))
	require.NoError(t, err)
	defer manager.Close()
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	return value
}
