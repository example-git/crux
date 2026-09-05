package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestInstalledImagePluginRunsJobToFiles(t *testing.T) {
	for _, mode := range []string{"individual", "batch", "batch-malformed"} {
		t.Run(mode, func(t *testing.T) {
			testInstalledImagePluginRunsJobToFiles(t, mode)
		})
	}
}

func testInstalledImagePluginRunsJobToFiles(t *testing.T, variantMode string) {
	t.Helper()
	malformed := variantMode == "batch-malformed"
	if malformed {
		variantMode = "batch"
	}
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	encoded := base64.StdEncoding.EncodeToString(pixels.Bytes())
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, float64(47), body["wire_model"])
		require.Equal(t, "paper bird", body["prompt"])
		if body["variant"] == float64(2) {
			http.Error(w, "synthetic variant failure", 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		images := []string{encoded}
		if malformed {
			images = []string{encoded, "invalid!", encoded}
		}
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"images": images}))
	}))
	defer server.Close()
	root := t.TempDir()
	manager, err := providerplugin.NewManager(t.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
	require.NoError(t, err)
	defer manager.Close()
	runtime := &PluginRuntime{Manager: manager, Client: server.Client()}
	jobs, err := NewJobManagerWithStore("synthetic", nil, JobManagerOptions{PluginRuntime: runtime})
	require.NoError(t, err)
	defer jobs.StopAll(context.Background())
	literal := func(value any) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "synthetic.images", Version: "1.0.0", Name: "Synthetic images", Description: "Installed synthetic image contract",
		Publisher: manifest.Publisher{ID: "synthetic", Name: "Synthetic"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "synthetic-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Origins: []manifest.ImageOrigin{{URL: server.URL}}, Models: []manifest.ImageModel{{ID: "model", Name: "Model", Parameters: map[string]any{"wire": 47}}}, DefaultModel: "model",
		Options: manifest.ImageOptions{AspectRatios: []string{"1:1", "2:1"}, Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto", "1024x64"}, Dimensions: true, DimensionLimits: &manifest.ImageDimensionLimits{Multiple: 16, MaxEdge: 1024, MinPixels: 4096, MaxPixels: 262144, MaxAspect: 2}, OutputExtension: ".png"},
		Limits:  manifest.ImageLimits{Concurrency: 1, Variants: 3, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 30}, Generate: "generate", VariantMode: variantMode,
		Workflows: map[string]manifest.ImageWorkflow{"generate": {
			Steps:  []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{Method: "POST", URL: literal(server.URL), Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{"wire_model": {Ref: "/model/parameters/wire"}, "prompt": {Ref: "/request/prompt"}, "variant": {Ref: "/variant"}}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 5}}},
			Result: manifest.ImageValue{Ref: "/steps/send/body/images"},
		}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	_, err = manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	output := t.TempDir()
	request := JobRequest{Mode: ModeGenerate, Backend: Backend(value.Backend), Prompt: "paper bird", Count: 3, OutputPaths: []string{filepath.Join(output, "placeholder1"), filepath.Join(output, "placeholder2"), filepath.Join(output, "placeholder3")}}
	for _, size := range []string{"1024x64", "65x64", "32x64", "768x768", "1040x64", "064x64", "128x96"} {
		invalid := request
		invalid.Size = size
		_, err := runtime.Execute(t.Context(), owner, invalid, nil)
		require.Error(t, err, size)
		require.Zero(t, calls.Load())
	}
	for _, size := range []string{"auto", "64x64", "128x64", "512x512"} {
		valid := request
		valid.Size = size
		_, _, err := runtime.Prepare(owner, valid)
		require.NoError(t, err, size)
	}
	runtime.Configuration = func(providerplugin.ImageOwner) (map[string]any, error) {
		return map[string]any{"unsupported": "private-configuration"}, nil
	}
	_, err = runtime.Execute(t.Context(), owner, request, nil)
	require.ErrorContains(t, err, "configuration")
	require.NotContains(t, err.Error(), "private-configuration")
	require.Zero(t, calls.Load())
	runtime.Configuration = nil
	expectedCalls := int64(3)
	savedIndexes, missingIndexes := []int{0, 2}, []int{1}
	if variantMode == "batch" {
		response, err := runtime.Execute(t.Context(), owner, request, nil)
		require.NoError(t, err)
		require.Equal(t, 1, response.Data[0].Variant)
		require.Equal(t, 2, response.Failures[0].Variant)
		if malformed {
			require.Len(t, response.Data, 2)
			require.Equal(t, 3, response.Data[1].Variant)
			require.Len(t, response.Failures, 1)
		} else {
			require.Len(t, response.Data, 1)
			require.Len(t, response.Failures, 2)
			require.Equal(t, 3, response.Failures[1].Variant)
			savedIndexes, missingIndexes = []int{0}, []int{1, 2}
		}
		expectedCalls = 2
	}
	view, paths, err := jobs.EnqueueNumbered(request, output, "synthetic image", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	_, err = jobs.Output(t.Context(), view.ID, true, 10*time.Second)
	require.NoError(t, err)
	for _, index := range savedIndexes {
		saved, err := os.ReadFile(paths[index])
		require.NoError(t, err)
		require.Equal(t, pixels.Bytes(), saved)
		decoded, err := png.Decode(bytes.NewReader(saved))
		require.NoError(t, err)
		require.Equal(t, image.Rect(0, 0, 2, 2), decoded.Bounds())
	}
	for _, index := range missingIndexes {
		_, err = os.Stat(paths[index])
		require.ErrorIs(t, err, os.ErrNotExist)
	}
	require.Equal(t, expectedCalls, calls.Load())
	_, err = manager.SetTrust(t.Context(), owner.PluginID, providerplugin.TrustRequest{Digest: owner.Digest, Trusted: false})
	require.NoError(t, err)
	request.Owner = &owner
	_, err = jobs.Enqueue(request, "revoked", managedtask.Ownership{ParentSessionID: "parent"})
	require.Error(t, err)
	require.Equal(t, expectedCalls, calls.Load())
}

func TestImagePluginOwnerSurvivesRecordRoundTrip(t *testing.T) {
	for _, owner := range []providerplugin.ImageOwner{
		{Backend: "synthetic-images", PluginID: "synthetic.images", Version: "1.0.0", Digest: "exact-digest"},
		{Backend: "synthetic-images", PluginID: "synthetic.images"},
	} {
		request := JobRequest{Owner: &owner, Backend: Backend(owner.Backend), Mode: ModeGenerate, Prompt: "paper bird", Count: 1, OutputExtension: ".jpg", OutputPaths: []string{"/synthetic/image.jpg"}}
		job := ImageJob{ID: "synthetic", Request: request}
		data, err := json.Marshal(job.record())
		require.NoError(t, err)
		var record managedtask.Record
		require.NoError(t, json.Unmarshal(data, &record))
		restored := jobRequestFromRecord(record.Image)
		require.Equal(t, request, restored)
		restored.Owner.Version = "changed"
		require.Equal(t, owner.Version, record.Image.PluginVersion)
	}
}
