package chat

import (
	"encoding/json"
	"testing"

	"github.com/example-git/crux/internal/imagegen"
	"github.com/stretchr/testify/require"
)

func TestFormatImagegenResultProducesReadableSuccess(t *testing.T) {
	data, err := json.Marshal(imagegen.JobResult{
		Success:  true,
		Mode:     imagegen.ModeGenerate,
		Outputs:  []string{"/workspace/one.png", "/workspace/two.png"},
		AuthMode: "codex",
		Model:    "gpt-image-2",
	})
	require.NoError(t, err)

	formatted, ok := FormatImagegenResult(string(data))
	require.True(t, ok)
	require.Contains(t, formatted, "Generated 2 image variants")
	require.Contains(t, formatted, "/workspace/one.png")
	require.Contains(t, formatted, "Model: gpt-image-2")
	require.Contains(t, formatted, "Account: Codex")
	require.NotContains(t, formatted, "{")
	require.NotContains(t, formatted, `"success"`)
}

func TestFormatImagegenResultReportsPartialVariantSuccess(t *testing.T) {
	data, err := json.Marshal(imagegen.JobResult{
		Success:   true,
		Mode:      imagegen.ModeGenerate,
		Requested: 3,
		Outputs:   []string{"/workspace/image_1.jpg", "/workspace/image_3.jpg"},
		Failures:  []imagegen.ImageVariantFailure{{Variant: 2, Error: "generation rejected"}},
		AuthMode:  "flow",
		Model:     "Nano Banana Pro",
	})
	require.NoError(t, err)

	formatted, ok := FormatImagegenResult(string(data))
	require.True(t, ok)
	require.Contains(t, formatted, "Generated 2 of 3 image variants")
	require.Contains(t, formatted, "/workspace/image_1.jpg")
	require.Contains(t, formatted, "/workspace/image_3.jpg")
	require.Contains(t, formatted, "Variant 2: generation rejected")
	require.Contains(t, formatted, "Account: Google Flow")
	require.NotContains(t, formatted, "Image generation failed")
}

func TestFormatImagegenResultProducesReadableFailure(t *testing.T) {
	data, err := json.Marshal(imagegen.JobResult{
		Success: false,
		Mode:    imagegen.ModeEdit,
		Outputs: []string{"/workspace/edited.png"},
		Error:   "service unavailable",
	})
	require.NoError(t, err)

	formatted, ok := FormatImagegenResult(string(data))
	require.True(t, ok)
	require.Contains(t, formatted, "Image edit failed")
	require.Contains(t, formatted, "service unavailable")
	require.Contains(t, formatted, "Planned output: /workspace/edited.png")
	require.NotContains(t, formatted, "{")
}
