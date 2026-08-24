package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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
	require.Equal(t, string(generated), string(checkedIn), "run `task schema` to update provider-plugin.schema.json")
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
			name: "unsafe instruction path",
			edit: func(value *Manifest) { value.Capabilities.Instructions.Profiles["native"] = "../native.txt" },
			want: "safe bundle-relative path",
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
			name: "multiple inference operations",
			edit: func(value *Manifest) {
				value.Capabilities.Operations = append(value.Capabilities.Operations, value.Capabilities.Operations[0])
				value.Capabilities.Operations[1].ID = "second-inference"
			},
			want: "exactly one inference operation",
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
