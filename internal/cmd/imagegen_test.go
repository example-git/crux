package cmd

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/example-git/crux/internal/imagegen"
	"github.com/example-git/crux/internal/providerplugin"
	"github.com/spf13/cobra"
)

func TestImagegenOutputCollisionsMakeNoHTTPRequest(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-test")
	var requests atomic.Int64
	installImagegenClientFactory(t, func() *imagegen.Client {
		return &imagegen.Client{HTTPClient: &http.Client{Transport: commandRoundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return commandJSONResponse(`{"data":[]}`), nil
		})}}
	})

	t.Run("single output", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "existing.png")
		if err := os.WriteFile(out, []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd := newImagegenTestCommand(runImagegenGenerate, false)
		cmd.SetArgs([]string{"--prompt", "test", "--out", out})
		if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("error = %v, want collision", err)
		}
		got, err := os.ReadFile(out)
		if err != nil || string(got) != "keep" {
			t.Fatalf("existing output changed: %q, %v", got, err)
		}
	})

	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestImagegenNumberedOutputsSkipExistingFiles(t *testing.T) {
	outputDir := t.TempDir()
	existing := filepath.Join(outputDir, "image_2.png")
	if err := os.WriteFile(existing, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	installImagegenClientFactory(t, func() *imagegen.Client {
		return &imagegen.Client{HTTPClient: &http.Client{Transport: commandRoundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return commandJSONResponse(`{"data":[{"b64_json":"Zmlyc3Q="},{"b64_json":"dGhpcmQ="}]}`), nil
		})}}
	})
	cmd := newImagegenTestCommand(runImagegenGenerate, false)
	cmd.SetArgs([]string{"--prompt", "test", "--out-dir", outputDir, "--n", "2"})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"image_1.png": "first", "image_2.png": "keep", "image_3.png": "third"} {
		data, err := os.ReadFile(filepath.Join(outputDir, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s = %q, %v; want %q", name, data, err, want)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("HTTP requests = %d, want 1", requests.Load())
	}
}

func TestImagegenForceProvidesExplicitOverwriteConsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.png")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	outputs, err := prepareImageOutputs(path, "", 1, true)
	if err != nil {
		t.Fatalf("prepareImageOutputs with force: %v", err)
	}
	defer outputs.abort()
	if err := outputs.write(&imagegen.Response{
		AuthMode: imagegen.AuthAPIKey,
		Data:     []imagegen.ImageData{{B64JSON: "bmV3"}},
	}); err != nil {
		t.Fatalf("write forced output: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new" {
		t.Fatalf("forced output = %q, %v", got, err)
	}
}

func TestImagegenOutputReservationUsesExclusiveCreation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reserved.png")
	outputs, err := prepareImageOutputs(path, "", 1, false)
	if err != nil {
		t.Fatalf("prepareImageOutputs: %v", err)
	}
	defer outputs.abort()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err == nil {
		_ = file.Close()
		t.Fatal("second exclusive create succeeded")
	}
	if !os.IsExist(err) {
		t.Fatalf("second create error = %v, want existence collision", err)
	}
}

func TestImagegenOutputsPreserveSuccessfulVariants(t *testing.T) {
	outputDir := t.TempDir()
	outputs, err := prepareImageOutputs("", outputDir, 3, false)
	if err != nil {
		t.Fatalf("prepareImageOutputs: %v", err)
	}
	defer outputs.abort()

	err = outputs.write(&imagegen.Response{
		AuthMode: imagegen.AuthCodex,
		Data: []imagegen.ImageData{
			{B64JSON: "Zmlyc3Q=", Variant: 1},
			{B64JSON: "dGhpcmQ=", Variant: 3},
		},
		Failures: []imagegen.ImageVariantFailure{{Variant: 2, Error: "variant failed"}},
	})
	if err != nil {
		t.Fatalf("write partial outputs: %v", err)
	}

	for _, expected := range []struct {
		name string
		data string
	}{
		{name: "image_1.png", data: "first"},
		{name: "image_3.png", data: "third"},
	} {
		data, err := os.ReadFile(filepath.Join(outputDir, expected.name))
		if err != nil || string(data) != expected.data {
			t.Fatalf("%s = %q, %v", expected.name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(outputDir, "image_2.png")); !os.IsNotExist(err) {
		t.Fatalf("failed variant placeholder remains: %v", err)
	}
}

func TestDocumentedReferenceImageCommandUsesEditInput(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-test")
	input := filepath.Join(t.TempDir(), "reference.png")
	if err := os.WriteFile(input, []byte("reference-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "derived.png")

	var gotPath, gotPrompt, gotFilename, gotImage string
	installImagegenClientFactory(t, func() *imagegen.Client {
		return &imagegen.Client{HTTPClient: &http.Client{Transport: commandRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotPath = req.URL.Path
			mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
			if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
				t.Fatalf("content type = %q: %v", mediaType, err)
			}
			reader := multipart.NewReader(req.Body, params["boundary"])
			for {
				part, err := reader.NextPart()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("read multipart: %v", err)
				}
				data, _ := io.ReadAll(part)
				switch part.FormName() {
				case "prompt":
					gotPrompt = string(data)
				case "image[]":
					gotFilename = part.FileName()
					gotImage = string(data)
				}
			}
			return commandJSONResponse(`{"data":[{"b64_json":"ZGVyaXZlZA=="}]}`), nil
		})}}
	})

	prompt := "Create a new mug photo; use Image 1 only for lighting and composition"
	cmd := newImagegenTestCommand(runImagegenEdit, true)
	cmd.SetArgs([]string{"--image", input, "--prompt", prompt, "--out", out})
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatalf("documented edit command: %v", err)
	}
	if gotPath != "/edit" || gotPrompt != prompt || gotFilename != "reference.png" || gotImage != "reference-bytes" {
		t.Fatalf("edit request = path %q, prompt %q, file %q, data %q", gotPath, gotPrompt, gotFilename, gotImage)
	}
	data, err := os.ReadFile(out)
	if err != nil || string(data) != "derived" {
		t.Fatalf("output = %q, %v", data, err)
	}
}

func TestImagegenRejectsConflictingOutputFlagsBeforeHTTP(t *testing.T) {
	t.Setenv("AI_CLI_DIR", t.TempDir())
	t.Setenv("OPENAI_API_KEY", "sk-test")
	var requests atomic.Int64
	installImagegenClientFactory(t, func() *imagegen.Client {
		return &imagegen.Client{HTTPClient: &http.Client{Transport: commandRoundTripFunc(func(*http.Request) (*http.Response, error) {
			requests.Add(1)
			return commandJSONResponse(`{"data":[]}`), nil
		})}}
	})

	cmd := newImagegenTestCommand(runImagegenGenerate, false)
	cmd.SetArgs([]string{"--prompt", "test", "--out", "one.png", "--out-dir", "many"})
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("error = %v, want conflicting-output error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func newImagegenTestCommand(run func(*cobra.Command, []string) error, edit bool) *cobra.Command {
	cmd := &cobra.Command{Use: "imagegen-test", RunE: run, SilenceErrors: true, SilenceUsage: true}
	cmd.Flags().String("backend", "auto", "")
	cmd.Flags().String("prompt", "", "")
	cmd.Flags().String("model", "", "")
	cmd.Flags().Int("n", 1, "")
	cmd.Flags().String("quality", "", "")
	cmd.Flags().String("size", "", "")
	cmd.Flags().String("background", "", "")
	cmd.Flags().String("out", "", "")
	cmd.Flags().String("out-dir", "", "")
	cmd.Flags().Bool("force", false, "")
	if edit {
		cmd.Flags().StringArray("image", nil, "")
	}
	return cmd
}

func installImagegenClientFactory(t *testing.T, factory func() *imagegen.Client) {
	t.Helper()
	original := newImagegenRuntime
	newImagegenRuntime = func(cmd *cobra.Command) (*imagegen.PluginRuntime, error) {
		root := t.TempDir()
		manager, err := providerplugin.NewManager(cmd.Context(), providerplugin.DefaultPaths(filepath.Join(root, "data"), filepath.Join(root, "cache")))
		if err != nil {
			return nil, err
		}
		source := t.TempDir()
		data := []byte(`{"plugin_type":"image-provider","manifest_version":1,"id":"synthetic.cli.images","version":"1.0.0","name":"Synthetic CLI","description":"Synthetic CLI image contract","publisher":{"id":"synthetic","name":"Synthetic"},"compatibility":{"host_api":{"min":1,"max":1}},"backend":"synthetic","configuration":{"schema":{"type":"object","additionalProperties":false}},"origins":[{"url":"https://images.example.test"}],"models":[{"id":"model","name":"Model"}],"default_model":"model","options":{"quality":["auto"],"background":["auto"],"sizes":["auto"],"output_extension":".png"},"limits":{"concurrency":1,"variants":10,"input_images":16,"input_bytes":1024,"total_input_bytes":16384,"output_bytes":1024,"response_bytes":16384,"timeout_seconds":30},"variant_mode":"batch","generate":"generate","edit":"edit","workflows":{"generate":{"steps":[{"id":"send","request":{"method":"POST","url":{"literal":"https://images.example.test/generate"},"encoding":"json","body":{"object":{"prompt":{"ref":"/request/prompt"}}},"response":"json","phase":"generation","max_bytes":16384,"timeout_seconds":30}}],"result":{"op":"map","args":[{"ref":"/steps/send/body/data"},{"ref":"/item/b64_json"}]}},"edit":{"steps":[{"id":"send","request":{"method":"POST","url":{"literal":"https://images.example.test/edit"},"encoding":"multipart","body":{"array":[{"object":{"name":{"literal":"prompt"},"value":{"ref":"/request/prompt"}}},{"object":{"name":{"literal":"image[]"},"filename":{"ref":"/inputs/0/filename"},"media_type":{"ref":"/inputs/0/media_type"},"data":{"ref":"/inputs/0/data"}}}]},"response":"json","phase":"generation","max_bytes":16384,"timeout_seconds":30}}],"result":{"op":"map","args":[{"ref":"/steps/send/body/data"},{"ref":"/item/b64_json"}]}}}}`)
		if err := os.WriteFile(filepath.Join(source, "manifest.json"), data, 0o600); err != nil {
			manager.Close()
			return nil, err
		}
		if _, err := manager.Install(cmd.Context(), providerplugin.InstallRequest{Source: source, Trust: true}); err != nil {
			manager.Close()
			return nil, err
		}
		runtime := &imagegen.PluginRuntime{Manager: manager, Client: factory().HTTPClient}
		runtime.Select = func(context.Context) (providerplugin.ImageOwner, error) {
			return manager.CaptureImageOwner("synthetic")
		}
		return runtime, nil
	}
	t.Cleanup(func() { newImagegenRuntime = original })
}

type commandRoundTripFunc func(*http.Request) (*http.Response, error)

func (f commandRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func commandJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
