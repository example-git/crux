package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	openairesponsestransport "github.com/example-git/crux/internal/providertransport/openairesponses"
	"github.com/stretchr/testify/require"
)

func TestClientResponsesContinuationBelongsToAcceptedGeneration(t *testing.T) {
	var mu sync.Mutex
	var requests []map[string]any
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		mu.Lock()
		requests = append(requests, request)
		id := fmt.Sprintf("resp_%d", len(requests))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":%q,\"status\":\"in_progress\",\"output\":[]}}\n\n", id)
		_, _ = fmt.Fprint(w, "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"in_progress\",\"content\":[]}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"content_index\":0,\"item_id\":\"msg\",\"delta\":\"answer\"}\n\n")
		_, _ = fmt.Fprint(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"answer\",\"annotations\":[]}]}}\n\n")
		_, _ = fmt.Fprintf(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":%q,\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n", id)
	}))
	defer host.Close()
	previous := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = previous })
	proposal := clientResponsesProposal(t, host.URL)
	principal := strings.Repeat("a", 64)
	compile := func(proposal config.RemoteRuntimeProposal, principal string) *config.ConfigStore {
		root := t.TempDir()
		store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "data"), false, proposal, principal, env.NewFromMap(map[string]string{}))
		require.NoError(t, err)
		return store
	}
	store := compile(proposal, principal)
	coord := &coordinator{cfg: store, responsesContinuations: openairesponsestransport.NewContinuationStore()}
	build := func(snapshot config.RuntimeSnapshot) Model {
		model, _, err := coord.buildAgentModelsWithSnapshot(t.Context(), config.Agent{Model: config.SelectedModelTypeLarge}, false, snapshot)
		require.NoError(t, err)
		return model
	}
	consume := func(model Model, call fantasy.Call) {
		stream, err := model.Model.Stream(t.Context(), call)
		require.NoError(t, err)
		for part := range stream {
			require.NoError(t, part.Error)
		}
	}
	first := fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("one")}, Headers: map[string]string{"x-session-id": "same-session", "x-request-purpose": "conversation"}}
	followup := first
	followup.Prompt = append(append(fantasy.Prompt{}, first.Prompt...), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("two"))
	third := followup
	third.Prompt = append(append(fantasy.Prompt{}, followup.Prompt...), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("three"))
	fourth := third
	fourth.Prompt = append(append(fantasy.Prompt{}, third.Prompt...), fantasy.Message{Role: fantasy.MessageRoleAssistant, Content: []fantasy.MessagePart{fantasy.TextPart{Text: "answer"}}}, fantasy.NewUserMessage("four"))
	old := build(store.RuntimeSnapshot())
	consume(old, first)
	consume(build(store.RuntimeSnapshot()), followup)
	proposal.Revision = 2
	proposal.Credentials[0].Generation = 2
	sealClientResponsesProposal(t, &proposal)
	_, err := store.ReplaceRemoteRuntime(t.Context(), proposal, principal, 1)
	require.NoError(t, err)
	consume(build(store.RuntimeSnapshot()), third)
	consume(old, third)
	consume(build(compile(proposal, strings.Repeat("b", 64)).RuntimeSnapshot()), fourth)
	proposal.Providers[0].Config.ExtraHeaders = map[string]string{"X-Client-Generation": "changed"}
	sealClientResponsesProposal(t, &proposal)
	consume(build(compile(proposal, principal).RuntimeSnapshot()), fourth)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, requests, 6)
	require.NotContains(t, requests[0], "previous_response_id")
	require.Equal(t, "resp_1", requests[1]["previous_response_id"], "same generation should continue")
	require.NotContains(t, requests[2], "previous_response_id", "new accepted revision must not reuse old state")
	require.Len(t, requests[2]["input"], 6, "new generation replays the complete five-message history plus the declared system prefix")
	require.Equal(t, "resp_2", requests[3]["previous_response_id"], "captured old model keeps its own chain")
	require.Len(t, requests[3]["input"], 2, "captured continuation sends only the next user turn plus the declared prefix")
	require.NotContains(t, requests[4], "previous_response_id", "another principal must not reuse continuation")
	require.Len(t, requests[4]["input"], 8)
	require.NotContains(t, requests[5], "previous_response_id", "different accepted content must not reuse continuation")
	require.Len(t, requests[5]["input"], 8)
}

func sealClientResponsesProposal(t *testing.T, proposal *config.RemoteRuntimeProposal) {
	t.Helper()
	var err error
	proposal.Digest, err = config.RemoteRuntimeDigest(*proposal)
	require.NoError(t, err)
}

func clientResponsesProposal(t *testing.T, endpoint string) config.RemoteRuntimeProposal {
	t.Helper()
	source := filepath.Join(t.TempDir(), "responses.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.CopyFS(source, os.DirFS(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "responses-oauth.plugin"))))
	data, err := os.ReadFile(filepath.Join(source, "manifest.json"))
	require.NoError(t, err)
	value, err := manifest.DecodeStrict(data)
	require.NoError(t, err)
	u, err := url.Parse(endpoint)
	require.NoError(t, err)
	for i := range value.Capabilities.Endpoints {
		item := &value.Capabilities.Endpoints[i]
		item.BaseURL = endpoint
		item.AllowedHosts = []string{u.Hostname()}
		item.AllowedSchemes = []string{"https"}
	}
	data, err = json.Marshal(value)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(root, filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true, ExpectedRevision: manager.Snapshot().Revision})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	bundles, err := manager.ExportRegisteredBundles(snapshot.Revision, map[string]string{status.ID: status.Digest})
	require.NoError(t, err)
	bundle, err := providerplugin.ValidateDetachedBundle(bundles[0])
	require.NoError(t, err)
	metadata, err := bundle.Catalog()
	require.NoError(t, err)
	registration, err := providerregistry.FromManifest(bundle.Provider().Manifest, bundle.Provider().StaticText)
	require.NoError(t, err)
	provider := config.ProviderConfig{ID: bundle.ProviderID(), Name: metadata.Name, Type: metadata.Type, BaseURL: endpoint, Models: metadata.Models, Plugin: &config.ProviderPluginReference{ID: bundle.ID(), Version: bundle.Version()}, Owner: &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction}, Configuration: map[string]any{"oauth_client_id": "synthetic-client"}}
	selected := config.SelectedModel{Provider: provider.ID, Model: provider.Models[0].ID}
	proposal := config.RemoteRuntimeProposal{Version: config.RemoteRuntimeVersion, Revision: 1, Bundles: bundles, Providers: []config.RemoteProviderDefinition{{Config: provider, BundleDigest: bundle.Digest()}}, Models: map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected}, Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, Account: &accounts.Entry{ID: "same-account", AccessToken: "synthetic-access", RefreshToken: "synthetic-refresh", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}}}}
	sealClientResponsesProposal(t, &proposal)
	return proposal
}
