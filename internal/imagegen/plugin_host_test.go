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
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	managedtask "github.com/example-git/crux/internal/task"
	"github.com/stretchr/testify/require"
)

func TestHostImageRuntimeUsesInstalledOwnerAndCapturedEnvironment(t *testing.T) {
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "host"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("FIXTURE_IMAGE_KEY", "captured-image-key")
	cfg := &config.Config{}
	store := config.NewTestStore(cfg)
	runtime, err := NewHostPluginRuntime(t.Context(), store, PluginCredentialBindings{})
	require.NoError(t, err)
	defer runtime.Manager.Close()
	t.Setenv("FIXTURE_IMAGE_KEY", "wrong-process-key")
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "captured-image-key", r.Header.Get("X-Image-Key"))
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, float64(79), body["wire_model"])
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{"images": []string{base64.StdEncoding.EncodeToString(pixels.Bytes())}}))
	}))
	defer server.Close()
	runtime.Client = server.Client()
	literal := func(value any) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "fixture.host.images", Version: "1.0.0", Name: "Fixture images", Description: "Host image contract",
		Publisher: manifest.Publisher{ID: "fixture", Name: "Fixture"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "fixture-host-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Credentials: []manifest.ImageCredential{{ID: "key", Source: "environment", Environment: "FIXTURE_IMAGE_KEY"}},
		Origins:     []manifest.ImageOrigin{{URL: server.URL, Credentials: []string{"key"}}}, Models: []manifest.ImageModel{{ID: "model", Name: "Model"}}, DefaultModel: "model",
		Options: manifest.ImageOptions{Quality: []string{"auto"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:  manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 30}, Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {Steps: []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{Method: "POST", URL: literal(server.URL), Headers: map[string]manifest.ImageValue{"X-Image-Key": {Ref: "/credentials/key"}}, Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{"wire_model": literal(79)}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 5}}}, Result: manifest.ImageValue{Ref: "/steps/send/body/images"}}},
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	_, err = runtime.Manager.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
	require.NoError(t, err)
	owner, err := runtime.Manager.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	cfg.Images = &config.ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}, Providers: map[string]config.ImageProviderConfiguration{owner.Backend: {Owner: owner}}}
	jobs, err := NewJobManagerWithStore("fixture-host", nil, JobManagerOptions{PluginRuntime: runtime})
	require.NoError(t, err)
	defer jobs.StopAll(context.Background())
	request := JobRequest{Mode: ModeGenerate, Prompt: "paper bird", Count: 1, OutputPaths: []string{filepath.Join(root, "placeholder.png")}}
	view, paths, err := jobs.EnqueueNumbered(request, t.TempDir(), "fixture", managedtask.Ownership{ParentSessionID: "parent"})
	require.NoError(t, err)
	_, err = jobs.Output(t.Context(), view.ID, true, 10*time.Second)
	require.NoError(t, err)
	saved, err := os.ReadFile(paths[0])
	require.NoError(t, err)
	require.Equal(t, pixels.Bytes(), saved)
	wrong := owner
	wrong.Digest = strings.Repeat("b", 64)
	cfg.Images = &config.ImageConfiguration{Preferred: []providerplugin.ImageOwner{wrong}, Providers: map[string]config.ImageProviderConfiguration{wrong.Backend: {Owner: wrong}}}
	request.Backend = Backend(owner.Backend)
	_, err = jobs.PrepareRequest(t.Context(), request)
	require.Error(t, err)
	cfg.Images = &config.ImageConfiguration{Preferred: []providerplugin.ImageOwner{owner}}
	_, err = runtime.Manager.SetTrust(t.Context(), owner.PluginID, providerplugin.TrustRequest{Digest: owner.Digest, Trusted: false})
	require.NoError(t, err)
	_, err = jobs.PrepareRequest(t.Context(), request)
	require.Error(t, err)
	require.EqualValues(t, 1, calls.Load())
}
