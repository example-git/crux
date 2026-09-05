package providertransport

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestCompileResolvesOperationPolicyAndDefaults(t *testing.T) {
	value := manifest.Manifest{Capabilities: manifest.Capabilities{
		Endpoints: []manifest.Endpoint{{
			ID: "api", BaseURL: "https://example.invalid", AllowedSchemes: []string{"https"},
			AllowedHosts: []string{"example.invalid"}, Override: "same-origin",
		}},
		JSONTransforms: map[string]manifest.JSONPipeline{
			"request": {Operations: []manifest.JSONOperation{{Operation: "delete", Path: "/unsupported"}}},
		},
		PromptTransforms: map[string]manifest.PromptPipeline{
			"prompt": {Operations: []manifest.PromptOperation{{Operation: "remove-lines-with-prefix", Prefix: "private:"}}},
		},
		RoleMaps: map[string]manifest.RoleMap{
			"roles": {System: "system", User: "user", Assistant: "assistant", Tool: "tool", Unknown: "reject"},
		},
		ToolCodecs: map[string]manifest.ToolCodec{
			"tools": {Aliases: []manifest.ToolAlias{{Host: "view", Provider: "read"}}, Surfaces: []string{"definitions"}},
		},
		ClientIdentities: map[string]manifest.ResolvedClientIdentity{
			"synthetic": {CacheKey: "synthetic", FallbackVersion: "1.2.3", VersionPattern: `^\d+\.\d+\.\d+$`, UserAgentFormat: "synthetic/{version}"},
		},
	}}
	operation := manifest.Operation{
		ID: "inference", Kind: "inference", Protocol: "openai-responses", Transport: "sse",
		Endpoint: "api", Method: "POST", Path: "/v1/responses", ClientIdentity: "synthetic",
		RequestTransform: "request", PromptTransform: "prompt", RoleMap: "roles", ToolCodec: "tools",
	}
	compiled, err := Compile(value, operation)
	require.NoError(t, err)
	require.Equal(t, Key{Protocol: "openai-responses", Transport: "sse"}, compiled.Key)
	require.Equal(t, "https://example.invalid", compiled.Endpoint.BaseURL)
	require.Equal(t, DefaultConnectTimeout, compiled.ConnectTimeout)
	require.Equal(t, DefaultRequestTimeout, compiled.RequestTimeout)
	require.Equal(t, DefaultStreamIdle, compiled.StreamIdleTimeout)
	require.Equal(t, 1, compiled.Retry.MaxAttempts)
	require.Equal(t, "never", compiled.Retry.Authentication)
	require.Equal(t, "delete", compiled.RequestTransform.Operations[0].Operation)
	require.Equal(t, "remove-lines-with-prefix", compiled.PromptTransform.Operations[0].Operation)
	require.Equal(t, "system", compiled.RoleMap.System)
	require.Equal(t, "read", compiled.ToolCodec.Aliases[0].Provider)
	require.Equal(t, "synthetic/{version}", compiled.ClientIdentity.UserAgentFormat)

	compiled.Endpoint.AllowedHosts[0] = "mutated"
	clone := compiled.Clone()
	clone.ToolCodec.Aliases[0].Provider = "changed"
	clone.ClientIdentity.UserAgentFormat = "changed"
	require.Equal(t, "mutated", compiled.Endpoint.AllowedHosts[0])
	require.Equal(t, "read", compiled.ToolCodec.Aliases[0].Provider)
	require.Equal(t, "synthetic/{version}", compiled.ClientIdentity.UserAgentFormat)
}

func TestCompileScopesErrorMappingsToModelOperations(t *testing.T) {
	mappings := []manifest.ErrorMapping{{Class: "capacity", Statuses: []int{http.StatusServiceUnavailable}, Retryable: true}}
	value := manifest.Manifest{Capabilities: manifest.Capabilities{
		Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}},
		Errors:    mappings,
	}}
	for _, test := range []struct {
		kind string
		want bool
	}{
		{kind: "inference", want: true},
		{kind: "compaction", want: true},
		{kind: "account"},
		{kind: "usage"},
		{kind: "model-catalog"},
		{kind: "custom"},
	} {
		t.Run(test.kind, func(t *testing.T) {
			compiled, err := Compile(value, manifest.Operation{ID: test.kind, Kind: test.kind, Endpoint: "api", Path: "/operation"})
			require.NoError(t, err)
			if !test.want {
				require.Empty(t, compiled.Errors)
				return
			}
			require.Equal(t, mappings, compiled.Errors)
			compiled.Errors[0].Title = "changed"
			require.Empty(t, mappings[0].Title)
		})
	}
}

func TestOperationEnforcesEndpointAndOrderedHeaders(t *testing.T) {
	literal := func(value string) *manifest.Template { return &manifest.Template{Kind: "literal", Value: value} }
	operation := &Operation{
		ID: "inference",
		Endpoint: manifest.Endpoint{
			BaseURL: "wss://consumer.example.invalid/private/responses", AllowedSchemes: []string{"wss"},
			AllowedHosts: []string{"consumer.example.invalid"}, Override: "forbidden",
		},
		Headers: []manifest.HeaderRule{
			{Name: "Origin", Operation: "set", Value: literal("https://consumer.example.invalid"), Protected: true},
			{Name: "X-Feature", Operation: "append-unique", Value: literal("required"), Protected: true},
			{Name: "X-Remove", Operation: "delete"},
		},
	}
	endpoint, err := operation.ResolveEndpoint("wss://consumer.example.invalid/private/responses")
	require.NoError(t, err)
	require.Equal(t, operation.Endpoint.BaseURL, endpoint)
	_, err = operation.ResolveEndpoint("wss://other.example.invalid/private/responses")
	require.ErrorContains(t, err, "override is forbidden")

	headers, err := operation.ApplyHeaders(map[string]string{
		"origin": "https://attacker.invalid", "X-Feature": "optional,required", "X-Remove": "ambient",
	}, nil)
	require.NoError(t, err)
	require.Equal(t, "https://consumer.example.invalid", headers["Origin"])
	require.Equal(t, "optional,required", headers["X-Feature"])
	require.NotContains(t, headers, "X-Remove")
}

func TestOperationSameOriginEndpointPolicy(t *testing.T) {
	operation := &Operation{ID: "inference", Endpoint: manifest.Endpoint{
		BaseURL: "https://api.example.invalid/v1", AllowedSchemes: []string{"https"},
		AllowedHosts: []string{"api.example.invalid"}, Override: "same-origin",
	}}
	resolved, err := operation.ResolveEndpoint("https://api.example.invalid/custom")
	require.NoError(t, err)
	require.Equal(t, "https://api.example.invalid/custom", resolved)
	_, err = operation.ResolveEndpoint("https://other.example.invalid/custom")
	require.ErrorContains(t, err, "origin allowlist")
}

func TestOperationHTTPClientExecutesTimeoutAndRedirectPolicy(t *testing.T) {
	preserved := errors.New("preserved redirect policy")
	base := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return preserved }}
	operation := &Operation{RequestTimeout: 2 * time.Second, Endpoint: manifest.Endpoint{FollowRedirects: false}}
	client := operation.HTTPClient(base)
	require.NotSame(t, base, client)
	require.Equal(t, time.Second, base.Timeout)
	require.Equal(t, 2*time.Second, client.Timeout)
	require.ErrorIs(t, client.CheckRedirect(nil, nil), http.ErrUseLastResponse)

	operation.Endpoint.FollowRedirects = true
	client = operation.HTTPClient(base)
	require.ErrorIs(t, client.CheckRedirect(nil, nil), preserved)
}

func TestOperationValidateSelection(t *testing.T) {
	operation := &Operation{ID: "inference", Key: Key{Protocol: "openai-responses", Transport: "sse"}}
	require.NoError(t, operation.ValidateSelection("openai-responses", "sse"))
	require.ErrorContains(t, operation.ValidateSelection("gemini-generate-content", "sse"), "constructor requires")
	require.ErrorContains(t, operation.ValidateSelection("openai-responses", "websocket-json"), "unsupported")

	var compatibility *Operation
	require.NoError(t, compatibility.ValidateSelection("openai-responses", "sse"))
}

func TestCompileUsesExplicitTimeoutsAndRetry(t *testing.T) {
	value := manifest.Manifest{Capabilities: manifest.Capabilities{Endpoints: []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid"}}}}
	operation := manifest.Operation{
		ID: "inference", Kind: "inference", Protocol: "gemini-generate-content", Transport: "sse", Endpoint: "api", Path: "/v1beta/models/x:streamGenerateContent",
		Retry:    &manifest.RetryPolicy{MaxAttempts: 3, InitialDelayMS: 1000, Authentication: "refresh-once", ReplayRequirement: "before-first-event"},
		Timeouts: &manifest.TimeoutHints{ConnectSeconds: 5, RequestSeconds: 40, IdleSeconds: 12},
	}
	compiled, err := Compile(value, operation)
	require.NoError(t, err)
	require.Equal(t, 3, compiled.Retry.MaxAttempts)
	require.Equal(t, operation.Timeouts, compiled.Timeouts)
	require.NotSame(t, operation.Timeouts, compiled.Timeouts)
	require.Equal(t, 5*time.Second, compiled.ConnectTimeout)
	require.Equal(t, 40*time.Second, compiled.RequestTimeout)
	require.Equal(t, 12*time.Second, compiled.StreamIdleTimeout)
}

func TestCompileRejectsMissingReferences(t *testing.T) {
	_, err := Compile(manifest.Manifest{}, manifest.Operation{ID: "inference", Endpoint: "missing"})
	require.ErrorContains(t, err, `missing endpoint "missing"`)
}
