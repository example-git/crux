package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	validator "github.com/kaptinlin/jsonschema"
	"github.com/stretchr/testify/require"
)

func TestCanonicalExamples(t *testing.T) {
	t.Parallel()

	schemaData, err := SchemaJSON()
	require.NoError(t, err)
	for _, name := range []string{"minimal.plugin", "responses-oauth.plugin"} {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			compiled, err := validator.NewCompiler().Compile(schemaData)
			require.NoError(t, err)
			data := readRepoFile(t, "docs", "provider-plugins", "examples", name, "manifest.json")
			result := compiled.ValidateJSON(data)
			require.True(t, result.IsValid(), "schema errors: %+v", result.Errors)
			value, err := DecodeStrict(data)
			require.NoError(t, err)
			require.Equal(t, Version, value.ManifestVersion)
		})
	}
}

func TestCompatibilityInventoryClassifiesEveryDelegate(t *testing.T) {
	t.Parallel()
	data := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(data)
	require.NoError(t, err)
	value.Capabilities.Compatibility = &CompatibilityAdapter{
		ID:        "integrated-example",
		Delegates: []string{"construction", "usage"},
		Inventory: []CompatibilityInventoryItem{
			{Delegate: "construction", Classification: "finite-core-primitive", Behavior: "Construct requests", Primitive: "responses-http"},
			{Delegate: "usage", Classification: "private-stateful", Behavior: "Resolve account usage"},
		},
	}
	require.NoError(t, Validate(value))

	tests := []struct {
		name string
		edit func(*CompatibilityAdapter)
		want string
	}{
		{
			name: "undelegated inventory item",
			edit: func(adapter *CompatibilityAdapter) { adapter.Inventory[0].Delegate = "oauth" },
			want: "is not delegated",
		},
		{
			name: "unclassified delegate",
			edit: func(adapter *CompatibilityAdapter) {
				adapter.Inventory = slices.DeleteFunc(adapter.Inventory, func(item CompatibilityInventoryItem) bool { return item.Delegate == "usage" })
			},
			want: "must classify delegated capability \"usage\"",
		},
		{
			name: "finite primitive unnamed",
			edit: func(adapter *CompatibilityAdapter) {
				for index := range adapter.Inventory {
					if adapter.Inventory[index].Classification == "finite-core-primitive" {
						adapter.Inventory[index].Primitive = ""
						return
					}
				}
			},
			want: "must name the proposed finite core primitive",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			copy := deepCopy(t, value)
			test.edit(copy.Capabilities.Compatibility)
			require.ErrorContains(t, Validate(copy), test.want)
		})
	}
}

func TestValidateDefersUsageEventExecutionToActivation(t *testing.T) {
	data := readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json")
	value, err := DecodeStrict(data)
	require.NoError(t, err)
	operation := &value.Capabilities.Operations[0]
	operation.Transport = "sse"
	operation.Streaming = &StreamingPolicy{
		EventSource:      "sse-data-json",
		EventTypePointer: "/type",
		MaxEventBytes:    1024,
		Mappings: []EventMapping{
			{Source: "usage", Event: "usage"},
			{Source: "done", Event: "finish"},
		},
	}
	require.NoError(t, Validate(value))
}

func TestValidateUsageOperationPipeline(t *testing.T) {
	data := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(data)
	require.NoError(t, err)
	endpoint := value.Capabilities.Endpoints[0].ID
	value.Capabilities.ClientIdentities = map[string]ResolvedClientIdentity{
		"synthetic": {
			LatestURL: "https://releases.example.invalid/{os}_{arch}.json", VersionPointer: "/version",
			CacheKey: "synthetic", FallbackVersion: "1.2.3", VersionPattern: `^\d+\.\d+\.\d+$`,
			UserAgentFormat: "synthetic/{version} {os}/{arch}", ProbeTimeoutMS: 1000, ProbeMaxBytes: 1024,
		},
	}
	value.Capabilities.JSONTransforms["usage-setup"] = JSONPipeline{MaxOperations: 1, Operations: []JSONOperation{{
		Operation: "set", Path: "/metadata/client", Value: &Template{Kind: "literal", Value: "synthetic"},
	}}}
	value.Capabilities.JSONTransforms["usage-summary"] = JSONPipeline{MaxOperations: 1, Operations: []JSONOperation{{
		Operation: "set", Path: "/project", Value: &Template{Kind: "context", Ref: "usage.project"},
	}}}
	value.Capabilities.Operations = append(value.Capabilities.Operations,
		Operation{ID: "usage-setup", Kind: "account", Protocol: "generic-json", Transport: "http-json", Endpoint: endpoint, Method: "POST", Path: "/usage/setup", ClientIdentity: "synthetic", Headers: []HeaderRule{{Operation: "set", Name: "User-Agent", Value: &Template{Kind: "context", Ref: "client.user_agent"}}}, RequestTransform: "usage-setup"},
		Operation{ID: "usage-summary", Kind: "usage", Protocol: "generic-json", Transport: "http-json", Endpoint: endpoint, Method: "POST", Path: "/usage/summary", ClientIdentity: "synthetic", Headers: []HeaderRule{{Operation: "set", Name: "User-Agent", Value: &Template{Kind: "context", Ref: "client.user_agent"}}}, RequestTransform: "usage-summary"},
	)
	value.Capabilities.Usage = &UsagePolicy{
		Setup: []UsageSetup{{
			Operation:    "usage-setup",
			Extract:      []UsageContextExtraction{{Context: "usage.project", Pointer: "/project/id"}},
			PlanPointers: []string{"/plan/preferred", "/plan/fallback"},
		}},
		Operation: "usage-summary", Source: "operation", Fallback: "unavailable",
		Windows: []WindowMap{{ID: "weekly", RemainingFractionPointer: "/groups/0/buckets/0/remaining", ResetPointer: "/groups/0/buckets/0/reset", ResetFormat: "rfc3339"}},
	}
	require.NoError(t, Validate(value))

	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{name: "unknown setup operation", edit: func(value *Manifest) {
			value.Capabilities.Usage.Setup[0].Operation = "missing"
		}, want: "references unknown id"},
		{name: "unavailable request context", edit: func(value *Manifest) {
			value.Capabilities.JSONTransforms["usage-summary"] = JSONPipeline{MaxOperations: 1, Operations: []JSONOperation{{
				Operation: "set", Path: "/project", Value: &Template{Kind: "context", Ref: "usage.missing"},
			}}}
		}, want: "references unavailable usage context"},
		{name: "malformed extraction pointer", edit: func(value *Manifest) {
			value.Capabilities.Usage.Setup[0].Extract[0].Pointer = "/project/~bad"
		}, want: "valid non-root JSON Pointer"},
		{name: "ambiguous fraction window", edit: func(value *Manifest) {
			value.Capabilities.Usage.Windows[0].LimitPointer = "/groups/0/buckets/0/limit"
		}, want: "cannot be combined with remaining_fraction_pointer"},
		{name: "unsupported retry", edit: func(value *Manifest) {
			for i := range value.Capabilities.Operations {
				if value.Capabilities.Operations[i].ID == "usage-summary" {
					value.Capabilities.Operations[i].Retry = &RetryPolicy{MaxAttempts: 2, Authentication: "never", ReplayRequirement: "never"}
				}
			}
		}, want: "retry policy is unavailable"},
		{name: "unknown client identity", edit: func(value *Manifest) {
			for i := range value.Capabilities.Operations {
				if value.Capabilities.Operations[i].ID == "usage-summary" {
					value.Capabilities.Operations[i].ClientIdentity = "missing"
				}
			}
		}, want: "references unknown id"},
		{name: "unbound client user agent", edit: func(value *Manifest) {
			for i := range value.Capabilities.Operations {
				if value.Capabilities.Operations[i].ID == "usage-summary" {
					value.Capabilities.Operations[i].ClientIdentity = ""
				}
			}
		}, want: "references unavailable usage context"},
		{name: "invalid version pointer", edit: func(value *Manifest) {
			identity := value.Capabilities.ClientIdentities["synthetic"]
			identity.VersionPointer = "/~bad"
			value.Capabilities.ClientIdentities["synthetic"] = identity
		}, want: "version_pointer is not a valid non-root JSON Pointer"},
		{name: "unused client identity", edit: func(value *Manifest) {
			value.Capabilities.ClientIdentities["unused"] = value.Capabilities.ClientIdentities["synthetic"]
		}, want: "is not referenced by an operation"},
		{name: "operation usage mappings", edit: func(value *Manifest) {
			value.Capabilities.Usage.Mappings = []UsageMapping{{Target: "input_tokens", Pointer: "/usage/input", Operation: "replace"}}
		}, want: "usage.mappings are unavailable for source operation"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := deepCopy(t, value)
			test.edit(&copy)
			require.ErrorContains(t, Validate(copy), test.want)
		})
	}
}

func TestSchemaEnforcesClosedObjectsAndUniqueCollections(t *testing.T) {
	t.Parallel()

	schemaData, err := SchemaJSON()
	require.NoError(t, err)
	compiled, err := validator.NewCompiler().Compile(schemaData)
	require.NoError(t, err)
	base := readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json")
	var object map[string]any
	require.NoError(t, json.Unmarshal(base, &object))
	object["unknown"] = true
	data, err := json.Marshal(object)
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(data).IsValid())

	require.NoError(t, json.Unmarshal(base, &object))
	models := object["models"].([]any)
	modalities := models[0].(map[string]any)["modalities"].(map[string]any)
	modalities["input"] = []any{"text", "text"}
	data, err = json.Marshal(object)
	require.NoError(t, err)
	require.False(t, compiled.ValidateJSON(data).IsValid())
}

func TestCheckedInSchemaIsCurrent(t *testing.T) {
	t.Parallel()

	generated, err := SchemaJSON()
	require.NoError(t, err)
	checkedIn := readRepoFile(t, "provider-plugin.schema.json")
	require.Equal(t, normalizeSchemaLineEndings(generated), normalizeSchemaLineEndings(checkedIn), "run `task schema` to update provider-plugin.schema.json")
}

func TestDecodeStrictRejectsUnknownAndTrailingData(t *testing.T) {
	t.Parallel()

	base := readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json")
	var object map[string]any
	require.NoError(t, json.Unmarshal(base, &object))
	object["unknown"] = true
	unknown, err := json.Marshal(object)
	require.NoError(t, err)
	_, err = DecodeStrict(unknown)
	require.ErrorContains(t, err, "unknown field")

	_, err = DecodeStrict(append(base, []byte(` {}`)...))
	require.ErrorContains(t, err, "multiple JSON values")
	_, err = DecodeStrict(make([]byte, MaxManifestBytes+1))
	require.ErrorContains(t, err, "exceeds")
}

func TestDecodeStrictEnforcesImageSchema(t *testing.T) {
	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	tests := []struct {
		name string
		edit func(map[string]any)
		want string
	}{
		{name: "missing accepted media types", edit: func(images map[string]any) { delete(images, "accepted_media_types") }, want: "provider plugin schema"},
		{name: "missing source limit", edit: func(images map[string]any) { delete(images, "max_source_bytes") }, want: "provider plugin schema"},
		{name: "empty accepted media types", edit: func(images map[string]any) { images["accepted_media_types"] = []any{} }, want: "provider plugin schema"},
		{name: "duplicate accepted media type", edit: func(images map[string]any) { images["accepted_media_types"] = []any{"image/jpeg", "image/jpeg"} }, want: "provider plugin schema"},
		{name: "zero source limit", edit: func(images map[string]any) { images["max_source_bytes"] = 0 }, want: "provider plugin schema"},
		{name: "zero side limit", edit: func(images map[string]any) { images["max_side_pixels"] = 0 }, want: "provider plugin schema"},
		{name: "zero output limit", edit: func(images map[string]any) { images["max_output_bytes"] = 0 }, want: "provider plugin schema"},
		{name: "zero patch limit", edit: func(images map[string]any) { images["max_patches"] = 0 }, want: "provider plugin schema"},
		{name: "unsupported alpha mode", edit: func(images map[string]any) { images["flatten_alpha"] = "transparent" }, want: "provider plugin schema"},
		{name: "duplicate quality", edit: func(images map[string]any) { images["quality_steps"] = []any{85, 85} }, want: "provider plugin schema"},
		{name: "zero resize", edit: func(images map[string]any) { images["resize_percent"] = 0 }, want: "provider plugin schema"},
		{name: "zero history request limit", edit: func(images map[string]any) {
			images["history_budget"] = map[string]any{"request_bytes": 0}
		}, want: "provider plugin schema"},
		{name: "zero history retry limit", edit: func(images map[string]any) {
			images["history_budget"] = map[string]any{"request_bytes": 1, "retry_request_bytes": 0}
		}, want: "provider plugin schema"},
		{name: "duplicate history target", edit: func(images map[string]any) {
			images["history_budget"] = map[string]any{"request_bytes": 1, "per_image_targets": []any{1, 1}}
		}, want: "provider plugin schema"},
		{name: "non-positive history target", edit: func(images map[string]any) {
			images["history_budget"] = map[string]any{"request_bytes": 1, "per_image_targets": []any{0}}
		}, want: "provider plugin schema"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var object map[string]any
			require.NoError(t, json.Unmarshal(base, &object))
			capabilities := object["capabilities"].(map[string]any)
			images := capabilities["images"].(map[string]any)
			test.edit(images)
			data, err := json.Marshal(object)
			require.NoError(t, err)
			_, err = DecodeStrict(data)
			require.ErrorContains(t, err, test.want)
		})
	}

	var object map[string]any
	require.NoError(t, json.Unmarshal(base, &object))
	privateValue := strings.Repeat("private-value-", 100)
	object["name"] = privateValue
	data, err := json.Marshal(object)
	require.NoError(t, err)
	_, err = DecodeStrict(data)
	require.ErrorContains(t, err, "provider plugin schema")
	require.NotContains(t, err.Error(), privateValue)
}

func TestValidateRejectsUnsupportedModalitiesAndImagePolicies(t *testing.T) {
	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)
	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{name: "audio input", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"text", "audio"} }, want: "audio\" is not executable"},
		{name: "video input", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"text", "video"} }, want: "video\" is not executable"},
		{name: "document input", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"text", "document"} }, want: "document\" is not executable"},
		{name: "image output", edit: func(value *Manifest) { value.Models[0].Modalities.Output = []string{"text", "image"} }, want: "image\" is not executable"},
		{name: "audio output", edit: func(value *Manifest) { value.Models[0].Modalities.Output = []string{"text", "audio"} }, want: "audio\" is not executable"},
		{name: "duplicate input", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"text", "text"} }, want: "duplicate modality"},
		{name: "duplicate output", edit: func(value *Manifest) { value.Models[0].Modalities.Output = []string{"text", "text"} }, want: "duplicate modality"},
		{name: "input without text", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"image"} }, want: "must include text"},
		{name: "output without text", edit: func(value *Manifest) { value.Models[0].Modalities.Output = []string{"image"} }, want: "must include text"},
		{name: "image input without policy", edit: func(value *Manifest) { value.Capabilities.Images = nil }, want: "capabilities.images is required"},
		{name: "image policy without image input", edit: func(value *Manifest) { value.Models[0].Modalities.Input = []string{"text"} }, want: "has no model declaring image input"},
		{name: "unsupported accepted media type", edit: func(value *Manifest) { value.Capabilities.Images.AcceptedMediaTypes = []string{"image/svg+xml"} }, want: "unsupported media type"},
		{name: "output outside accepted media", edit: func(value *Manifest) { value.Capabilities.Images.AcceptedMediaTypes = []string{"image/png"} }, want: "is not declared in accepted_media_types"},
		{name: "implicit output without core encoder", edit: func(value *Manifest) {
			value.Capabilities.Images.AcceptedMediaTypes = []string{"image/gif", "image/webp"}
			value.Capabilities.Images.OutputMediaType = ""
			value.Capabilities.Images.QualitySteps = nil
		}, want: "no accepted output media type supported by the core image encoder"},
		{name: "negative side limit", edit: func(value *Manifest) { value.Capabilities.Images.MaxSidePixels = -1 }, want: "max_side_pixels"},
		{name: "negative output limit", edit: func(value *Manifest) { value.Capabilities.Images.MaxOutputBytes = -1 }, want: "max_output_bytes"},
		{name: "negative patch limit", edit: func(value *Manifest) { value.Capabilities.Images.MaxPatches = -1 }, want: "max_patches"},
		{name: "invalid alpha mode", edit: func(value *Manifest) { value.Capabilities.Images.FlattenAlpha = "transparent" }, want: "flatten_alpha"},
		{name: "duplicate quality", edit: func(value *Manifest) { value.Capabilities.Images.QualitySteps = []int{85, 85} }, want: "duplicate JPEG quality"},
		{name: "quality without transform", edit: func(value *Manifest) {
			value.Capabilities.Images.MaxSidePixels = 0
			value.Capabilities.Images.MaxOutputBytes = 0
			value.Capabilities.Images.MaxPatches = 0
			value.Capabilities.Images.OutputMediaType = ""
			value.Capabilities.Images.FlattenAlpha = "none"
			value.Capabilities.Images.QualitySteps = []int{85}
		}, want: "no executable image transformation"},
		{name: "quality without JPEG output", edit: func(value *Manifest) {
			value.Capabilities.Images.AcceptedMediaTypes = []string{"image/png"}
			value.Capabilities.Images.OutputMediaType = "image/png"
			value.Capabilities.Images.QualitySteps = []int{85}
		}, want: "requires executable JPEG output"},
		{name: "negative resize", edit: func(value *Manifest) { value.Capabilities.Images.ResizePercent = -1 }, want: "resize_percent"},
		{name: "resize without output limit", edit: func(value *Manifest) {
			value.Capabilities.Images.MaxOutputBytes = 0
			value.Capabilities.Images.ResizePercent = 80
		}, want: "requires max_output_bytes"},
		{name: "invalid history request limit", edit: func(value *Manifest) { value.Capabilities.Images.HistoryBudget = &ImageHistoryBudget{} }, want: "request_bytes must be positive"},
		{name: "negative history retry limit", edit: func(value *Manifest) {
			value.Capabilities.Images.HistoryBudget = &ImageHistoryBudget{RequestBytes: 1, RetryRequestBytes: -1}
		}, want: "retry_request_bytes"},
		{name: "duplicate history target", edit: func(value *Manifest) {
			value.Capabilities.Images.HistoryBudget = &ImageHistoryBudget{RequestBytes: 1, PerImageTargets: []int64{1, 1}}
		}, want: "duplicate target"},
		{name: "non-positive history target", edit: func(value *Manifest) {
			value.Capabilities.Images.HistoryBudget = &ImageHistoryBudget{RequestBytes: 1, PerImageTargets: []int64{0}}
		}, want: "non-positive target"},
		{name: "retain newest without omission", edit: func(value *Manifest) {
			value.Capabilities.Images.HistoryBudget = &ImageHistoryBudget{RequestBytes: 1, RetainNewestImage: true}
		}, want: "requires omit_old_images"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy := deepCopy(t, value)
			test.edit(&copy)
			require.ErrorContains(t, Validate(copy), test.want)
		})
	}
}

func TestValidateImagePolicyExecutionRequiresEncodableImplicitOutput(t *testing.T) {
	tests := []struct {
		name   string
		policy ImagePolicy
	}{
		{name: "maximum side", policy: ImagePolicy{MaxSidePixels: 1}},
		{name: "maximum output", policy: ImagePolicy{MaxOutputBytes: 1}},
		{name: "maximum patches", policy: ImagePolicy{MaxPatches: 1}},
		{name: "white alpha", policy: ImagePolicy{FlattenAlpha: "white"}},
		{name: "black alpha", policy: ImagePolicy{FlattenAlpha: "black"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			test.policy.AcceptedMediaTypes = []string{"image/gif", "image/webp"}
			require.ErrorContains(t, ValidateImagePolicyExecution(&test.policy), "no accepted output media type supported by the core image encoder")
		})
	}

	require.NoError(t, ValidateImagePolicyExecution(&ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/jpeg"},
		MaxSidePixels:      1,
	}))
	require.NoError(t, ValidateImagePolicyExecution(&ImagePolicy{
		AcceptedMediaTypes: []string{"image/gif", "image/jpeg"},
		OutputMediaType:    "image/jpeg",
		MaxSidePixels:      1,
	}))
	require.ErrorContains(t, ValidateImagePolicyExecution(&ImagePolicy{
		AcceptedMediaTypes: []string{"image/webp"},
		OutputMediaType:    "image/webp",
	}), "not supported by the core image encoder")
	require.ErrorContains(t, ValidateImagePolicyExecution(&ImagePolicy{
		AcceptedMediaTypes: []string{"image/png"},
		OutputMediaType:    "image/jpeg",
	}), "not declared in accepted_media_types")
}

//nolint:tparallel // Table subtests intentionally remain serial.
func TestValidateRejectsSemanticConflicts(t *testing.T) {
	t.Parallel()

	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)

	tests := []struct {
		name string
		edit func(*Manifest)
		want string
	}{
		{
			name: "duplicate model",
			edit: func(value *Manifest) { value.Models = append(value.Models, value.Models[0]) },
			want: "is duplicated",
		},
		{
			name: "unknown default model",
			edit: func(value *Manifest) { value.Provider.DefaultLargeModel = "missing" },
			want: "references unknown model",
		},
		{
			name: "unknown config template reference",
			edit: func(value *Manifest) { value.Capabilities.OAuth[0].ClientID.Ref = "missing" },
			want: "unknown configuration property",
		},
		{
			name: "credential audience",
			edit: func(value *Manifest) { value.Capabilities.Credentials[0].Audience = []string{"missing"} },
			want: "references unknown id",
		},
		{
			name: "endpoint credential outside audience",
			edit: func(value *Manifest) { value.Capabilities.Credentials[0].Audience = nil },
			want: "does not declare endpoint",
		},
		{
			name: "oauth scope exceeds credential",
			edit: func(value *Manifest) {
				value.Capabilities.OAuth[0].Scopes = append(value.Capabilities.OAuth[0].Scopes, "admin")
			},
			want: "exceeds credential scope",
		},
		{
			name: "unsupported OAuth revocation",
			edit: func(value *Manifest) {
				value.Capabilities.OAuth[0].RevocationEndpoint = value.Capabilities.OAuth[0].TokenEndpoint
			},
			want: "revocation_endpoint is unsupported by this host",
		},
		{
			name: "wildcard endpoint host",
			edit: func(value *Manifest) { value.Capabilities.Endpoints[0].AllowedHosts = []string{"*.example.invalid"} },
			want: "invalid exact hostname",
		},
		{
			name: "endpoint host",
			edit: func(value *Manifest) {
				value.Capabilities.Endpoints[0].AllowedHosts = []string{"other.example.invalid"}
			},
			want: "host is not allowed",
		},
		{
			name: "non-bijective tool aliases",
			edit: func(value *Manifest) {
				value.Capabilities.ToolCodecs["example-tools"] = ToolCodec{
					Aliases:  []ToolAlias{{Host: "view", Provider: "same"}, {Host: "write", Provider: "same"}},
					Surfaces: []string{"definitions"},
				}
			},
			want: "not bidirectionally unique",
		},
		{
			name: "overlapping tool prefixes",
			edit: func(value *Manifest) {
				value.Capabilities.ToolCodecs["example-tools"] = ToolCodec{
					PrefixAliases: []ToolPrefixAlias{
						{HostPrefix: "mcp_one_", ProviderPrefix: "mcp__one__"},
						{HostPrefix: "mcp_one_nested_", ProviderPrefix: "mcp__two__"},
					},
					Surfaces: []string{"definitions"},
				}
			},
			want: "overlaps another host prefix",
		},
		{
			name: "deferred prefix without tool search",
			edit: func(value *Manifest) {
				value.Capabilities.ToolCodecs["example-tools"] = ToolCodec{
					PrefixAliases: []ToolPrefixAlias{{
						HostPrefix: "mcp_test_", ProviderPrefix: "mcp__test__", DeferLoading: true,
					}},
					Surfaces: []string{"definitions"},
				}
			},
			want: "tool_search is required",
		},
		{
			name: "tool search without deferred prefix",
			edit: func(value *Manifest) {
				value.Capabilities.ToolCodecs["example-tools"] = ToolCodec{
					PrefixAliases: []ToolPrefixAlias{{HostPrefix: "mcp_test_", ProviderPrefix: "mcp__test__"}},
					Surfaces:      []string{"definitions"},
					ToolSearch:    "regex",
				}
			},
			want: "requires a deferred prefix alias",
		},
		{
			name: "invalid instruction selection default",
			edit: func(value *Manifest) { value.Capabilities.Instructions.SelectionDefault = "automatic" },
			want: "selection_default must be crux or native",
		},
		{
			name: "unsafe instruction path",
			edit: func(value *Manifest) { value.Capabilities.Instructions.Profiles["native"] = "../native.txt" },
			want: "safe bundle-relative path",
		},
		{
			name: "invalid hidden skill",
			edit: func(value *Manifest) { value.Capabilities.Instructions.HiddenSkills = []string{"invalid skill"} },
			want: "not a valid skill name",
		},
		{
			name: "duplicate hidden skill",
			edit: func(value *Manifest) { value.Capabilities.Instructions.HiddenSkills = []string{"imagegen", "imagegen"} },
			want: "duplicates",
		},
		{
			name: "unsupported image output format",
			edit: func(value *Manifest) { value.Capabilities.Images.OutputMediaType = "image/webp" },
			want: "not supported by the core image encoder",
		},
		{
			name: "invalid image quality",
			edit: func(value *Manifest) { value.Capabilities.Images.QualitySteps = []int{0} },
			want: "invalid JPEG quality",
		},
		{
			name: "duplicate runtime control",
			edit: func(value *Manifest) {
				value.Capabilities.RuntimeControls = append(value.Capabilities.RuntimeControls, value.Capabilities.RuntimeControls[0])
			},
			want: "is duplicated",
		},
		{
			name: "runtime enum default outside values",
			edit: func(value *Manifest) { value.Capabilities.RuntimeControls[0].Default = "missing" },
			want: "default must be one of values",
		},
		{
			name: "runtime control malformed pointer",
			edit: func(value *Manifest) { value.Capabilities.RuntimeControls[0].RequestPath = "/reasoning/~2effort" },
			want: "not a valid JSON Pointer",
		},
		{
			name: "error status outside HTTP range",
			edit: func(value *Manifest) { value.Capabilities.Errors[0].Statuses = []int{99} },
			want: "invalid HTTP status",
		},
		{
			name: "error malformed pointer",
			edit: func(value *Manifest) { value.Capabilities.Errors[0].CodePointer = "/error/~code" },
			want: "not a valid JSON Pointer",
		},
		{
			name: "error codes without pointer",
			edit: func(value *Manifest) {
				value.Capabilities.Errors[0].Codes = []string{"expired"}
				value.Capabilities.Errors[0].CodePointer = ""
			},
			want: "code_pointer is required when codes are declared",
		},
		{
			name: "error pointer without codes",
			edit: func(value *Manifest) { value.Capabilities.Errors[0].CodePointer = "/error/code" },
			want: "code_pointer has no codes to match",
		},
		{
			name: "empty error code",
			edit: func(value *Manifest) { value.Capabilities.Errors[2].Codes = []string{""} },
			want: "codes[0] must not be empty",
		},
		{
			name: "overlong error code",
			edit: func(value *Manifest) { value.Capabilities.Errors[2].Codes = []string{strings.Repeat("x", 257)} },
			want: "codes[0] exceeds 256 characters",
		},
		{
			name: "overlong error code pointer",
			edit: func(value *Manifest) { value.Capabilities.Errors[2].CodePointer = "/" + strings.Repeat("x", 1024) },
			want: "code_pointer exceeds 1024 characters",
		},
		{
			name: "overlong error message pointer",
			edit: func(value *Manifest) { value.Capabilities.Errors[0].MessagePointer = "/" + strings.Repeat("x", 1024) },
			want: "message_pointer exceeds 1024 characters",
		},
		{
			name: "catch-all error shadows later mapping",
			edit: func(value *Manifest) {
				value.Capabilities.Errors = []ErrorMapping{
					{Class: "unknown"},
					{Class: "server", Statuses: []int{500}},
				}
			},
			want: "errors[1] is unreachable because errors[0] matches every error it could match",
		},
		{
			name: "error status superset shadows later mapping",
			edit: func(value *Manifest) {
				value.Capabilities.Errors = []ErrorMapping{
					{Class: "server", Statuses: []int{500, 503}},
					{Class: "capacity", Statuses: []int{503}},
				}
			},
			want: "errors[1] is unreachable because errors[0] matches every error it could match",
		},
		{
			name: "error code superset shadows later mapping",
			edit: func(value *Manifest) {
				value.Capabilities.Errors = []ErrorMapping{
					{Class: "server", Codes: []string{"temporary", "busy"}, CodePointer: "/error/code"},
					{Class: "capacity", Codes: []string{"busy"}, CodePointer: "/error/code"},
				}
			},
			want: "errors[1] is unreachable because errors[0] matches every error it could match",
		},
		{
			name: "multiple inference operations",
			edit: func(value *Manifest) {
				value.Capabilities.Operations = append(value.Capabilities.Operations, value.Capabilities.Operations[0])
				value.Capabilities.Operations[1].ID = "second-inference"
			},
			want: "exactly one inference operation",
		},
		{
			name: "stream mappings without event type pointer",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish"}}}
			},
			want: "event_type_pointer is required with event mappings",
		},
		{
			name: "stream condition unknown config template reference",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish", Condition: &Predicate{Operation: "equals", Path: "/type", Value: &Template{Kind: "config", Ref: "missing"}}}}}
			},
			want: "unknown configuration property",
		},
		{
			name: "stream malformed event type pointer",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/~bad", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish"}}}
			},
			want: "event_type_pointer is not a valid non-root JSON Pointer",
		},
		{
			name: "stream malformed field pointer",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish", Fields: map[string]string{"finish_reason": "/~bad"}}}}
			},
			want: "fields.finish_reason is not a valid non-root JSON Pointer",
		},
		{
			name: "stream metadata field without namespace",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish", Fields: map[string]string{"metadata.trace": "/trace"}}}}
			},
			want: "metadata field requires metadata_namespace",
		},
		{
			name: "duplicate stream event mapping",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "delta", Event: "text-delta"}, {Source: "delta", Event: "text-delta"}, {Source: "done", Event: "finish"}}}
			},
			want: `duplicates source "delta" event "text-delta"`,
		},
		{
			name: "multiple stream terminal mappings",
			edit: func(value *Manifest) {
				value.Capabilities.Operations[0].Streaming = &StreamingPolicy{EventSource: "sse-data-json", EventTypePointer: "/type", MaxEventBytes: 1024, Mappings: []EventMapping{{Source: "done", Event: "finish"}, {Source: "done", Event: "error"}}}
			},
			want: `source "done" declares more than one terminal mapping`,
		},
		{
			name: "usage malformed pointer",
			edit: func(value *Manifest) { value.Capabilities.Usage.Mappings[0].Pointer = "/usage/~bad" },
			want: "usage.mappings[0].pointer is not a valid non-root JSON Pointer",
		},
		{
			name: "usage duplicate target",
			edit: func(value *Manifest) {
				value.Capabilities.Usage.Mappings = append(value.Capabilities.Usage.Mappings, value.Capabilities.Usage.Mappings[0])
			},
			want: "duplicates target",
		},
		{
			name: "usage subtract wrong target",
			edit: func(value *Manifest) { value.Capabilities.Usage.Mappings[1].Operation = "subtract-cache-read" },
			want: "subtract-cache-read requires input_tokens",
		},
		{
			name: "duplicate metadata namespace",
			edit: func(value *Manifest) {
				contract := value.Capabilities.Metadata[0]
				contract.Scope = "message"
				value.Capabilities.Metadata = append(value.Capabilities.Metadata, contract)
			},
			want: "duplicates namespace",
		},
		{
			name: "protocol continuation mismatch",
			edit: func(value *Manifest) { value.Capabilities.Operations[0].Protocol = "gemini-generate-content" },
			want: "previous-response requires openai-responses",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			copy := deepCopy(t, value)
			test.edit(&copy)
			require.ErrorContains(t, Validate(copy), test.want)
		})
	}
}

func TestValidateAcceptsReachableErrorMappings(t *testing.T) {
	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)

	for _, mappings := range [][]ErrorMapping{
		{
			{Class: "server", Statuses: []int{500}},
			{Class: "capacity", Statuses: []int{503}},
		},
		{
			{Class: "server", Codes: []string{"temporary"}, CodePointer: "/error/code"},
			{Class: "capacity", Codes: []string{"busy"}, CodePointer: "/error/code"},
		},
		{
			{Class: "server", Codes: []string{"temporary"}, CodePointer: "/error/code"},
			{Class: "capacity", Codes: []string{"temporary"}, CodePointer: "/failure/code"},
		},
	} {
		copy := deepCopy(t, value)
		copy.Capabilities.Errors = mappings
		require.NoError(t, Validate(copy))
	}
}

func TestValidateErrorMappingDiagnosticsDoNotExposeCodes(t *testing.T) {
	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)
	privateCode := strings.Repeat("private-code-", 32)
	value.Capabilities.Errors[2].Codes = []string{privateCode}

	err = Validate(value)
	require.ErrorContains(t, err, "codes[0] exceeds 256 characters")
	require.NotContains(t, err.Error(), privateCode)
}

func TestValidateEventMappingsAllowsMutuallyExclusiveConditions(t *testing.T) {
	mappings := []EventMapping{
		{Source: "item", Event: "text-delta", Condition: &Predicate{Operation: "equals", Path: "/kind", Value: &Template{Kind: "literal", Value: "text"}}},
		{Source: "item", Event: "text-delta", Condition: &Predicate{Operation: "equals", Path: "/kind", Value: &Template{Kind: "literal", Value: "alternate"}}},
		{Source: "done", Event: "finish"},
	}
	require.NoError(t, ValidateEventMappings(mappings))
}

func TestValidateMetadataValueBoundsPrivateDiagnostics(t *testing.T) {
	const namespace = "synthetic.meta"
	schemas, err := CompileMetadataContracts([]MetadataContract{{
		Namespace: namespace,
		Schema: map[string]any{
			"$schema": "https://json-schema.org/draft/2020-12/schema",
			"type":    "object",
			"properties": map[string]any{
				"mode": map[string]any{"type": "string", "enum": []any{"allowed"}},
			},
		},
	}})
	require.NoError(t, err)
	privateValue := strings.Repeat("private-value-", 400)
	err = ValidateMetadataValue(namespace, schemas[namespace], map[string]any{"mode": privateValue})
	require.ErrorContains(t, err, "schema validation failed")
	require.NotContains(t, err.Error(), privateValue)
	require.LessOrEqual(t, len(err.Error()), 4096)
}

func TestValidateEventMappingsComparesHugeNumericConditionsExactly(t *testing.T) {
	condition := func(value json.Number) *Predicate {
		return &Predicate{Operation: "equals", Path: "/value", Value: &Template{Kind: "literal", Value: value}}
	}
	overlapping := []EventMapping{
		{Source: "item", Event: "text-delta", Condition: condition(json.Number("1e1000000000"))},
		{Source: "item", Event: "text-delta", Condition: condition(json.Number("10e999999999"))},
	}
	require.ErrorContains(t, ValidateEventMappings(overlapping), `duplicates source "item" event "text-delta"`)

	disjoint := []EventMapping{
		{Source: "item", Event: "text-delta", Condition: condition(json.Number("1e1000000000"))},
		{Source: "item", Event: "text-delta", Condition: condition(json.Number("2e1000000000"))},
	}
	require.NoError(t, ValidateEventMappings(disjoint))
}

func TestValidateAllowsDeferredPrefixToolCodec(t *testing.T) {
	t.Parallel()

	base := readRepoFile(t, "docs", "provider-plugins", "examples", "responses-oauth.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)
	value.Capabilities.ToolCodecs["example-tools"] = ToolCodec{
		PrefixAliases: []ToolPrefixAlias{{
			HostPrefix:        "mcp_test_",
			ProviderPrefix:    "mcp__test__",
			DeferLoading:      true,
			OmitEmptyRequired: true,
		}},
		Parameters: []ParameterMap{{
			Tool: "mcp_test_open", Host: "target", Provider: "url",
		}},
		Surfaces:   []string{"definitions", "history-calls", "stream-events"},
		ToolSearch: "regex",
	}
	require.NoError(t, Validate(value))
}

func TestSemverAndHostCompatibilityValidation(t *testing.T) {
	t.Parallel()

	base := readRepoFile(t, "docs", "provider-plugins", "examples", "minimal.plugin", "manifest.json")
	value, err := DecodeStrict(base)
	require.NoError(t, err)
	value.Version = "v1.0.0"
	require.ErrorContains(t, Validate(value), "canonical SemVer")

	value, err = DecodeStrict(base)
	require.NoError(t, err)
	value.Compatibility.HostAPI = VersionBounds{Min: 2, Max: 1}
	require.ErrorContains(t, Validate(value), "ascending range")
}

func TestSafeBundlePathBoundaries(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"instructions/native.txt",
		"a/b/c.txt",
	} {
		require.True(t, safeBundlePath(value), value)
	}
	for _, value := range []string{
		"", ".", "..", "../escape", "a/../escape", "a/./b", "a//b",
		"/absolute", `C:\\escape`, `\\server\\share`, "name:stream", "nul\x00byte",
	} {
		require.False(t, safeBundlePath(value), value)
	}
}

func normalizeSchemaLineEndings(data []byte) string {
	return string(bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")))
}

func readRepoFile(t *testing.T, parts ...string) []byte {
	t.Helper()
	all := append([]string{"..", "..", ".."}, parts...)
	data, err := os.ReadFile(filepath.Join(all...))
	require.NoError(t, err)
	return data
}

func deepCopy(t *testing.T, value Manifest) Manifest {
	t.Helper()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	var copy Manifest
	require.NoError(t, json.Unmarshal(data, &copy))
	return copy
}
