package providerregistry

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/foundation/providers/openai"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/providertransport"
	declarativetransport "github.com/example-git/crux/internal/providertransport/declarative"
	"github.com/stretchr/testify/require"
)

func TestRuntimeControlBindingRequiresActualDeclaration(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Type: "enum", Values: []string{"base", "selected"}, Default: "base", Scope: "model", RequestPath: "/vendor/mode"}
	for _, construction := range []Construction{ConstructionGenericJSON, ConstructionOpenAIResponses, ConstructionGeminiContent, ConstructionGeminiInteraction} {
		t.Run(string(construction), func(t *testing.T) {
			registration := Registration{Construction: construction, RuntimeControls: []manifest.RuntimeControl{control}}
			binding, err := ResolveRuntimeControlBinding(registration, control)
			require.NoError(t, err)
			require.Equal(t, RuntimeControlModelOption, binding.Kind)
			require.Empty(t, binding.HostOption)
			require.Nil(t, binding.GlobalOverride)
			changed := control
			changed.RequestPath = "/different"
			_, err = ResolveRuntimeControlBinding(registration, changed)
			require.Error(t, err)
			changed = control
			changed.Scope = "provider"
			registration.RuntimeControls = []manifest.RuntimeControl{changed}
			binding, err = ResolveRuntimeControlBinding(registration, changed)
			if construction == ConstructionOpenAIResponses {
				require.NoError(t, err)
				require.Equal(t, RuntimeControlProviderOption, binding.Kind)
			} else {
				require.Error(t, err)
			}
		})
	}
	for _, conflict := range []string{"/vendor/mode", "/vendor", "/vendor/mode/nested"} {
		registration := Registration{Construction: ConstructionOpenAIResponses, RuntimeControls: []manifest.RuntimeControl{
			control,
			{ID: "another", Type: "string", Scope: "model", RequestPath: conflict},
		}}
		_, err := ResolveRuntimeControlBinding(registration, control)
		require.ErrorContains(t, err, "overlapping request paths")
	}
	registration := Registration{Construction: ConstructionCodex, RuntimeControls: codexRuntimeControls()}
	for _, declared := range registration.RuntimeControls {
		binding, err := ResolveRuntimeControlBinding(registration, declared)
		require.NoError(t, err)
		require.Equal(t, RuntimeControlHostOption, binding.Kind)
		require.NotEmpty(t, binding.HostOption)
	}
	registration.Construction = ConstructionOpenAIResponses
	_, err := ResolveRuntimeControlBinding(registration, registration.RuntimeControls[0])
	require.ErrorContains(t, err, "reserved host option")
	registration.Construction = ConstructionCodex
	registration.Manifest = &manifest.Manifest{}
	_, err = ResolveRuntimeControlBinding(registration, registration.RuntimeControls[0])
	require.ErrorContains(t, err, "runtime delegation")
}

func TestRuntimeControlDescriptorBindsMeaningAndBinding(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Type: "string", Scope: "model", RequestPath: "/mode", Default: "base"}
	binding := RuntimeControlBinding{Kind: RuntimeControlModelOption}
	digest := RuntimeControlDescriptorDigest(control, binding)
	require.Len(t, digest, 64)
	require.Equal(t, digest, RuntimeControlDescriptorDigest(control, binding))
	for _, change := range []func(*manifest.RuntimeControl){
		func(c *manifest.RuntimeControl) { c.ID = "other" },
		func(c *manifest.RuntimeControl) { c.Type = "boolean" },
		func(c *manifest.RuntimeControl) { c.Scope = "provider" },
		func(c *manifest.RuntimeControl) { c.RequestPath = "/other" },
		func(c *manifest.RuntimeControl) { c.Default = "changed" },
	} {
		changed := control
		change(&changed)
		require.NotEqual(t, digest, RuntimeControlDescriptorDigest(changed, binding))
	}
	override := HostAnalysisEffort
	binding.GlobalOverride = &override
	require.NotEqual(t, digest, RuntimeControlDescriptorDigest(control, binding))
}

func TestRuntimeControlDescriptorSurvivesSurfaceClone(t *testing.T) {
	for _, value := range []json.Number{"1.0", "9007199254740993", "1.25e2"} {
		control := manifest.RuntimeControl{ID: "vendor.limit", Type: "number", Scope: "model", RequestPath: "/limit", Default: value}
		binding := RuntimeControlBinding{Kind: RuntimeControlModelOption}
		digest := RuntimeControlDescriptorDigest(control, binding)
		surface := Surface{RuntimeControls: []RuntimeControlSurface{{RuntimeControl: control, Available: true, Binding: &binding, DescriptorDigest: digest}}}
		copied := surface.Clone().RuntimeControls[0]
		require.Equal(t, digest, RuntimeControlDescriptorDigest(copied.RuntimeControl, *copied.Binding))
		require.Equal(t, value, copied.Default)
	}
}

func TestRuntimeControlBindingDescribesConditionalFallback(t *testing.T) {
	control := manifest.RuntimeControl{ID: "vendor.mode", Type: "string", Scope: "model", RequestPath: "/vendor/mode", Default: "base"}
	for _, test := range []struct {
		operation manifest.JSONOperation
		dependent bool
	}{
		{manifest.JSONOperation{Operation: "set", Path: "/unrelated"}, false},
		{manifest.JSONOperation{Operation: "set", Path: "/vendor/mode"}, true},
		{manifest.JSONOperation{Operation: "delete", Path: "/vendor"}, true},
		{manifest.JSONOperation{Operation: "drop-keys", Path: ""}, true},
		{manifest.JSONOperation{Operation: "move", Path: "/other", From: "/vendor/mode"}, true},
		{manifest.JSONOperation{Operation: "rename-key", Path: "/other", From: "/vendor/mode"}, true},
		{manifest.JSONOperation{Operation: "copy", Path: "/other", From: "/vendor/mode"}, false},
	} {
		registration := Registration{Construction: ConstructionGenericJSON, RuntimeControls: []manifest.RuntimeControl{control}, Operation: &providertransport.Operation{RequestTransform: &manifest.JSONPipeline{Operations: []manifest.JSONOperation{test.operation}}}}
		binding, err := ResolveRuntimeControlBinding(registration, control)
		require.NoError(t, err)
		require.Equal(t, test.dependent, binding.FallbackMode == "before-request-transform")
	}
	registration := Registration{Construction: ConstructionOpenAIResponses, RuntimeControls: []manifest.RuntimeControl{control}}
	binding, err := ResolveRuntimeControlBinding(registration, control)
	require.NoError(t, err)
	require.Equal(t, "if-absent", binding.FallbackMode)
	digest := RuntimeControlDescriptorDigest(control, binding)
	binding.FallbackMode = ""
	require.NotEqual(t, digest, RuntimeControlDescriptorDigest(control, binding))
}

func TestRuntimeControlValuesPreservePrimitiveMeaning(t *testing.T) {
	for _, test := range []struct {
		kind, raw string
		valid     bool
	}{
		{"boolean", "false", true},
		{"boolean", "true", true},
		{"boolean", `"false"`, false},
		{"integer", "0", true},
		{"integer", "9007199254740993", true},
		{"integer", "1.0", true},
		{"integer", "0.5", false},
		{"number", "0.5", true},
		{"number", "-2.25e3", true},
		{"number", "1e1000000000", false},
		{"number", "1e-1000000000", false},
		{"number", "1e309", false},
		{"number", "1e-1000", false},
		{"number", "NaN", false},
		{"string", `""`, true},
		{"string", `"literal \\ud800"`, true},
		{"string", `"\ud83d\ude00"`, true},
		{"string", `"\ufffd"`, true},
		{"string", `"\ud800"`, false},
		{"string", `"\udc00"`, false},
		{"string", `"\ud800x"`, false},
		{"string", `"\ud800\ud800"`, false},
		{"enum", `"selected"`, true},
		{"enum", `"unknown"`, false},
		{"string", `null`, false},
		{"number", `null`, false},
		{"boolean", `null`, false},
		{"string", `{}`, false},
		{"string", `[]`, false},
		{"integer", `1 2`, false},
	} {
		t.Run(test.kind+"/"+test.raw, func(t *testing.T) {
			control := manifest.RuntimeControl{ID: "fixture", Type: test.kind, Values: []string{"base", "selected"}}
			err := ValidateRuntimeControlValue(control, json.RawMessage(test.raw))
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
	control := manifest.RuntimeControl{ID: "fixture", Type: "string"}
	require.Error(t, ValidateRuntimeControlValue(control, json.RawMessage{'"', 0xff, '"'}))
	require.Error(t, ValidateRuntimeControlValue(control, json.RawMessage(`"`+strings.Repeat("x", MaxRuntimeControlValueBytes)+`"`)))
}

func TestRuntimeControlOptionResolutionConsumesAliasesInDeclarationOrder(t *testing.T) {
	controls := []manifest.RuntimeControl{
		{ID: "first.mode", RequestPath: "/first"},
		{ID: "second.mode", RequestPath: "/second"},
		{ID: "third.mode", RequestPath: "/third"},
	}
	input := map[string]any{"first.mode": "canonical", "mode": "alias", "unrelated": false}
	before := maps.Clone(input)
	remaining, byPath, keys := ResolveRuntimeControlOptions(controls, input)
	require.Equal(t, map[string]any{"/first": "canonical", "/second": "alias"}, byPath)
	require.Equal(t, map[string]string{"first.mode": "first.mode", "second.mode": "mode"}, keys)
	require.Equal(t, map[string]any{"unrelated": false}, remaining)
	require.Equal(t, before, input)
	remaining, _, _ = ResolveRuntimeControlOptions(controls, nil)
	require.NotNil(t, remaining)
}

func TestRuntimeControlGlobalOverrideHasParityAndPreservesInputs(t *testing.T) {
	controls := []manifest.RuntimeControl{
		{ID: "vendor.mode", RequestPath: "/vendor/mode"},
		{ID: "reasoning_effort", RequestPath: "/reasoning/effort"},
	}
	for _, construction := range []Construction{ConstructionGenericJSON, ConstructionOpenAIResponses} {
		t.Run(string(construction), func(t *testing.T) {
			reasoning := declarativeReasoningCapability("fixture", construction, controls)
			initial, err := reasoning.Options("model", "", false, map[string]any{"vendor.mode": "selected", "reasoning_effort": "high"})
			require.NoError(t, err)
			read := func(options fantasy.ProviderOptions) map[string]any {
				if construction == ConstructionOpenAIResponses {
					return options[openai.Name].(*openai.ResponsesProviderOptions).RuntimeControls
				}
				return options["fixture"].(*declarativetransport.Options).Controls
			}
			before := maps.Clone(read(initial))
			runtime := declarativeRuntimeCapability("fixture", construction, controls)
			updated := runtime.Apply(RuntimeValues{AnalysisEffort: "low"}, initial)
			require.Equal(t, "low", read(updated)["/reasoning/effort"])
			require.Equal(t, "selected", read(updated)["/vendor/mode"])
			require.Equal(t, before, read(initial))
			read(updated)["/vendor/mode"] = "changed later"
			require.Equal(t, "selected", read(initial)["/vendor/mode"])
			unchanged := runtime.Apply(RuntimeValues{}, initial)
			require.Equal(t, before, read(unchanged))
		})
	}
}
