package manifest

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func imageManifestFixture() ImageManifest {
	return ImageManifest{
		PluginType: PluginTypeImageProvider, ManifestVersion: 1, ID: "example.images", Version: "1.0.0", Name: "Example images", Description: "Synthetic image protocol",
		Publisher: Publisher{ID: "example", Name: "Example"}, Compatibility: Compatibility{HostAPI: VersionBounds{Min: 1, Max: 1}},
		Backend: "example-images", Configuration: Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Origins: []ImageOrigin{{URL: "https://images.example.test"}},
		Models:  []ImageModel{{ID: "example-model", Name: "Example model"}}, DefaultModel: "example-model",
		Options:  ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:   ImageLimits{Concurrency: 1, Variants: 10, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 10},
		Generate: "generate", VariantMode: "individual",
		Workflows: map[string]ImageWorkflow{"generate": {
			Steps:  []ImageStep{{ID: "send", Request: &ImageRequest{Method: "POST", URL: ImageValue{Literal: json.RawMessage(`"https://images.example.test/generate"`)}, Encoding: "json", Body: &ImageValue{Object: map[string]ImageValue{"prompt": {Ref: "/request/prompt"}}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 10}}},
			Result: ImageValue{Ref: "/steps/send/body/images"},
		}},
	}
}

func TestImageManifestStrictContract(t *testing.T) {
	value := imageManifestFixture()
	data, err := json.Marshal(value)
	require.NoError(t, err)
	decoded, err := DecodeImageStrict(data)
	require.NoError(t, err)
	require.Equal(t, value.Backend, decoded.Backend)
	for name, change := range map[string]func(map[string]any){
		"unknown field":     func(v map[string]any) { v["executable"] = "never-print-private-value" },
		"zero timeout":      func(v map[string]any) { v["limits"].(map[string]any)["timeout_seconds"] = 0 },
		"missing publisher": func(v map[string]any) { delete(v, "publisher") },
		"missing workflow":  func(v map[string]any) { v["generate"] = "absent" },
		"unknown operator": func(v map[string]any) {
			v["workflows"].(map[string]any)["generate"].(map[string]any)["result"] = map[string]any{"op": "shell", "args": []any{}}
		},
		"conflicting value": func(v map[string]any) {
			v["workflows"].(map[string]any)["generate"].(map[string]any)["result"] = map[string]any{"ref": "/request/prompt", "literal": "private"}
		},
		"invalid arity": func(v map[string]any) {
			v["workflows"].(map[string]any)["generate"].(map[string]any)["result"] = map[string]any{"op": "equal", "args": []any{}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			var raw map[string]any
			require.NoError(t, json.Unmarshal(data, &raw))
			change(raw)
			invalid, err := json.Marshal(raw)
			require.NoError(t, err)
			_, err = DecodeImageStrict(invalid)
			require.Error(t, err)
			require.NotContains(t, err.Error(), "never-print-private-value")
		})
	}
	_, err = DecodeImageStrict(append(data, []byte(` {}`)...))
	require.Error(t, err)
}

func TestImageManifestRejectsUnexecutableReferences(t *testing.T) {
	for _, ref := range []string{"/steps/send/body", "/steps/missing/body", "/credentials/missing", "/clients/missing/version"} {
		value := imageManifestFixture()
		workflow := value.Workflows["generate"]
		workflow.Steps[0].Request.Body = &ImageValue{Ref: ref}
		data, err := json.Marshal(value)
		require.NoError(t, err)
		_, err = DecodeImageStrict(data)
		require.ErrorContains(t, err, "/workflows/value/ref")
	}
	value := imageManifestFixture()
	for index := range 34 {
		id := fmt.Sprintf("workflow-%d", index)
		step := ImageStep{ID: "next", Call: fmt.Sprintf("workflow-%d", index+1)}
		if index == 33 {
			step = ImageStep{ID: "next", Value: &ImageValue{Literal: json.RawMessage(`[]`)}}
		}
		value.Workflows[id] = ImageWorkflow{Steps: []ImageStep{step}, Result: ImageValue{Ref: "/steps/next"}}
	}
	value.Generate = "workflow-0"
	require.ErrorContains(t, ValidateImage(value), "/workflows/depth")
	for _, media := range []string{"image/*", "image/png; charset=utf-8", "Image/PNG", "invalid"} {
		value := imageManifestFixture()
		value.Workflows["generate"].Steps[0].Request.AcceptedMediaTypes = []string{media}
		require.ErrorContains(t, ValidateImage(value), "accepted_media_types")
	}
}

func TestImageManifestRejectsWorkflowAndFallbackCycles(t *testing.T) {
	value := imageManifestFixture()
	value.Workflows["generate"] = ImageWorkflow{Steps: []ImageStep{{ID: "recurse", Call: "generate"}}, Result: ImageValue{Literal: json.RawMessage(`[]`)}}
	require.ErrorContains(t, ValidateImage(value), "/workflows/cycle")
	value = imageManifestFixture()
	value.Models[0].Fallback = &ImageFallback{Model: value.Models[0].ID, WhenUnavailable: true}
	require.ErrorContains(t, ValidateImage(value), "/models/fallback")
}

func TestImageManifestSchemaDoesNotContainPrivateProtocols(t *testing.T) {
	data, err := ImageSchemaJSON()
	require.NoError(t, err)
	for _, value := range []string{"chatgpt.com", "flow.google.com", "GEM_PIX", "NARWHAL"} {
		require.False(t, strings.Contains(string(data), value))
	}
}
