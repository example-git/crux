package agent

import (
	"fmt"
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
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providerregistry"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

func TestClientAnthropicIdentityReachesInferenceWithoutServerResolution(t *testing.T) {
	t.Setenv("CLAUDE_CODE_VERSION", "")
	var latestRequests, inferenceRequests atomic.Int32
	const clientUserAgent = "claude-cli/1.2.3 (client-os; client-arch)"
	host := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			latestRequests.Add(1)
			_, _ = fmt.Fprint(w, "9.9.9")
		case "/v1/messages":
			inferenceRequests.Add(1)
			assert.Equal(t, clientUserAgent, r.Header.Get("User-Agent"))
			assert.Equal(t, "Bearer synthetic-client-key", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-test\",\"content\":[],\"stop_reason\":null,\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"client identity verified\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		default:
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	}))
	defer host.Close()
	endpoint, err := url.Parse(host.URL)
	require.NoError(t, err)

	source := filepath.Join(t.TempDir(), "claude.plugin")
	require.NoError(t, os.MkdirAll(source, 0o700))
	templatePath, err := filepath.Abs(filepath.Join("..", "..", "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json"))
	require.NoError(t, err)
	data, err := os.ReadFile(templatePath)
	require.NoError(t, err)
	for path, value := range map[string]any{
		"id":                                     "test.claude",
		"provider.id":                            "claude-ai",
		"provider.account_namespace":             "test.claude",
		"capabilities.endpoints.0.base_url":      host.URL,
		"capabilities.endpoints.0.allowed_hosts": []string{endpoint.Hostname()},
		"capabilities.operations.0.protocol":     string(providerregistry.ConstructionAnthropicMessages),
		"capabilities.operations.0.transport":    "sse",
		"capabilities.operations.0.path":         "/v1/messages",
		"capabilities.headers":                   []manifest.HeaderRule{{Operation: "set", Name: "User-Agent", Value: &manifest.Template{Kind: "context", Ref: "client.user_agent"}, Protected: true}},
		"capabilities.anthropic": manifest.AnthropicPolicy{
			ClientIdentity: &manifest.ResolvedClientIdentity{
				Environment: "CLAUDE_CODE_VERSION", LatestURL: host.URL + "/latest", CacheKey: "claude-agent-test",
				FallbackVersion: "0.0.1", VersionPattern: `^[0-9]+\.[0-9]+\.[0-9]+$`, UserAgentFormat: "claude-cli/{version} ({os}; {arch})",
				ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024,
			},
			MaxRequestBytes: 1 << 20, TransformFailure: "error",
		},
	} {
		data, err = sjson.SetBytes(data, path, value)
		require.NoError(t, err)
	}
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(root, filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	snapshot, err := manager.Install(t.Context(), providerplugin.InstallRequest{Source: source})
	require.NoError(t, err)
	status := snapshot.Plugins[0]
	snapshot, err = manager.SetTrust(t.Context(), status.ID, providerplugin.TrustRequest{Digest: status.Digest, Trusted: true})
	require.NoError(t, err)
	bundles, err := manager.ExportRegisteredBundles(snapshot.Revision, map[string]string{status.ID: status.Digest})
	require.NoError(t, err)
	detached, err := providerplugin.ValidateDetachedBundle(bundles[0])
	require.NoError(t, err)
	registered := detached.Provider()
	require.NotNil(t, registered)
	registration, err := providerregistry.FromManifest(registered.Manifest, registered.StaticText)
	require.NoError(t, err)
	metadata, err := detached.Catalog()
	require.NoError(t, err)
	selected := config.SelectedModel{Provider: "claude-ai", Model: metadata.Models[0].ID}
	proposal := config.RemoteRuntimeProposal{
		Version: config.RemoteRuntimeVersion, Revision: 1, Bundles: bundles,
		Providers: []config.RemoteProviderDefinition{{
			Config: config.ProviderConfig{
				ID: "claude-ai", Name: metadata.Name, Type: metadata.Type, BaseURL: metadata.APIEndpoint, Models: metadata.Models,
				Owner:  &config.ProviderOwnerReference{Type: config.ProviderOwnerPlugin, Construction: registration.Construction},
				Plugin: &config.ProviderPluginReference{ID: detached.ID(), Version: detached.Version()},
			},
			BundleDigest:   detached.Digest(),
			ClientIdentity: &config.ResolvedProviderClientIdentity{Version: "1.2.3", UserAgent: clientUserAgent, OS: "client-os", Arch: "client-arch"},
		}},
		Models:      map[config.SelectedModelType]config.SelectedModel{config.SelectedModelTypeLarge: selected, config.SelectedModelTypeSmall: selected},
		Credentials: []config.RemoteCredentialBinding{{Owner: registration.Owner(), Generation: 1, APIKey: "synthetic-client-key"}},
	}
	proposal.Digest, err = config.RemoteRuntimeDigest(proposal)
	require.NoError(t, err)
	store, err := config.CompileRemoteRuntime(root, filepath.Join(root, "remote"), false, proposal, strings.Repeat("a", 64), env.NewFromMap(map[string]string{"CLAUDE_CODE_VERSION": "server-version"}))
	require.NoError(t, err)
	oldTransport := http.DefaultTransport
	http.DefaultTransport = host.Client().Transport
	defer func() { http.DefaultTransport = oldTransport }()
	coord := &coordinator{cfg: store}
	providerConfig, ok := store.Config().Providers.Get("claude-ai")
	require.True(t, ok)
	provider, err := coord.buildProvider(store.RuntimeSnapshot(), providerConfig, selected, false)
	require.NoError(t, err)
	model, err := provider.LanguageModel(t.Context(), selected.Model)
	require.NoError(t, err)
	stream, err := model.Stream(t.Context(), fantasy.Call{Prompt: fantasy.Prompt{fantasy.NewUserMessage("verify identity")}})
	require.NoError(t, err)
	for part := range stream {
		require.NoError(t, part.Error)
	}
	require.EqualValues(t, 1, inferenceRequests.Load())
	require.Zero(t, latestRequests.Load(), "the execution host must not resolve client identity")
}
