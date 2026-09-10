package providerregistry

import (
	"testing"

	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/stretchr/testify/require"
)

func TestModelCatalogActivationMatchesExecutor(t *testing.T) {
	base := manifest.Manifest{Provider: manifest.Provider{ID: "catalog", Name: "Catalog"}, Capabilities: manifest.Capabilities{
		Endpoints:  []manifest.Endpoint{{ID: "api", BaseURL: "https://example.invalid", AllowedSchemes: []string{"https"}, AllowedHosts: []string{"example.invalid"}, Override: "forbidden"}},
		Operations: []manifest.Operation{{ID: "inference", Kind: "inference", Protocol: "openai-responses", Transport: "sse", Endpoint: "api", Method: "POST", Path: "/response"}, {ID: "catalog", Kind: "model-catalog", Protocol: "generic-json", Transport: "http-json", Endpoint: "api", Method: "GET", Path: "/catalog"}},
	}}
	for _, test := range []struct {
		name  string
		edit  func(*manifest.Operation)
		valid bool
	}{
		{"declared", func(*manifest.Operation) {}, true},
		{"idempotent retry", func(o *manifest.Operation) {
			o.Retry = &manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "idempotent", Statuses: []int{503}}
		}, true},
		{"request timeout", func(o *manifest.Operation) { o.Timeouts = &manifest.TimeoutHints{RequestSeconds: 1, ConnectSeconds: 1} }, true},
		{"stream", func(o *manifest.Operation) { o.Transport = "sse" }, false},
		{"native framing", func(o *manifest.Operation) { o.Protocol = "openai-responses" }, false},
		{"implicit method", func(o *manifest.Operation) { o.Method = "" }, false},
		{"fragment", func(o *manifest.Operation) { o.Path = "/catalog#ignored" }, false},
		{"model placeholder", func(o *manifest.Operation) { o.Path = "/models/{model}" }, false},
		{"alternate origin", func(o *manifest.Operation) { o.Path = "//elsewhere.invalid/catalog" }, false},
		{"refresh", func(o *manifest.Operation) {
			o.Retry = &manifest.RetryPolicy{MaxAttempts: 1, Authentication: "refresh-once", ReplayRequirement: "never"}
		}, false},
		{"non-idempotent retry", func(o *manifest.Operation) {
			o.Retry = &manifest.RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "never"}
		}, false},
		{"idle timeout", func(o *manifest.Operation) { o.Timeouts = &manifest.TimeoutHints{IdleSeconds: 1} }, false},
		{"continuation", func(o *manifest.Operation) { o.Continuation = &manifest.ContinuationPolicy{Mode: "none"} }, false},
		{"compaction", func(o *manifest.Operation) { o.Compaction = &manifest.CompactionPolicy{Mode: "none"} }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			declaration := base
			declaration.Capabilities.Operations = append([]manifest.Operation(nil), base.Capabilities.Operations...)
			test.edit(&declaration.Capabilities.Operations[1])
			registration, err := FromManifest(declaration)
			require.NoError(t, err)
			_, err = New(registration)
			if test.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "model-catalog")
			}
		})
	}
}
