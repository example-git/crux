package workspace

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/env"
	"github.com/example-git/crux/internal/proto"
	"github.com/example-git/crux/internal/providerauth"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWorkspaceNativeCheckedAPIKeyTLSAndInference(t *testing.T) {
	f := newClientAuthenticationFixture(t, false)
	root := t.TempDir()
	marker := filepath.Join(root, "must-not-run")
	literal := "native-$(touch '" + marker + "')-$NEVER_EXPAND"
	var checks, inferences atomic.Int32
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.Equal(t, "captured", r.Header.Get("X-Configured"))
		switch r.URL.Path {
		case "/declared/catalog":
			checks.Add(1)
			require.Equal(t, "GET", r.Method)
			require.Empty(t, r.Header.Get("Authorization"))
			require.Equal(t, literal, r.Header.Get("X-Catalog-Key"))
			_, _ = w.Write([]byte(`{"models":[]}`))
		case "/v1/responses":
			inferences.Add(1)
			require.Equal(t, "POST", r.Method)
			require.Equal(t, "Bearer "+literal, r.Header.Get("Authorization"))
			require.Equal(t, "example-reasoner", gjson.GetBytes(data, "model").String())
			if !gjson.GetBytes(data, "stream").Bool() {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"native-json","status":"completed","output":[{"id":"message","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"native key accepted","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`))
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"native\",\"status\":\"in_progress\",\"output\":[]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.content_part.added\ndata: {\"type\":\"response.content_part.added\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"part\":{\"type\":\"output_text\",\"text\":\"\"}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"message\",\"delta\":\"native key accepted\"}\n\n")
			_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"message\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"native key accepted\",\"annotations\":[]}]}}\n\n")
			_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"native\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
		default:
			t.Errorf("undeclared request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(host.Close)
	previous := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	bundle := filepath.Join(root, "native.plugin")
	require.NoError(t, os.CopyFS(bundle, os.DirFS("../../docs/provider-plugins/examples/responses-oauth.plugin")))
	data, err := os.ReadFile(filepath.Join(bundle, "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	value.Capabilities.OAuth = nil
	value.Compatibility.RequiredFeatures = append(value.Compatibility.RequiredFeatures, "operation.model-catalog-http")
	value.Capabilities.Credentials = []manifest.Credential{{ID: "key", Kind: "api-key", Audience: []string{"api"}}}
	target, err := url.Parse(host.URL)
	require.NoError(t, err)
	for i := range value.Capabilities.Endpoints {
		if value.Capabilities.Endpoints[i].ID == "api" {
			value.Capabilities.Endpoints[i] = manifest.Endpoint{ID: "api", BaseURL: host.URL, AllowedSchemes: []string{"https"}, AllowedHosts: []string{target.Hostname()}, Override: "same-origin", Credential: "key"}
		}
	}
	value.Capabilities.Operations = append(value.Capabilities.Operations, manifest.Operation{ID: "catalog", Kind: "model-catalog", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: "GET", Path: "/declared/catalog", Headers: []manifest.HeaderRule{{Operation: "set", Name: "X-Catalog-Key", Value: &manifest.Template{Kind: "credential", Ref: "key"}}}})
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(bundle, "manifest.json"), data, 0o600))
	values := map[string]string{"HOME": root, "USERPROFILE": root, "AI_CLI_DIR": filepath.Join(root, "accounts"), "CRUX_GLOBAL_CONFIG": filepath.Join(root, "config"), "CRUX_GLOBAL_DATA": filepath.Join(root, "data"), "CRUX_CACHE_DIR": filepath.Join(root, "cache"), "CRUX_PROVIDER_PROFILE": "plugin-native", "CRUX_PROVIDER_PLUGINS": "example-responses", "CRUX_DISABLE_AUTO_MEMORY": "true"}
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(values["CRUX_GLOBAL_DATA"], values["CRUX_CACHE_DIR"]))
	require.NoError(t, err)
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: bundle, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	manager.Close()
	require.NoError(t, os.MkdirAll(values["CRUX_GLOBAL_CONFIG"], 0o700))
	cfg := fmt.Sprintf(`{"providers":{"example-responses":{"plugin":{"id":"example.responses-oauth"},"base_url":%q,"extra_headers":{"X-Configured":"captured"},"configuration":{"oauth_client_id":"synthetic"}}},"models":{"large":{"provider":"example-responses","model":"example-reasoner","max_tokens":100},"small":{"provider":"example-responses","model":"example-small","max_tokens":30}},"options":{"notifications":"disabled","disable_auto_summarize":true},"tools":{"codebase_search":{"enabled":false}}}`, host.URL)
	require.NoError(t, os.WriteFile(filepath.Join(values["CRUX_GLOBAL_CONFIG"], "crux.json"), []byte(cfg), 0o600))
	path := filepath.Join(values["CRUX_GLOBAL_DATA"], "crux.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"providers":{"example-responses":{"api_key":"synthetic-old"}}}`), 0o600))
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, env.NewFromMap(values))
	require.NoError(t, err)
	proposal, err := store.CollectRemoteRuntime(t.Context(), 1)
	require.NoError(t, err)
	c := f.w.client
	c.SetLocalRuntimeStore(store)
	created, err := c.CreateWorkspace(t.Context(), proto.Workspace{Path: t.TempDir(), DataDir: t.TempDir(), AuthorityMode: "client", Runtime: &proposal})
	require.NoError(t, err)
	w := NewClientWorkspace(c, *created)
	t.Cleanup(w.Shutdown)
	require.NoError(t, w.InitCoderAgentNonInteractive(t.Context()))
	receiver, err := f.s.Backend().GetWorkspace(w.workspaceID())
	require.NoError(t, err)
	models := store.RuntimeSnapshot().AgentModelState()
	originalAccounts, err := os.ReadFile(f.accountsPath)
	require.NoError(t, err)
	hostConfig, err := os.ReadFile(f.path)
	require.NoError(t, err)
	owner, ok := store.RuntimeSnapshot().ProviderOwner("example-responses")
	require.True(t, ok)
	status, err := w.ProviderAuthentication(t.Context())
	require.NoError(t, err)
	counter := filepath.Join(root, "source-counter")
	source := fmt.Sprintf("$(printf x >> '%s'; printf '%%s' '%s')", counter, strings.ReplaceAll(literal, "'", "'\\''"))
	check := providerauth.APIKeyCheckRequest{CheckID: strings.Repeat("a", 32), Target: providerauth.Target{WorkspaceID: status.WorkspaceID, Generation: status.Generation, Owner: providerauth.PublicOwner(owner)}, CredentialID: "provider.api_key", Source: source}
	checked, err := w.CheckProviderAPIKey(t.Context(), check)
	require.NoError(t, err)
	require.NotNil(t, checked.CheckedTarget)
	require.Equal(t, config.ConnectionProbeResult{Kind: config.ConnectionProbeHTTPResponse, Policy: config.ConnectionProbePolicyManifestHTTP200, HTTPStatus: 200}, checked.Probe)
	require.Zero(t, inferences.Load(), "catalog check cannot substitute inference")
	baseline := f.puts.Load()
	request := providerauth.APIKeySaveRequest{OperationID: strings.Repeat("b", 32), CheckID: check.CheckID, Target: *checked.CheckedTarget}
	saved, err := w.SaveCheckedProviderAPIKey(t.Context(), request)
	require.NoError(t, err)
	require.NoError(t, saved.ValidateAPIKeySave(request))
	require.NotNil(t, saved.Change)
	replay, err := w.SaveCheckedProviderAPIKey(t.Context(), request)
	require.NoError(t, err)
	require.Equal(t, saved, replay)
	require.Equal(t, baseline+1, f.puts.Load())
	require.Equal(t, models, receiver.Cfg.RuntimeSnapshot().AgentModelState())
	result, err := receiver.CurrentAgentCoordinator().Model().Model.Generate(t.Context(), fantasy.Call{Headers: map[string]string{"x-session-id": "native-checked-session"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("native checked literal")}})
	require.NoError(t, err)
	require.NotNil(t, result)
	require.Equal(t, "native key accepted", result.Content[0].(fantasy.TextContent).Text)
	require.EqualValues(t, 1, checks.Load())
	stream, err := receiver.CurrentAgentCoordinator().Model().Model.Stream(t.Context(), fantasy.Call{Headers: map[string]string{"x-session-id": "native-checked-stream"}, Prompt: fantasy.Prompt{fantasy.NewUserMessage("native checked stream")}})
	require.NoError(t, err)
	var text strings.Builder
	for part := range stream {
		require.NoError(t, part.Error)
		if part.Type == fantasy.StreamPartTypeTextDelta {
			text.WriteString(part.Delta)
		}
	}
	require.Equal(t, "native key accepted", text.String())
	require.EqualValues(t, 2, inferences.Load())
	evaluated, err := os.ReadFile(counter)
	require.NoError(t, err)
	require.Equal(t, "x", string(evaluated))
	require.NoFileExists(t, marker)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, source, gjson.GetBytes(disk, "providers.example-responses.api_key").String())
	afterAccounts, err := os.ReadFile(f.accountsPath)
	require.NoError(t, err)
	require.Equal(t, originalAccounts, afterAccounts)
	afterHostConfig, err := os.ReadFile(f.path)
	require.NoError(t, err)
	require.Equal(t, hostConfig, afterHostConfig)
}
