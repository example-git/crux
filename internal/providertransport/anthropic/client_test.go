package anthropic

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	"github.com/stretchr/testify/require"
)

func syntheticAnthropicOperation(t *testing.T) *providertransport.Operation {
	t.Helper()
	literal := func(value string) *manifest.Template {
		return &manifest.Template{Kind: "literal", Value: value}
	}
	contextValue := func(ref string) *manifest.Template {
		return &manifest.Template{Kind: "context", Ref: ref}
	}
	value := manifest.Manifest{Capabilities: manifest.Capabilities{
		Endpoints: []manifest.Endpoint{{
			ID: "inference", BaseURL: "https://api.example.test", AllowedSchemes: []string{"https"},
			AllowedHosts: []string{"api.example.test"}, Override: "forbidden",
		}},
		Headers: []manifest.HeaderRule{
			{Operation: "set", Name: "User-Agent", Value: contextValue("client.user_agent"), Protected: true},
			{Operation: "set", Name: "X-App", Value: literal("test"), Protected: true},
			{Operation: "append-unique", Name: "Anthropic-Beta", Value: literal("feature-a"), Protected: true},
			{Operation: "append-unique", Name: "Anthropic-Beta", Value: literal("feature-b"), Protected: true},
		},
		JSONTransforms: map[string]manifest.JSONPipeline{
			"request": {MaxOperations: 2, Operations: []manifest.JSONOperation{
				{Operation: "delete", Path: "/top_k"},
				{Operation: "delete", Path: "/top_p"},
			}},
		},
		ToolCodecs: map[string]manifest.ToolCodec{
			"tools": {
				Aliases:         []manifest.ToolAlias{{Host: "grep", Provider: "Search"}},
				Parameters:      []manifest.ParameterMap{{Tool: "grep", Host: "include", Provider: "pattern"}},
				Surfaces:        []string{"definitions", "prompt-references", "history-calls", "history-results", "stream-events"},
				CaseFoldInbound: true,
			},
		},
		Anthropic: &manifest.AnthropicPolicy{
			SessionHeader: "X-Synthetic-Session-Id", DeleteHeaderPrefixes: []string{"X-Untrusted-"},
			DeleteHeaders: []string{"X-Api-Key", "X-Session-Id"}, MaxRequestBytes: 1 << 20,
			TransformFailure: "error", SystemLinePrefixes: []string{"Workspace root folder:"},
			SystemPrefixes: []string{"Synthetic host prefix"}, MetadataUserID: true,
			Billing:             &manifest.BillingAttribution{Salt: "synthetic", ByteOffsets: []int{0}, MissingByte: "?", HashPrefix: 8, Format: "client_version={version};fingerprint={fingerprint}"},
			StreamHoldbackBytes: 256,
		},
	}}
	operation := manifest.Operation{
		ID: "messages", Kind: "inference", Protocol: "anthropic-messages", Transport: "sse",
		Endpoint: "inference", Method: http.MethodPost, Path: "/v1/messages",
		RequestTransform: "request", ToolCodec: "tools",
	}
	compiled, err := providertransport.Compile(value, operation)
	require.NoError(t, err)
	return compiled
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func TestPluginPolicyOwnsProtectedInferenceHeaders(t *testing.T) {
	operation := syntheticAnthropicOperation(t)
	client := &Client{
		operation: operation,
		version:   "9.9.9",
		userAgent: "synthetic-client/9.9.9",
		sessionID: "session-id",
		base: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			require.Empty(t, request.Header.Get("X-Api-Key"))
			require.Empty(t, request.Header.Get("X-Untrusted-Test"))
			require.Empty(t, request.Header.Get("X-Session-Id"))
			require.Equal(t, "Bearer token", request.Header.Get("Authorization"))
			require.Equal(t, "synthetic-client/9.9.9", request.Header.Get("User-Agent"))
			require.Equal(t, "test", request.Header.Get("X-App"))
			require.Equal(t, "session-id", request.Header.Get("X-Synthetic-Session-Id"))
			require.Equal(t, []string{"feature-a", "feature-b", "caller-beta"}, splitHeader(request.Header.Get("Anthropic-Beta")))
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header), Request: request}, nil
		}),
	}
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://api.anthropic.com/v1/messages", strings.NewReader(`{"messages":[]}`))
	require.NoError(t, err)
	request.Header.Set("Authorization", "Bearer token")
	request.Header.Set("X-Api-Key", "ambient-secret")
	request.Header.Set("X-Untrusted-Test", "remove")
	request.Header.Set("X-Session-Id", "remove")
	request.Header.Set("Anthropic-Beta", "caller-beta,feature-a")
	response, err := client.RoundTrip(request)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
}

func TestPluginPolicyRewritesRequestWithoutDroppingImages(t *testing.T) {
	operation := syntheticAnthropicOperation(t)
	body := []byte(`{
		"model":"claude-test",
		"top_k":10,
		"top_p":0.9,
		"system":[{"type":"text","text":"  Workspace root folder: /tmp/project\r\nUse the available tools."}],
		"messages":[
			{"role":"user","content":[{"type":"text","text":"inspect this image"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBORw=="}}]},
			{"role":"assistant","content":[{"type":"tool_use","id":"tool-1","name":"grep","input":{"include":"*.go"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"tool-1","name":"grep","content":"ok"}]}
		],
		"tools":[{"name":"grep","description":"Use glob patterns.","input_schema":{"type":"object","properties":{"include":{"type":"string"}},"required":["include"]}}]
	}`)

	rewritten, err := rewriteBody(body, operation, "9.9.9", "session-id")
	require.NoError(t, err)
	var request map[string]any
	require.NoError(t, json.Unmarshal(rewritten, &request))
	require.NotContains(t, request, "top_k")
	require.NotContains(t, request, "top_p")

	encoded := string(rewritten)
	require.NotContains(t, encoded, "Workspace root folder:")
	require.Contains(t, encoded, "Synthetic host prefix")
	require.Contains(t, encoded, "client_version=9.9.9")
	require.Contains(t, encoded, `"name":"Search"`)
	require.Contains(t, encoded, `"pattern"`)
	require.NotContains(t, encoded, `"include"`)
	require.Contains(t, encoded, `"media_type":"image/png"`)
	require.Contains(t, encoded, `"data":"iVBORw=="`)

	metadata := request["metadata"].(map[string]any)
	var identity map[string]string
	require.NoError(t, json.Unmarshal([]byte(metadata["user_id"].(string)), &identity))
	require.Equal(t, "session-id", identity["device_id"])
	require.Equal(t, "session-id", identity["session_id"])
}

func TestPluginToolCodecReversesAliasesAndParameters(t *testing.T) {
	operation := syntheticAnthropicOperation(t)
	input := `data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"SEARCH","input":{"pattern":"*.go"}}}`
	output := remapText(input, operation.ToolCodec)
	require.Contains(t, output, `"name":"grep"`)
	require.Contains(t, output, `"include":"*.go"`)
	require.NotContains(t, output, `"name":"SEARCH"`)
}

func TestPluginToolCodecHandlesSplitStreamChunks(t *testing.T) {
	operation := syntheticAnthropicOperation(t)
	first := `data: {"type":"content_block_start","content_block":{"type":"tool_use","name":"Sea`
	second := `rch","input":{"pattern":"*.go"}}}` + "\n\n"
	pipeReader, pipeWriter := io.Pipe()
	go func() {
		_, _ = pipeWriter.Write([]byte(first))
		_, _ = pipeWriter.Write([]byte(second))
		_ = pipeWriter.Close()
	}()

	reader := &remapReader{reader: pipeReader, codec: operation.ToolCodec, holdback: operation.Anthropic.StreamHoldbackBytes}
	output, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(output), `"name":"grep"`)
	require.Contains(t, string(output), `"include":"*.go"`)
}

func TestResolvedIdentityUsesConfiguredDirectoryAndValidatedFallback(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("AI_CLI_DIR", directory)
	t.Setenv("SYNTHETIC_CLIENT_VERSION", "not-a-version")
	identity := &manifest.ResolvedClientIdentity{
		Environment: "SYNTHETIC_CLIENT_VERSION", CacheKey: "synthetic", FallbackVersion: "1.2.3",
		VersionPattern: `^\d+\.\d+\.\d+$`, UserAgentFormat: "synthetic/{version}",
	}
	version, userAgent, err := ResolveIdentity(identity)
	require.NoError(t, err)
	require.Equal(t, "1.2.3", version)
	require.Equal(t, "synthetic/1.2.3", userAgent)
	data, err := os.ReadFile(filepath.Join(directory, "provider-client-versions.json"))
	require.NoError(t, err)
	require.True(t, bytes.Contains(data, []byte(`"synthetic":"1.2.3"`)))
}

func TestEffectiveBaseURLRejectsForbiddenOverride(t *testing.T) {
	operation := syntheticAnthropicOperation(t)
	baseURL, err := EffectiveBaseURL(operation, operation.Endpoint.BaseURL)
	require.NoError(t, err)
	require.Equal(t, strings.TrimRight(operation.Endpoint.BaseURL, "/"), strings.TrimRight(baseURL, "/"))
	_, err = EffectiveBaseURL(operation, "https://example.invalid")
	require.ErrorContains(t, err, "forbidden")
}
