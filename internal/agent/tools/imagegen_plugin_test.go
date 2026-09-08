package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	fantasy "github.com/example-git/crux/foundation"
	"github.com/example-git/crux/internal/config"
	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/permission"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/example-git/crux/internal/providerplugin/manifest"
	"github.com/example-git/crux/internal/pubsub"
	"github.com/example-git/crux/internal/question"
	"github.com/stretchr/testify/require"
)

func TestImageToolUsesNewlyInstalledCustomProvider(t *testing.T) {
	testImageToolInstalledProvider(t, false, false)
}

func TestImageToolSetupResumesOriginalRequest(t *testing.T) {
	testImageToolInstalledProvider(t, true, false)
}

type imageToolSetupQuestions struct {
	question.Service
	ask func(context.Context, question.Request) ([]question.Answer, error)
}

func (q imageToolSetupQuestions) Ask(ctx context.Context, request question.Request) ([]question.Answer, error) {
	return q.ask(ctx, request)
}

func TestImageToolAuthenticatesBrowserAfterPermission(t *testing.T) {
	testImageToolInstalledProvider(t, false, true)
}

func testImageToolInstalledProvider(t *testing.T, withSetup, withBrowser bool) {
	t.Helper()
	var pixels bytes.Buffer
	require.NoError(t, png.Encode(&pixels, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	var calls atomic.Int64
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if withBrowser {
			cookie, err := r.Cookie("session")
			require.NoError(t, err)
			require.Equal(t, "synthetic-browser-session", cookie.Value)
		}
		var body map[string]any
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "draft", body["quality"])
		require.Equal(t, float64(73), body["model"])
		require.NoError(t, json.NewEncoder(w).Encode([]string{base64.StdEncoding.EncodeToString(pixels.Bytes())}))
	}))
	defer server.Close()
	root := t.TempDir()
	t.Setenv("CRUX_GLOBAL_DATA", filepath.Join(root, "data"))
	t.Setenv("CRUX_CACHE_DIR", filepath.Join(root, "cache"))
	t.Setenv("CRUX_GLOBAL_CONFIG", filepath.Join(root, "config"))
	t.Setenv("CRUX_PROVIDER_PROFILE", "core-only")
	store, err := config.LoadIsolated(root, filepath.Join(root, "workspace"), false, config.SnapshotEnvironment())
	require.NoError(t, err)
	var resolutions atomic.Int64
	bindings := imagegen.PluginCredentialBindings{}
	if withBrowser {
		jar, err := cookiejar.New(nil)
		require.NoError(t, err)
		address, err := url.Parse(server.URL)
		require.NoError(t, err)
		jar.SetCookies(address, []*http.Cookie{{Name: "session", Value: "synthetic-browser-session"}})
		bindings.Browser = func(context.Context, manifest.ImageCredential) (http.CookieJar, string, error) {
			resolutions.Add(1)
			return jar, "synthetic-browser", nil
		}
	}
	runtime, err := imagegen.NewHostPluginRuntime(t.Context(), store, bindings)
	require.NoError(t, err)
	plugins := runtime.Manager
	defer plugins.Close()
	runtime.Client = server.Client()
	var setup *imagegen.SetupService
	var source string
	accept := true
	questions := 0
	if withSetup {
		setup = &imagegen.SetupService{Runtime: runtime, Store: store, Questions: imageToolSetupQuestions{ask: func(ctx context.Context, request question.Request) ([]question.Answer, error) {
			questions++
			require.Equal(t, "parent", request.SessionID)
			require.NoError(t, request.Validate())
			return []question.Answer{{QuestionID: request.Questions[0].ID, Yes: &accept, FillInText: source}}, nil
		}}}
	}
	if withBrowser {
		setup = &imagegen.SetupService{Runtime: runtime, Store: store}
	}
	jobs, err := imagegen.NewJobManagerWithStore(root, nil, imagegen.JobManagerOptions{PluginRuntime: runtime, Setup: setup})
	require.NoError(t, err)
	defer jobs.StopAll(context.Background())
	permissions := &recordingPermissionService{Broker: pubsub.NewBroker[permission.PermissionRequest](), allow: true}
	tool := NewImagegenTool(jobs, permissions, root, true)
	literal := func(value any) manifest.ImageValue {
		data, err := json.Marshal(value)
		require.NoError(t, err)
		return manifest.ImageValue{Literal: data}
	}
	value := manifest.ImageManifest{
		PluginType: manifest.PluginTypeImageProvider, ManifestVersion: 1, ID: "fixture.image-tool", Version: "1.0.0", Name: "Fixture images", Description: "Synthetic tool contract",
		Publisher: manifest.Publisher{ID: "fixture", Name: "Fixture"}, Compatibility: manifest.Compatibility{HostAPI: manifest.VersionBounds{Min: 1, Max: 1}},
		Backend: "fixture-images", Configuration: manifest.Configuration{Schema: map[string]any{"type": "object", "additionalProperties": false}},
		Origins: []manifest.ImageOrigin{{URL: server.URL}}, Models: []manifest.ImageModel{{ID: "model", Name: "Model"}}, DefaultModel: "model",
		Options: manifest.ImageOptions{Quality: []string{"draft"}, Background: []string{"auto"}, Sizes: []string{"auto"}, OutputExtension: ".png"},
		Limits:  manifest.ImageLimits{Concurrency: 1, Variants: 1, InputImages: 1, InputBytes: 1024, TotalInputBytes: 1024, OutputBytes: 1024, ResponseBytes: 4096, TimeoutSeconds: 30}, Generate: "generate", VariantMode: "individual",
		Workflows: map[string]manifest.ImageWorkflow{"generate": {
			Steps:  []manifest.ImageStep{{ID: "send", Request: &manifest.ImageRequest{Method: "POST", URL: literal(server.URL), Encoding: "json", Body: &manifest.ImageValue{Object: map[string]manifest.ImageValue{"quality": {Ref: "/request/quality"}, "model": literal(73)}}, Response: "json", Phase: "generation", MaxBytes: 4096, TimeoutSeconds: 5}}},
			Result: manifest.ImageValue{Ref: "/steps/send/body"},
		}},
	}
	if withBrowser {
		address, err := url.Parse(server.URL)
		require.NoError(t, err)
		value.Credentials = []manifest.ImageCredential{{ID: "browser", Source: "browser", Domains: []string{address.Hostname()}}}
		value.Origins[0].Credentials = []string{"browser"}
	}
	data, err := json.Marshal(value)
	require.NoError(t, err)
	source = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600))
	if !withSetup {
		_, err = plugins.Install(t.Context(), providerplugin.InstallRequest{Source: source, Trust: true})
		require.NoError(t, err)
	}
	if withBrowser {
		owner, err := plugins.CaptureImageOwner(value.Backend)
		require.NoError(t, err)
		require.NoError(t, store.CompareAndSetImageConfiguration(nil, &config.ImageConfiguration{Providers: map[string]config.ImageProviderConfiguration{owner.Backend: {Owner: owner, BrowserProfiles: map[string]string{"browser": strings.Repeat("a", 64)}}}}))
	}
	input, err := json.Marshal(ImagegenParams{Mode: imagegen.ModeGenerate, Backend: "fixture-images", Prompt: "paper bird", Quality: "draft", OutputDirectory: filepath.Join(root, "outputs")})
	require.NoError(t, err)
	ctx := context.WithValue(t.Context(), SessionIDContextKey, "parent")
	if withSetup {
		headless, err := NewImagegenTool(jobs, permissions, root, false).Run(ctx, fantasy.ToolCall{ID: "headless", Name: ImagegenToolName, Input: string(input)})
		require.NoError(t, err)
		require.True(t, headless.IsError)
		require.Zero(t, questions)
		accept = false
		declined, err := tool.Run(ctx, fantasy.ToolCall{ID: "decline", Name: ImagegenToolName, Input: string(input)})
		require.NoError(t, err)
		require.True(t, declined.IsError)
		require.Zero(t, permissions.requestCount)
		require.Zero(t, calls.Load())
		require.Empty(t, plugins.Snapshot().Plugins)
		_, err = os.Stat(filepath.Join(root, "outputs"))
		require.ErrorIs(t, err, os.ErrNotExist)
		accept = true
	}
	if withBrowser {
		permissions.allow = false
		denied, err := tool.Run(ctx, fantasy.ToolCall{ID: "denied", Name: ImagegenToolName, Input: string(input)})
		require.NoError(t, err)
		require.True(t, denied.IsError)
		require.Zero(t, resolutions.Load())
		permissions.allow = true
		permissions.requestCount = 0
	}
	response, err := tool.Run(ctx, fantasy.ToolCall{ID: "first", Name: ImagegenToolName, Input: string(input)})
	require.NoError(t, err)
	require.False(t, response.IsError, response.Content)
	var metadata ImagegenResponseMetadata
	require.NoError(t, json.Unmarshal([]byte(response.Metadata), &metadata))
	_, err = jobs.Output(t.Context(), metadata.TaskID, true, 10*time.Second)
	require.NoError(t, err)
	saved, err := os.ReadFile(metadata.Outputs[0])
	require.NoError(t, err)
	require.Equal(t, pixels.Bytes(), saved)
	if withBrowser {
		require.GreaterOrEqual(t, resolutions.Load(), int64(2))
	}
	owner, err := plugins.CaptureImageOwner(value.Backend)
	require.NoError(t, err)
	_, err = plugins.SetTrust(t.Context(), owner.PluginID, providerplugin.TrustRequest{Digest: owner.Digest, Trusted: false})
	require.NoError(t, err)
	response, err = tool.Run(ctx, fantasy.ToolCall{ID: "revoked", Name: ImagegenToolName, Input: string(input)})
	require.NoError(t, err)
	require.True(t, response.IsError)
	require.Equal(t, 1, permissions.requestCount)
	require.EqualValues(t, 1, calls.Load())
}
